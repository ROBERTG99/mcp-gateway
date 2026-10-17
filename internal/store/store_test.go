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

// TestMigrateOnePendingDedups: migration 00005 adds the one-pending-approval-per-call index on a
// database that already holds duplicate pending rows, keeping the newest and expiring the rest.
func TestMigrateOnePendingDedups(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	for _, cmd := range []string{"up", "down", "down"} { // the downs roll back 00006, then 00005
		if err := store.Migrate(ctx, db.URL, cmd, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := pgx.Connect(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for i, created := range []string{"2026-10-16 12:00:00+00", "2026-10-16 12:01:00+00", "2026-10-16 12:02:00+00"} {
		if _, err := conn.Exec(ctx, `INSERT INTO approvals (id, tenant, agent, server, tool, args_sha256, args_redacted,
			idempotency_key, status, expires_at, created_at, updated_at)
			VALUES ($1, 't_alpha', 'agent-support', 'orders', 'refund_order', repeat('a', 64), '{}', 'k-1', 'pending',
			$2::timestamptz + interval '15 minutes', $2::timestamptz, $2::timestamptz)`,
			"00000000-0000-4000-8000-00000000000"+string(rune('1'+i)), created); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatalf("migrate up over duplicates: %v", err)
	}
	var pending string
	if err := conn.QueryRow(ctx, `SELECT string_agg(id::text, ',') FROM approvals WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != "00000000-0000-4000-8000-000000000003" {
		t.Fatalf("pending after migration = %q, want only the newest", pending)
	}
}

// TestRefundTotalsUseIndex: per-order refund totals (orders.get_order, refund_order) can use the
// (tenant, order_id) index of migration 00006 instead of scanning every refund.
func TestRefundTotalsUseIndex(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN (FORMAT TEXT) SELECT COALESCE(sum(amount_cents), 0) FROM orders.refunds
		WHERE tenant = 't_alpha' AND order_id = 'O-00001'`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if plan := strings.Join(lines, "\n"); !strings.Contains(plan, "orders_refunds_order") {
		t.Fatalf("plan does not use orders_refunds_order: %s", plan)
	}
}
