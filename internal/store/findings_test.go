//go:build integration

package store_test

import (
	"io"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

func TestFindingsStored(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": pgtest.RolePassword}); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.RoleURL("mcpgw_app", pgtest.RolePassword))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := store.NewFindings(pool)
	err = f.RecordFinding(ctx, pipeline.Finding{AuditSeq: 42, Tenant: "t_alpha", Server: "crm", Tool: "get_customer",
		Score: 0.7, Signals: []string{"override_phrase", "tool_reference"}, TS: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.ListFindings(ctx, "t_alpha", 10)
	if err != nil || len(rows) != 1 || *rows[0].AuditSeq != 42 || len(rows[0].Signals) != 2 {
		t.Fatalf("rows = %+v, err = %v", rows, err)
	}
	if other, _ := f.ListFindings(ctx, "t_beta", 10); len(other) != 0 {
		t.Fatal("findings leak across tenants")
	}
}
