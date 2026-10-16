//go:build integration

package audit_test

import (
	"context"
	"fmt"
	"io"
	"slices"
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

// TestAuditQueryPostgres checks that every filter, alone and combined, and the keyset cursor select
// the same events from Postgres as the in-memory reference reader.
func TestAuditQueryPostgres(t *testing.T) {
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
	approval := "4f9c1a52-7d1e-4c55-9a43-0d6a8b2e1f07"
	var events []audit.Event
	for i := range 40 {
		e := event([]string{"t_alpha", "t_beta"}[i%2])
		e.Seq = int64(i + 1)
		e.TS = e.TS.Add(time.Duration(i) * time.Minute)
		e.Agent = fmt.Sprintf("agent-%d", i%3)
		e.Server, e.Tool = []string{"orders", "crm"}[i%4/2], []string{"refund_order", "get_customer"}[i%4/2]
		e.Status = []string{"ok", "denied", "held", "error", "ok"}[i%5]
		if i%7 == 0 {
			e.ApprovalID = &approval
		}
		events = append(events, e)
	}
	pg, mem := audit.NewPostgresReader(pool), audit.MemoryReader{Sink: audit.NewMemorySink()}
	for _, batch := range [][]audit.Event{events[:25], events[25:]} {
		if _, err := audit.NewPostgresSink(pool).Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
		if _, err := mem.Sink.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	t0 := events[0].TS
	filters := map[string]audit.Filter{
		"all":           {},
		"tenant":        {Tenant: "t_beta"},
		"agent":         {Agent: "agent-1"},
		"server.tool":   {Tool: "crm.get_customer"},
		"bare tool":     {Tool: "refund_order"},
		"status":        {Status: "held"},
		"approval":      {ApprovalID: approval},
		"bad approval":  {ApprovalID: "not-a-uuid"},
		"time window":   {From: t0.Add(10 * time.Minute), To: t0.Add(20 * time.Minute)},
		"combined":      {Tenant: "t_alpha", Tool: "orders.refund_order", Status: "ok"},
		"cursor":        {Cursor: "30", Limit: 5},
		"small page":    {Limit: 3},
		"no match":      {Tenant: "t_gamma"},
		"cursor+filter": {Tenant: "t_alpha", Cursor: "21"},
	}
	for name, f := range filters {
		t.Run(name, func(t *testing.T) {
			got, err := pg.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			want, err := mem.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			seqs := func(p audit.Page) []int64 {
				out := []int64{}
				for _, e := range p.Events {
					out = append(out, e.Seq)
				}
				return out
			}
			if !slices.Equal(seqs(got), seqs(want)) || got.NextCursor != want.NextCursor {
				t.Fatalf("postgres %v next=%q, memory %v next=%q", seqs(got), got.NextCursor, seqs(want), want.NextCursor)
			}
		})
	}
	if p, _ := pg.List(ctx, audit.Filter{ApprovalID: approval}); len(p.Events) != 6 {
		t.Fatalf("approval filter matched %d events, want 6", len(p.Events))
	}
}
