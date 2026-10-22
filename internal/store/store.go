// Package store opens Postgres pools and applies the embedded goose migrations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/ROBERTG99/mcp-gateway/migrations"
)

// Open connects a pgx pool and pings it.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// Migrate runs a goose command ("up", "down", "reset", "status") with the embedded migrations.
func Migrate(ctx context.Context, url, command string, out io.Writer) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = db.Close() }()
	// A Postgres advisory lock serialises concurrent migrators (Helm job, compose, replicas).
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithLogger(log.New(out, "migrate: ", 0)), goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	switch command {
	case "up":
		res, err := provider.Up(ctx)
		for _, r := range res {
			_, _ = fmt.Fprintln(out, r)
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
	case "down":
		if _, err := provider.Down(ctx); err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
	case "reset":
		if _, err := provider.DownTo(ctx, 0); err != nil {
			return fmt.Errorf("migrate reset: %w", err)
		}
	case "status":
		status, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range status {
			_, _ = fmt.Fprintf(out, "%-8s %s\n", s.State, s.Source.Path)
		}
	default:
		return fmt.Errorf("unknown migrate command %q (want up, down, reset or status)", command)
	}
	return nil
}

// SetRolePasswords sets the login passwords of the application roles; empty values are skipped.
// ALTER ROLE cannot take bind parameters, so the literal is quoted, which is only safe with
// standard_conforming_strings=on (checked). The statement is run with log_statement disabled
// for this session when permitted (superuser) so the password does not reach the server log.
func SetRolePasswords(ctx context.Context, url string, passwords map[string]string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	pending := false
	for _, pw := range passwords {
		pending = pending || pw != ""
	}
	if !pending {
		return nil
	}
	var scs string
	if err := conn.QueryRow(ctx, "SHOW standard_conforming_strings").Scan(&scs); err != nil {
		return fmt.Errorf("read standard_conforming_strings: %w", err)
	}
	if scs != "on" {
		return fmt.Errorf("standard_conforming_strings must be on to set passwords (got %q)", scs)
	}
	// Needs superuser; without it the statement may be logged when log_statement=all.
	_, _ = conn.Exec(ctx, "SET log_statement = 'none'")
	for role, pw := range passwords {
		if pw == "" {
			continue
		}
		// ALTER ROLE cannot take bind parameters; quote the literal safely.
		lit := "'" + pgEscape(pw) + "'"
		if err := execRetryingConcurrentUpdate(ctx, conn, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" PASSWORD "+lit); err != nil {
			return fmt.Errorf("set password for %s: %w", role, err)
		}
	}
	return nil
}

// alterRoleAttempts bounds the retries of a role update refused as "tuple concurrently updated".
const alterRoleAttempts = 20

// execRetryingConcurrentUpdate runs a role statement, retrying when Postgres refuses it with
// "tuple concurrently updated" (XX000). Roles are cluster-wide catalog rows, so concurrent callers
// (migrate jobs of several replicas, test binaries sharing one server) race on the same pg_authid
// row, and the catalog update does not wait for the other transaction: the loser just fails. Each
// statement sets an absolute value, so running it again is safe.
func execRetryingConcurrentUpdate(ctx context.Context, conn *pgx.Conn, sql string) error {
	for attempt := 1; ; attempt++ {
		_, err := conn.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if err == nil || attempt == alterRoleAttempts || !errors.As(err, &pgErr) ||
			pgErr.Code != "XX000" || pgErr.Message != "tuple concurrently updated" {
			return err
		}
		// Jittered so callers that collided do not retry in lockstep and collide again.
		t := time.NewTimer(time.Millisecond + rand.N(time.Duration(attempt)*10*time.Millisecond)) //nolint:gosec // G404: retry jitter, not a secret
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (retrying after: %w)", ctx.Err(), err)
		case <-t.C:
		}
	}
}

func pgEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}
