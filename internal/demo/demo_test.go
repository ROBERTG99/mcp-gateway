//go:build integration

package demo_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// tamper does what scripts/tamper_audit.sh does on the compose stack: as the superuser, with the
// append-only trigger bypassed, it edits the middle audit row (restore=false) or undoes that edit.
func tamper(ctx context.Context, db *pgxpool.Pool, seq int64, restore bool) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		return 0, err
	}
	if seq == 0 {
		err := tx.QueryRow(ctx, "SELECT seq FROM audit_events ORDER BY chain_pos OFFSET (SELECT count(*) / 2 FROM audit_events) LIMIT 1").Scan(&seq)
		if err != nil {
			return 0, err
		}
	}
	q := "UPDATE audit_events SET agent = agent || '-tampered' WHERE seq = $1"
	if restore {
		q = "UPDATE audit_events SET agent = left(agent, -length('-tampered')) WHERE seq = $1 AND agent LIKE '%-tampered'"
	}
	if _, err := tx.Exec(ctx, q, seq); err != nil {
		return 0, err
	}
	return seq, tx.Commit(ctx)
}

// TestDemoScenarioSteps runs the scripted demo scenario in process, all seven steps: the
// tamper step edits one audit row and verify must fail at exactly that seq. Restoring the row
// makes the chain verify again, and a second run (new keys) passes on the same data.
func TestDemoScenarioSteps(t *testing.T) {
	st := stack.Start(t, stack.Options{UpstreamTimeout: time.Second, Stall: 2 * time.Second})
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var tampered int64
	results, err := demo.Run(ctx, demo.Config{
		GatewayURL: st.MCPURL, AdminURL: st.AdminURL, IssuerURL: st.IssuerURL, WebhookAdminURL: st.SinkURL,
		StallPrefix: "demo-stall-", Out: &out, RunID: "test",
		Tamper: func(ctx context.Context) (int64, error) {
			seq, err := tamper(ctx, st.DB, 0, false)
			tampered = seq
			return seq, err
		},
	})
	t.Log("\n" + out.String())
	if err != nil {
		t.Fatalf("scenario: %v", err)
	}
	if len(results) != 7 {
		t.Fatalf("results = %d, want 7", len(results))
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("step %d (%s) failed: %s", r.Step, r.Name, r.Detail)
		}
	}
	for i := 1; i <= 7; i++ {
		if !strings.Contains(out.String(), fmt.Sprintf("PASS step %d:", i)) {
			t.Fatalf("output lacks PASS step %d", i)
		}
	}
	if !strings.Contains(results[6].Detail, fmt.Sprintf("seq %d", tampered)) {
		t.Fatalf("step 7 detail %q does not name the tampered seq %d", results[6].Detail, tampered)
	}
	if _, err := tamper(ctx, st.DB, tampered, true); err != nil {
		t.Fatal(err)
	}
	// A second run (new keys, no tampering) passes too: the scenario is repeatable on the same data.
	results, err = demo.Run(ctx, demo.Config{GatewayURL: st.MCPURL, AdminURL: st.AdminURL, IssuerURL: st.IssuerURL,
		StallPrefix: "demo-stall-", Out: &out, RunID: "test2"})
	if err != nil {
		t.Fatalf("second run: %v (%+v)", err, results)
	}
}
