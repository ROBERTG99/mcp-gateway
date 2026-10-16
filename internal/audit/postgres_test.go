//go:build integration

package audit_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

// Two writers (two replicas) append 1,000 events concurrently to one Postgres chain.
func TestAuditChainAppendPostgres(t *testing.T) {
	ctx := t.Context()
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": "pw"}); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.RoleURL("mcpgw_app", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var writers []*audit.Writer
	var runs sync.WaitGroup
	rctx, cancel := context.WithCancel(context.Background())
	for range 2 {
		seq := audit.NewPostgresSeq(pool, 128) // small blocks force several refills
		if err := seq.Warm(ctx); err != nil {
			t.Fatal(err)
		}
		w := audit.NewWriter(audit.NewPostgresSink(pool), seq, audit.Options{FlushInterval: 5 * time.Millisecond, Batch: 100,
			EnqueueTimeout: 2 * time.Second}) // a 1,000-call burst; latency is TestAuditEnqueueLatency
		writers = append(writers, w)
		runs.Go(func() {
			if err := w.Run(rctx); err != nil {
				t.Errorf("run: %v", err)
			}
		})
	}
	var wg sync.WaitGroup
	for i := range 1000 {
		wg.Go(func() {
			tk, err := writers[i%2].Reserve(context.Background())
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			tk.Commit(event("t_alpha"))
		})
	}
	wg.Wait()
	cancel()
	runs.Wait()
	res, err := audit.VerifyPostgres(ctx, pool, 0)
	if err != nil || !res.OK || res.Checked != 1000 {
		t.Fatalf("verify = %+v, %v", res, err)
	}
	var distinct int
	if err := pool.QueryRow(ctx, "SELECT count(DISTINCT seq) FROM audit_events").Scan(&distinct); err != nil || distinct != 1000 {
		t.Fatalf("distinct seqs = %d (%v)", distinct, err)
	}
}
