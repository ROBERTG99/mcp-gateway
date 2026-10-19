package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewMemoryDedup returns an in-process DedupStore for tests and examples only: entries are never
// evicted and waiters cannot be cancelled. Concurrent calls with the same (tenant, key) are
// serialised so the write runs once.
func NewMemoryDedup() DedupStore { return &memoryDedup{entries: map[string]*memEntry{}} }

type memoryDedup struct {
	mu      sync.Mutex
	entries map[string]*memEntry
}

type memEntry struct {
	mu     sync.Mutex
	done   bool
	tool   string
	hash   string
	result json.RawMessage
}

func (m *memoryDedup) Once(ctx context.Context, tenant, key, tool, argsSHA256 string,
	fn func(ctx context.Context, tx Tx) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	m.mu.Lock()
	e, ok := m.entries[tenant+"\x00"+key]
	if !ok {
		e = &memEntry{}
		m.entries[tenant+"\x00"+key] = e
	}
	m.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		if e.tool != tool || e.hash != argsSHA256 {
			return nil, false, ErrKeyReused
		}
		return e.result, true, nil
	}
	res, err := fn(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	e.done, e.tool, e.hash, e.result = true, tool, argsSHA256, res
	return res, false, nil
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// NewPostgresDedup returns a DedupStore backed by <schema>.mcp_idempotency. The write handler
// runs inside the same transaction that claims the key, so side effect and record commit together.
func NewPostgresDedup(pool *pgxpool.Pool, schema string) (DedupStore, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	return &pgDedup{pool: pool, table: pgx.Identifier{schema, "mcp_idempotency"}.Sanitize(), timeout: 60 * time.Second}, nil
}

type pgDedup struct {
	pool    *pgxpool.Pool
	table   string
	timeout time.Duration // upper bound for one write transaction
}

func (p *pgDedup) Once(ctx context.Context, tenant, key, tool, argsSHA256 string,
	fn func(ctx context.Context, tx Tx) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	// A write that started completes even if the caller disconnects (as real systems do); the
	// caller's retry then replays the stored result instead of writing twice.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("dedup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Claim the key. A concurrent claimant blocks here until the first transaction ends.
	tag, err := tx.Exec(ctx, "INSERT INTO "+p.table+
		" (tenant, key, tool, args_sha256, result) VALUES ($1, $2, $3, $4, 'null') ON CONFLICT DO NOTHING",
		tenant, key, tool, argsSHA256)
	if err != nil {
		return nil, false, fmt.Errorf("dedup: claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		var storedTool, storedHash string
		var result json.RawMessage
		err := p.pool.QueryRow(ctx, "SELECT tool, args_sha256, result FROM "+p.table+" WHERE tenant = $1 AND key = $2",
			tenant, key).Scan(&storedTool, &storedHash, &result)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, errors.New("dedup: claimed key vanished; retry")
		}
		if err != nil {
			return nil, false, fmt.Errorf("dedup: read stored result: %w", err)
		}
		if storedTool != tool || strings.TrimSpace(storedHash) != argsSHA256 {
			return nil, false, ErrKeyReused
		}
		// jsonb keeps numeric precision; formatting may differ from the first response, which is
		// harmless because the result is decoded into the tool's output type before it is sent.
		return result, true, nil
	}
	res, err := fn(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE "+p.table+" SET result = $3 WHERE tenant = $1 AND key = $2", tenant, key, res); err != nil {
		return nil, false, fmt.Errorf("dedup: store result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("dedup: commit: %w", err)
	}
	return res, false, nil
}
