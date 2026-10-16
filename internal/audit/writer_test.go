package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

func event(tenant string) audit.Event {
	return audit.Event{TS: time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC), Tenant: tenant, Agent: "agent-support",
		Server: "orders", Tool: "refund_order", ArgsRedacted: json.RawMessage(`{"order_id":"O-1","amount_cents":5000}`),
		ArgsSHA256: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", Decision: "allow", Status: "ok", LatencyMS: 3.25}
}

func startWriter(t *testing.T, sink audit.Sink, opts audit.Options) *audit.Writer {
	t.Helper()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("writer run: %v", err)
		}
	})
	return w
}

func TestAuditChainAppend(t *testing.T) {
	sink := audit.NewMemorySink()
	w := startWriter(t, sink, audit.Options{FlushInterval: time.Millisecond, Batch: 50})
	var wg sync.WaitGroup
	seqs := make(chan int64, 1000)
	for i := range 1000 {
		wg.Go(func() {
			tk, err := w.Reserve(context.Background())
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			seqs <- tk.Seq()
			tk.Commit(event([]string{"t_alpha", "t_beta"}[i%2]))
		})
	}
	wg.Wait()
	close(seqs)
	if err := w.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := sink.Events()
	if len(events) != 1000 {
		t.Fatalf("stored %d events, want 1000", len(events))
	}
	if res := audit.VerifyChain(events, audit.GenesisHash); !res.OK || res.Checked != 1000 {
		t.Fatalf("verify: %+v", res)
	}
	if events[0].PrevHash != audit.GenesisHash {
		t.Fatalf("genesis prev_hash = %q", events[0].PrevHash)
	}
	var got []int64
	for s := range seqs {
		got = append(got, s)
	}
	slices.Sort(got)
	for i := range got {
		if got[i] != int64(i+1) {
			t.Fatalf("seqs not unique 1..1000: %v...", got[:10])
		}
	}
}

func TestAuditVerifyDetectsMutation(t *testing.T) {
	sink := audit.NewMemorySink()
	w := startWriter(t, sink, audit.Options{FlushInterval: time.Millisecond})
	for range 20 {
		tk, _ := w.Reserve(context.Background())
		tk.Commit(event("t_alpha"))
	}
	_ = w.Flush(t.Context())
	events := sink.Events()
	tampered := slices.Clone(events)
	tampered[7].Tool = "issue_credit_note"
	res := audit.VerifyChain(tampered, audit.GenesisHash)
	if res.OK || res.FirstBadSeq != tampered[7].Seq {
		t.Fatalf("verify tampered: %+v, want first bad seq %d", res, tampered[7].Seq)
	}
	// --from-seq: starting after the tampered row (anchored on its successor's prev_hash) passes.
	if res := audit.VerifyChain(tampered[8:], ""); !res.OK {
		t.Fatalf("verify from seq: %+v", res)
	}
}

func TestAuditEnqueueLatency(t *testing.T) {
	sink := audit.NewMemorySink()
	w := startWriter(t, sink, audit.Options{Buffer: 10000, FlushInterval: 50 * time.Millisecond, Batch: 500})
	var worst time.Duration
	durations := make([]time.Duration, 0, 5000)
	for range 5000 {
		start := time.Now()
		tk, err := w.Reserve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		tk.Commit(event("t_alpha"))
		d := time.Since(start)
		durations = append(durations, d)
		worst = max(worst, d)
	}
	slices.Sort(durations)
	p99 := durations[len(durations)*99/100]
	if p99 > time.Millisecond {
		t.Fatalf("enqueue p99 = %s, want <= 1ms (worst %s)", p99, worst)
	}
	t.Logf("enqueue p99 = %s, worst = %s", p99, worst)
}

// A stalled sink fills the buffer; further reservations fail fast with ErrBackpressure.
func TestAuditBackpressure(t *testing.T) {
	sink := &blockingSink{release: make(chan struct{})}
	w := startWriter(t, sink, audit.Options{Buffer: 10, FlushInterval: time.Millisecond, Batch: 5, EnqueueTimeout: time.Millisecond})
	defer close(sink.release)
	accepted := 0
	for range 30 {
		tk, err := w.Reserve(context.Background())
		if errors.Is(err, audit.ErrBackpressure) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		tk.Commit(event("t_alpha"))
		accepted++
	}
	if accepted != 10 {
		t.Fatalf("accepted %d events with a stalled sink and buffer 10", accepted)
	}
	if d := w.Depth(); d != 10 {
		t.Fatalf("depth = %d", d)
	}
}

// Shutdown drains every committed event.
func TestAuditDrainOnShutdown(t *testing.T) {
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{FlushInterval: time.Hour, Batch: 10000, Clock: clock.Real{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	for range 250 {
		tk, _ := w.Reserve(context.Background())
		tk.Commit(event("t_alpha"))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(sink.Events()); n != 250 {
		t.Fatalf("drained %d of 250 events", n)
	}
}

type blockingSink struct{ release chan struct{} }

func (b *blockingSink) Append(ctx context.Context, events []audit.Event) ([]audit.Event, error) {
	select {
	case <-b.release:
		return audit.NewMemorySink().Append(ctx, events)
	case <-ctx.Done():
		return nil, errors.New("sink stalled")
	}
}
