//go:build integration

package approvals_test

import (
	"io"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals/approvalstest"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

func TestPostgresStore(t *testing.T) {
	approvalstest.Run(t, func(t *testing.T) approvals.Store {
		db := pgtest.New(t)
		if err := store.Migrate(t.Context(), db.URL, "up", io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := store.SetRolePasswords(t.Context(), db.URL, map[string]string{"mcpgw_app": pgtest.RolePassword}); err != nil {
			t.Fatal(err)
		}
		pool, err := store.Open(t.Context(), db.RoleURL("mcpgw_app", pgtest.RolePassword))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return approvals.NewPostgres(pool)
	})
}
