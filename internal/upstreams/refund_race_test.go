//go:build integration

package upstreams

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

// Deterministic interleaving: B's refund statement starts while A holds the order lock with an
// uncommitted refund; after A commits, B must see A's refund and reject the over-refund.
func TestInsertRefundSerialisesOnOrderLock(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO orders.orders VALUES ('t_alpha', 'O-RACE', 'C-1', 100, 'USD', 'paid', now())`); err != nil {
		t.Fatal(err)
	}
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := InsertRefund(ctx, txA, "t_alpha", "key-a-0001", "O-RACE", 80, "a", &refundOut{}); err != nil {
		t.Fatal(err)
	}
	errB := make(chan error, 1)
	go func() {
		txB, err := pool.Begin(context.Background())
		if err != nil {
			errB <- err
			return
		}
		defer func() { _ = txB.Rollback(context.Background()) }()
		err = InsertRefund(context.Background(), txB, "t_alpha", "key-b-0001", "O-RACE", 80, "b", &refundOut{})
		if err == nil {
			err = txB.Commit(context.Background())
			if err == nil {
				errB <- nil
				return
			}
		}
		errB <- err
	}()
	// Wait until B is blocked on A's row lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting)
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B never blocked on the order lock")
		}
		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // polling a database condition with a deadline
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errB; err == nil {
		t.Fatal("second refund of 80 on an order of 100 was accepted")
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT sum(amount_cents) FROM orders.refunds WHERE order_id = 'O-RACE'`).Scan(&total); err != nil || total != 80 {
		t.Fatalf("refunded total = %d (%v), want 80", total, err)
	}
}
