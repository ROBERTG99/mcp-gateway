//go:build integration

package upstreams_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
)

func seeded(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := datagen.Seed(ctx, pool, datagen.Generate(datagen.DefaultSeed)); err != nil {
		t.Fatal(err)
	}
	return pool
}

func session(t *testing.T, pool *pgxpool.Pool, name string) *mcp.ClientSession {
	t.Helper()
	srv, err := upstreams.New(name, pool)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "gw", Version: "v0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool, tenant, key string, args any) map[string]any {
	t.Helper()
	meta := mcp.Meta{"mcpgw": map[string]any{"tenant": tenant, "agent": "agent-support", "request_id": "r"}}
	if key != "" {
		meta["idempotency_key"] = key
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error %v", tool, res.Content[0].(*mcp.TextContent).Text)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestUpstreamToolsListed(t *testing.T) {
	pool := seeded(t)
	want := map[string][]string{
		"ledger": {"get_balance", "issue_credit_note", "record_promise_to_pay"},
		"crm":    {"add_note", "get_customer"},
		"orders": {"get_order", "refund_order"},
	}
	total := 0
	for name, tools := range want {
		res, err := session(t, pool, name).ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Tools) != len(tools) {
			t.Fatalf("%s tools = %d, want %d", name, len(res.Tools), len(tools))
		}
		for i, tl := range res.Tools {
			if tl.Name != tools[i] {
				t.Errorf("%s tool %d = %s, want %s", name, i, tl.Name, tools[i])
			}
		}
		total += len(res.Tools)
	}
	if total != 7 {
		t.Fatalf("total tools = %d, want 7", total)
	}
}

func TestLedgerRecordPromise(t *testing.T) {
	pool := seeded(t)
	cs := session(t, pool, "ledger")
	args := map[string]any{"customer_id": "C-0007", "amount_cents": 12500, "currency": "USD",
		"due_date": "2026-11-15", "note": "agreed on call"}
	first := call(t, cs, "record_promise_to_pay", "t_alpha", "promise-key-0001", args)
	second := call(t, cs, "record_promise_to_pay", "t_alpha", "promise-key-0001", args)
	if first["promise_id"] == nil || first["promise_id"] != second["promise_id"] {
		t.Fatalf("promise ids = %v / %v", first["promise_id"], second["promise_id"])
	}
	var rows int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM ledger.promises WHERE tenant = 't_alpha'").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("promise rows = %d (%v), want 1", rows, err)
	}
	bal := call(t, cs, "get_balance", "t_alpha", "", map[string]any{"customer_id": "C-0007"})
	if bal["currency"] != "USD" || bal["balance_cents"] == nil {
		t.Fatalf("balance = %v", bal)
	}
}

func TestUpstreamTenantIsolation(t *testing.T) {
	pool := seeded(t)
	crm := session(t, pool, "crm")
	alpha := call(t, crm, "get_customer", "t_alpha", "", map[string]any{"customer_id": "C-0042"})
	beta := call(t, crm, "get_customer", "t_beta", "", map[string]any{"customer_id": "C-0042"})
	if alpha["email"] == beta["email"] {
		t.Fatalf("same customer data across tenants: %v", alpha["email"])
	}
	call(t, crm, "add_note", "t_beta", "note-key-000001", map[string]any{"customer_id": "C-0042", "text": "beta only"})
	after := call(t, crm, "get_customer", "t_alpha", "", map[string]any{"customer_id": "C-0042"})
	for _, n := range after["notes"].([]any) {
		if n.(map[string]any)["body"] == "beta only" {
			t.Fatal("t_alpha sees a t_beta note")
		}
	}
	orders := session(t, pool, "orders")
	// O-00001 exists for every tenant with different amounts; a refund in t_beta never touches t_alpha.
	ao := call(t, orders, "get_order", "t_alpha", "", map[string]any{"order_id": "O-00001"})
	call(t, orders, "refund_order", "t_beta", "refund-key-0001", map[string]any{"order_id": "O-00001", "amount_cents": 100, "reason": "test"})
	ao2 := call(t, orders, "get_order", "t_alpha", "", map[string]any{"order_id": "O-00001"})
	if ao["refunded_cents"] != ao2["refunded_cents"] {
		t.Fatalf("t_alpha order changed by a t_beta refund: %v -> %v", ao, ao2)
	}
	ledger := session(t, pool, "ledger")
	res, err := ledger.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_balance", Arguments: map[string]any{"customer_id": "C-9999"},
		Meta: mcp.Meta{"mcpgw": map[string]any{"tenant": "t_alpha"}}})
	if err != nil || !res.IsError {
		t.Fatalf("unknown customer: err=%v res=%+v, want tool error", err, res)
	}
}

