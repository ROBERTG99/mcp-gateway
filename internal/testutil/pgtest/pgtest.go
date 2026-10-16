// Package pgtest gives each integration test its own fresh Postgres database. It uses the compose
// service when MCPGW_TEST_USE_COMPOSE=1 (MCPGW_TEST_DATABASE_URL), otherwise one
// pgvector/pgvector:pg16 testcontainer shared by the test binary.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// shared is the per-test-binary container. A package variable is the only way to share one
// container across the tests of a package without a TestMain in every package.
var shared struct {
	once sync.Once
	url  string
	err  error
}

func baseURL(ctx context.Context) (string, error) {
	shared.once.Do(func() {
		if os.Getenv("MCPGW_TEST_USE_COMPOSE") == "1" {
			shared.url = os.Getenv("MCPGW_TEST_DATABASE_URL")
			if shared.url == "" {
				shared.url = "postgres://postgres:postgres@localhost:8220/postgres?sslmode=disable"
			}
			return
		}
		c, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg16",
			tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"),
			tcpostgres.BasicWaitStrategies())
		if err != nil {
			shared.err = fmt.Errorf("start postgres container: %w", err)
			return
		}
		shared.url, shared.err = c.ConnectionString(ctx, "sslmode=disable")
	})
	return shared.url, shared.err
}

// DB is a fresh database for one test.
type DB struct {
	// URL connects as the superuser to the test database.
	URL string
	// Name is the database name.
	Name string
}

// RoleURL returns a URL for the test database as role with the given password.
func (d DB) RoleURL(role, password string) string {
	u, _ := url.Parse(d.URL)
	u.User = url.UserPassword(role, password)
	return u.String()
}

// New creates an empty database and drops it when the test ends.
func New(t testing.TB) DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	base, err := baseURL(ctx)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		conn, err := pgx.Connect(cctx, base)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(cctx) }()
		_, _ = conn.Exec(cctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	return DB{URL: u.String(), Name: name}
}
