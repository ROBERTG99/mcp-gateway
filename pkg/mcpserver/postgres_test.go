//go:build integration

package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

func TestPostgresDedupConcurrent(t *testing.T) {
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
	dedup, err := mcpserver.NewPostgresDedup(pool, "orders")
	if err != nil {
		t.Fatal(err)
	}
	refund := func(ctx context.Context, tx mcpserver.Tx) (json.RawMessage, error) {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO orders.refunds (tenant, order_id, amount_cents, currency, reason, idempotency_key)
			VALUES ('t_alpha', 'O-00001', 5000, 'USD', 'test', 'refund-key-1') RETURNING id`).Scan(&id)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]int64{"refund_id": id})
	}
	var wg sync.WaitGroup
	results := make([]string, 12)
	for i := range results {
		wg.Go(func() {
			res, _, err := dedup.Once(context.Background(), "t_alpha", "refund-key-1", "refund_order", strings.Repeat("a", 64), refund)
			if err != nil {
				t.Errorf("once: %v", err)
				return
			}
			results[i] = string(res)
		})
	}
	wg.Wait()
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM orders.refunds").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("refund rows = %d (err %v), want 1", rows, err)
	}
	for _, r := range results {
		var a, b map[string]any
		if json.Unmarshal([]byte(r), &a) != nil || json.Unmarshal([]byte(results[0]), &b) != nil || a["refund_id"] != b["refund_id"] {
			t.Fatalf("results differ: %v", results)
		}
	}
	if _, _, err := dedup.Once(ctx, "t_alpha", "refund-key-1", "refund_order", strings.Repeat("b", 64), refund); !errors.Is(err, mcpserver.ErrKeyReused) {
		t.Fatalf("reuse with other args: %v", err)
	}
	if _, err := mcpserver.NewPostgresDedup(pool, "orders; drop table x"); err == nil {
		t.Fatal("invalid schema accepted")
	}
}
