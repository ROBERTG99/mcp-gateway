//go:build integration

package store_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

const insertEvent = `INSERT INTO audit_events (seq, chain_pos, ts, tenant, agent, server, tool, args_redacted,
  args_sha256, decision, status, latency_ms, prev_hash, hash)
  VALUES (1, 1, now(), 't_alpha', 'agent-support', 'crm', 'get_customer', '{}', repeat('a', 64), 'allow', 'ok', 1.5,
  repeat('0', 64), repeat('b', 64))`

func TestAuditAppendOnly(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": "app-pw"}); err != nil {
		t.Fatal(err)
	}
	app, err := pgx.Connect(ctx, db.RoleURL("mcpgw_app", "app-pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	if _, err := app.Exec(ctx, insertEvent); err != nil {
		t.Fatalf("app role INSERT: %v", err)
	}
	var n int
	if err := app.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&n); err != nil || n != 1 {
		t.Fatalf("app role SELECT: n=%d err=%v", n, err)
	}
	for _, stmt := range []string{
		"UPDATE audit_events SET tenant = 't_beta'",
		"DELETE FROM audit_events",
		"TRUNCATE audit_events",
	} {
		if _, err := app.Exec(ctx, stmt); err == nil {
			t.Errorf("app role %q succeeded, want rejection", stmt)
		}
	}
	// Even the owner is stopped by the trigger.
	owner, err := pgx.Connect(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	for _, stmt := range []string{"UPDATE audit_events SET tenant = 't_beta'", "DELETE FROM audit_events", "TRUNCATE audit_events"} {
		_, err := owner.Exec(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("owner %q: err = %v, want append-only rejection", stmt, err)
		}
	}
}

func TestMigrateUpDown(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	for _, cmd := range []string{"up", "reset", "up", "status"} {
		if err := store.Migrate(ctx, db.URL, cmd, io.Discard); err != nil {
			t.Fatalf("migrate %s: %v", cmd, err)
		}
	}
	if err := store.Migrate(ctx, db.URL, "sideways", io.Discard); err == nil {
		t.Fatal("unknown command accepted")
	}
}
