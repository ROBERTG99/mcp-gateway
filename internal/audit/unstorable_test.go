package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
)

// poisonArgs are argument payloads an authenticated agent can send that are not storable as-is:
// integers beyond ±2^53 (not I-JSON), NUL characters (rejected by Postgres jsonb and text),
// duplicate keys and text that is not JSON at all.
var poisonArgs = map[string]string{
	"big integer":   `{"amount_cents":9007199254740993}`,
	"NUL in value":  `{"note":"a\u0000b"}`,
	"NUL in key":    `{"a\u0000b":1}`,
	"nested big":    `{"items":[{"qty":-9007199254740993}]}`,
	"duplicate key": `{"a":1,"a":2}`,
	"not JSON":      `{"a":`,
}

// TestAuditArgsAlwaysStorable: one hostile call must never stop the audit log. Every poison payload
// is stored with a marker in place of the unstorable value, the chain verifies, and a valid
// event committed afterwards is stored too.
func TestAuditArgsAlwaysStorable(t *testing.T) {
	sink := audit.NewMemorySink()
	w := startWriter(t, sink, audit.Options{FlushInterval: time.Hour})
	ctx := t.Context()
	for name, args := range poisonArgs {
		tk, err := w.Reserve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e := event("t_alpha")
		e.Tool = name
		e.ArgsRedacted = json.RawMessage(args)
		tk.Commit(e)
	}
	tk, _ := w.Reserve(ctx)
	odd := event("t_alpha")
	odd.Agent, odd.Tool, odd.LatencyMS = "agent\x00x\xff", "after", math.NaN()
	tk.Commit(odd)
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	events := sink.Events()
	if len(events) != len(poisonArgs)+1 {
		t.Fatalf("stored %d events, want %d", len(events), len(poisonArgs)+1)
	}
	if res := audit.VerifyChain(events, audit.GenesisHash); !res.OK {
		t.Fatalf("verify: %+v", res)
	}
	for _, e := range events {
		args := string(e.ArgsRedacted)
		if strings.Contains(args, `\u0000`) || strings.Contains(args, "9007199254740993") {
			t.Errorf("%s: unstorable value kept: %s", e.Tool, args)
		}
		switch e.Tool {
		case "big integer", "nested big":
			if !strings.Contains(args, "[REDACTED:NUMBER]") {
				t.Errorf("%s: args = %s", e.Tool, args)
			}
		case "NUL in value", "NUL in key":
			if !strings.Contains(args, "[REDACTED:NUL]") {
				t.Errorf("%s: args = %s", e.Tool, args)
			}
		case "not JSON":
			if args != `"[REDACTED:INVALID_JSON]"` {
				t.Errorf("%s: args = %s", e.Tool, args)
			}
		case "after":
			if strings.ContainsRune(e.Agent, 0) || e.LatencyMS != 0 || !strings.Contains(e.Agent, "[REDACTED:NUL]") {
				t.Errorf("string fields not sanitised: %q %v", e.Agent, e.LatencyMS)
			}
		}
	}
}

// pickySink refuses (as unstorable) every batch holding an event whose args are in refuse, the way
// Postgres rejects a whole COPY for one bad row.
type pickySink struct {
	*audit.MemorySink
	mu     sync.Mutex
	refuse map[string]bool
	calls  int
}

func (p *pickySink) Append(ctx context.Context, events []audit.Event) ([]audit.Event, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	for _, e := range events {
		if p.refuse[string(e.ArgsRedacted)] {
			return nil, fmt.Errorf("%w: event %d refused", audit.ErrUnstorable, e.Seq)
		}
	}
	return p.MemorySink.Append(ctx, events)
}

// TestAuditWriterIsolatesUnstorableEvent: when the sink refuses a batch as unstorable, the writer
// stores the good events around the offender, replaces the offender by a placeholder that keeps
// its seq and status, and keeps flushing; nothing is retried forever.
func TestAuditWriterIsolatesUnstorableEvent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		refusePlaceholder bool
	}{{"placeholder stored", false}, {"placeholder refused too", true}} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &pickySink{MemorySink: audit.NewMemorySink(), refuse: map[string]bool{`{"poison":true}`: true}}
			if tc.refusePlaceholder {
				sink.refuse[`"[REDACTED:UNSTORABLE]"`] = true
			}
			w := startWriter(t, sink, audit.Options{FlushInterval: time.Hour, Batch: 64})
			ctx := t.Context()
			var poisonSeq int64
			for i := range 100 {
				tk, err := w.Reserve(ctx)
				if err != nil {
					t.Fatal(err)
				}
				e := event("t_alpha")
				if i == 41 {
					e.ArgsRedacted, e.Status, poisonSeq = json.RawMessage(`{"poison":true}`), "denied", tk.Seq()
				}
				tk.Commit(e)
			}
			if err := w.Flush(ctx); err != nil {
				t.Fatalf("flush: %v", err)
			}
			events := sink.Events()
			want := 100
			if tc.refusePlaceholder {
				want = 99 // dropped after the placeholder was refused as well
			}
			if len(events) != want || w.Depth() != 0 {
				t.Fatalf("stored %d events (depth %d), want %d", len(events), w.Depth(), want)
			}
			if res := audit.VerifyChain(events, audit.GenesisHash); !res.OK {
				t.Fatalf("verify: %+v", res)
			}
			var found bool
			for _, e := range events {
				if e.Seq == poisonSeq {
					found = true
					if e.Status != "denied" || string(e.ArgsRedacted) != `"[REDACTED:UNSTORABLE]"` {
						t.Fatalf("placeholder = %+v", e)
					}
				}
			}
			if found == tc.refusePlaceholder {
				t.Fatalf("placeholder present = %v", found)
			}
			if sink.calls > 30 {
				t.Fatalf("%d sink calls for 100 events: the writer should bisect, not retry one by one", sink.calls)
			}
			// The writer keeps working afterwards.
			tk, _ := w.Reserve(ctx)
			tk.Commit(event("t_alpha"))
			if err := w.Flush(ctx); err != nil || len(sink.Events()) != want+1 {
				t.Fatalf("after poison: %v, %d events", err, len(sink.Events()))
			}
		})
	}
}
