//go:build integration

package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": pgtest.RolePassword}); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.RoleURL("mcpgw_app", pgtest.RolePassword))
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
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": pgtest.RolePassword}); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, db.RoleURL("mcpgw_app", pgtest.RolePassword))
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

// TestAuditUnstorableArgsPostgres: the poison payloads that once stopped the audit log (an integer
// beyond 2^53, a NUL that Postgres jsonb rejects) are stored with markers, the chain verifies, and
// a raw NUL reaching the sink is reported as ErrUnstorable rather than a transient failure.
func TestAuditUnstorableArgsPostgres(t *testing.T) {
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
	sink := audit.NewPostgresSink(pool)

	raw := event("t_alpha")
	raw.Seq, raw.ArgsRedacted = 1_000_000, json.RawMessage(`{"note":"a\u0000b"}`)
	if _, err := sink.Append(ctx, []audit.Event{raw}); !errors.Is(err, audit.ErrUnstorable) {
		t.Fatalf("raw NUL append: %v, want ErrUnstorable", err)
	}

	seq := audit.NewPostgresSeq(pool, 16)
	if err := seq.Warm(ctx); err != nil {
		t.Fatal(err)
	}
	w := audit.NewWriter(sink, seq, audit.Options{FlushInterval: time.Hour, EnqueueTimeout: time.Second})
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(rctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	for _, args := range []string{`{"amount_cents":9007199254740993}`, `{"note":"a\u0000b"}`, `{"order_id":"O-1"}`} {
		tk, err := w.Reserve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e := event("t_alpha")
		e.ArgsRedacted = json.RawMessage(args)
		tk.Commit(e)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	res, err := audit.VerifyPostgres(ctx, pool, 0)
	if err != nil || !res.OK || res.Checked != 3 {
		t.Fatalf("verify = %+v, %v", res, err)
	}
	page, err := audit.NewPostgresReader(pool).List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(string(page.Events[2].ArgsRedacted), string(page.Events[1].ArgsRedacted))
	if !strings.Contains(got, "[REDACTED:NUMBER]") || !strings.Contains(got, "[REDACTED:NUL]") {
		t.Fatalf("stored args = %s", got)
	}
}

// TestAuditVerifyDetectsValueTampers: verification hashes the stored row as it is. Values the
// writer would have sanitised (an unrepresentable number in place of a marker, a NaN latency) or
// rounded must fail verification, not be cleaned up on read and pass.
func TestAuditVerifyDetectsValueTampers(t *testing.T) {
	for name, update := range map[string]string{
		"marker replaced by big number": `UPDATE audit_events SET args_redacted = '{"amount_cents": 99999999999999999999}' WHERE chain_pos = 3`,
		"NaN latency":                   `UPDATE audit_events SET latency_ms = 'NaN' WHERE chain_pos = 3`,
		"sub-microsecond latency":       `UPDATE audit_events SET latency_ms = latency_ms + 0.0004 WHERE chain_pos = 3`,
		"args value":                    `UPDATE audit_events SET args_redacted = '{"amount_cents": "[REDACTED:NUMBER]", "x": 1}' WHERE chain_pos = 3`,
	} {
		t.Run(name, func(t *testing.T) {
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
			var events []audit.Event
			for i := range 5 {
				e := event("t_alpha")
				e.Seq = int64(i + 1)
				e.ArgsRedacted = json.RawMessage(`{"amount_cents":9007199254740993}`)
				e.Sanitize()
				events = append(events, e)
			}
			if _, err := audit.NewPostgresSink(pool).Append(ctx, events); err != nil {
				t.Fatal(err)
			}
			if res, err := audit.VerifyPostgres(ctx, pool, 0); err != nil || !res.OK {
				t.Fatalf("clean chain: %+v %v", res, err)
			}
			if _, err := pool.Exec(ctx, `SET session_replication_role = replica; `+update); err != nil {
				t.Fatal(err)
			}
			res, err := audit.VerifyPostgres(ctx, pool, 0)
			if err != nil || res.OK || res.FirstBadSeq != 3 {
				t.Fatalf("tampered chain verified: %+v %v", res, err)
			}
		})
	}
}

// TestAuditAppendAfterLostCommitAck: when a batch committed but its acknowledgement was lost (the
// connection broke or the context ended during COMMIT), the writer retries the same events. The
// retry must succeed without storing them twice, store only the events that are new, and keep the
// chain valid; a different event that reuses a stored seq (a sequence reset) must still fail.
func TestAuditAppendAfterLostCommitAck(t *testing.T) {
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
	sink := audit.NewPostgresSink(pool)
	batch := make([]audit.Event, 5)
	for i := range batch {
		batch[i] = event("t_alpha")
		batch[i].Seq = int64(31 + i)
	}
	first, err := sink.Append(ctx, batch[:3])
	if err != nil {
		t.Fatal(err)
	}
	// The retry carries the committed prefix plus events queued since.
	again, err := sink.Append(ctx, batch)
	if err != nil {
		t.Fatalf("retry after a lost acknowledgement: %v", err)
	}
	if len(again) != len(batch) {
		t.Fatalf("retry returned %d events, want %d", len(again), len(batch))
	}
	for i, e := range again {
		if e.Seq != batch[i].Seq {
			t.Fatalf("retry returned seq %d at %d, want %d", e.Seq, i, batch[i].Seq)
		}
		if i < len(first) && (e.Hash != first[i].Hash || e.ChainPos != first[i].ChainPos) {
			t.Fatalf("seq %d: retry reports hash %s pos %d, stored %s pos %d", e.Seq, e.Hash, e.ChainPos, first[i].Hash, first[i].ChainPos)
		}
	}
	res, err := audit.VerifyPostgres(ctx, pool, 0)
	if err != nil || !res.OK || res.Checked != len(batch) {
		t.Fatalf("verify = %+v, %v", res, err)
	}
	other := event("t_beta") // same seq as a stored event, different content
	other.Seq = 31
	if _, err := sink.Append(ctx, []audit.Event{other}); err == nil {
		t.Fatal("a different event reusing a stored seq was accepted")
	}
}