func TestRefundValidation(t *testing.T) {
	pool := seeded(t)
	orders := session(t, pool, "orders")
	o := call(t, orders, "get_order", "t_alpha", "", map[string]any{"order_id": "O-00002"})
	amount := o["amount_cents"].(float64)
	res, err := orders.CallTool(t.Context(), &mcp.CallToolParams{Name: "refund_order",
		Arguments: map[string]any{"order_id": "O-00002", "amount_cents": amount + 1, "reason": "too much"},
		Meta:      mcp.Meta{"idempotency_key": "refund-key-over", "mcpgw": map[string]any{"tenant": "t_alpha"}}})
	if err != nil || !res.IsError {
		t.Fatalf("over-refund: err=%v res=%+v, want tool error", err, res)
	}
}

// Concurrent refunds of one order under distinct keys never exceed the order amount.
func TestConcurrentRefundsNeverExceedOrder(t *testing.T) {
	pool := seeded(t)
	orders := session(t, pool, "orders")
	o := call(t, orders, "get_order", "t_alpha", "", map[string]any{"order_id": "O-00003"})
	amount := int64(o["amount_cents"].(float64))
	part := amount/3 + 1 // three parts fit, the fourth would not; eight requests race
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			_, _ = orders.CallTool(context.Background(), &mcp.CallToolParams{Name: "refund_order",
				Arguments: map[string]any{"order_id": "O-00003", "amount_cents": part, "reason": "race"},
				Meta: mcp.Meta{"idempotency_key": fmt.Sprintf("race-key-%04d", i),
					"mcpgw": map[string]any{"tenant": "t_alpha"}}})
		})
	}
	wg.Wait()
	var total int64
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(sum(amount_cents), 0) FROM orders.refunds
		WHERE tenant = 't_alpha' AND order_id = 'O-00003'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total > amount || total != 2*part {
		t.Fatalf("refunded %d of %d with parts of %d, want exactly %d", total, amount, part, 2*part)
	}
}

// Writes in one tenant cannot reach another tenant's records, and rejected refunds leave no row.
func TestUpstreamWriteTenantIsolation(t *testing.T) {
	pool := seeded(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO orders.orders VALUES ('t_alpha', 'O-ALPHA-ONLY', 'C-0001', 1000, 'USD', 'paid', now())`); err != nil {
		t.Fatal(err)
	}
	orders := session(t, pool, "orders")
	res, err := orders.CallTool(t.Context(), &mcp.CallToolParams{Name: "refund_order",
		Arguments: map[string]any{"order_id": "O-ALPHA-ONLY", "amount_cents": 10, "reason": "x"},
		Meta:      mcp.Meta{"idempotency_key": "cross-tenant-1", "mcpgw": map[string]any{"tenant": "t_beta"}}})
	if err != nil || !res.IsError {
		t.Fatalf("cross-tenant refund: err=%v res=%+v, want not-found tool error", err, res)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM orders.refunds WHERE order_id = 'O-ALPHA-ONLY'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refund rows = %d (%v), want 0", rows, err)
	}
}
