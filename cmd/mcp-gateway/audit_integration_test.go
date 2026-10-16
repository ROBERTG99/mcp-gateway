//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

func TestAuditVerifyDetectsTamper(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": "pw"}); err != nil {
		t.Fatal(err)
	}
	appURL := db.RoleURL("mcpgw_app", "pw")
	pool, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	seq := audit.NewPostgresSeq(pool, 1024)
	w := audit.NewWriter(audit.NewPostgresSink(pool), seq, audit.Options{FlushInterval: 5 * time.Millisecond, EnqueueTimeout: time.Second})
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(rctx) }()
	for i := range 1000 {
		tk, err := w.Reserve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tk.Commit(audit.Event{TS: time.Now(), Tenant: "t_alpha", Agent: "agent-support", Server: "crm", Tool: "get_customer",
			ArgsRedacted: json.RawMessage(fmt.Sprintf(`{"customer_id":"C-%04d"}`, i)), ArgsSHA256: strings.Repeat("c", 64),
			Decision: "allow", Status: "ok", LatencyMS: 1.5})
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPGW_DATABASE_URL", appURL)
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"audit", "verify"}, &out, &errb); code != 0 {
		t.Fatalf("verify clean chain: exit %d: %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "1000 events") {
		t.Fatalf("output = %q", out.String())
	}
	// Tamper with one row as the superuser, bypassing the append-only trigger.
	conn, err := pgx.Connect(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var tampered int64
	err = conn.QueryRow(ctx, `SELECT seq FROM audit_events WHERE chain_pos = 500`).Scan(&tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE audit_events SET tool = 'issue_credit_note' WHERE seq = $1`, tampered); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(ctx, []string{"audit", "verify"}, &out, &errb); code != 1 {
		t.Fatalf("verify tampered chain: exit %d, want 1: %s", code, out.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("seq %d", tampered)) {
		t.Fatalf("output %q does not name seq %d", out.String(), tampered)
	}
	// --from-seq after the tampered row passes again.
	var after int64
	_ = conn.QueryRow(ctx, `SELECT seq FROM audit_events WHERE chain_pos = 501`).Scan(&after)
	out.Reset()
	if code := run(ctx, []string{"audit", "verify", "--from-seq", fmt.Sprint(after)}, &out, &errb); code != 0 {
		t.Fatalf("verify --from-seq %d: exit %d: %s", after, code, out.String())
	}
	// Deleting a row is detected as well.
	if _, err := conn.Exec(ctx, `DELETE FROM audit_events WHERE chain_pos = 700`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(ctx, []string{"audit", "verify", "--from-seq", fmt.Sprint(after)}, &out, &errb); code != 1 {
		t.Fatalf("verify after delete: exit %d: %s", code, out.String())
	}
}
