package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

type denyRefundsOver struct{ limit float64 }

func (d denyRefundsOver) Evaluate(_ context.Context, in policy.Input) (policy.Decision, error) {
	if m, ok := in.Args.(map[string]any); ok {
		if amt, _ := m["amount_cents"].(float64); amt > d.limit {
			return policy.Decision{Reasons: []string{"refund_over_limit"}}, nil
		}
	}
	return policy.Decision{Allow: true, Reasons: []string{"scope_granted"}}, nil
}

func auditedPipeline(t *testing.T, up *fakeUpstream) (*pipeline.Pipeline, *audit.MemorySink, *audit.Writer) {
	t.Helper()
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{FlushInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	c := clock.Real{}
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: denyRefundsOver{limit: 100000}, Idempotency: idempotency.NewMemory(c),
		Audit: w, Redactor: redact.New(),
		Limiter: ratelimit.NewMemory(map[string]config.Tenant{"t_alpha": {RatePerSecond: 0.001, Burst: 6, DailyBudget: 100}}, c)},
		pipeline.Options{})
	return p, sink, w
}

func TestEveryOutcomeAuditedOnce(t *testing.T) {
	up := &fakeUpstream{}
	p, sink, w := auditedPipeline(t, up)
	card := map[string]any{"order_id": "O-1", "amount_cents": 5000, "reason": "card 4111 1111 1111 1111"}
	steps := []struct {
		args       any
		key        string
		wantStatus string
		wantDec    string
		wantCode   int64
	}{
		{card, "audit-key-0001", "ok", "allow", 0},
		{card, "audit-key-0001", "replayed", "allow", 0},
		{map[string]any{"order_id": "O-1", "amount_cents": 6000, "reason": "x"}, "audit-key-0001", "rejected", "allow", mcperr.CodeIdempotencyConflict},
		{map[string]any{"order_id": "O-1", "amount_cents": 500000, "reason": "x"}, "audit-key-0002", "denied", "deny", mcperr.CodePolicyDenied},
		{card, "", "rejected", "none", mcperr.CodeIdempotencyKeyRequired}, // validated before the policy stage
		{card, "audit-key-0003", "ok", "allow", 0},
		{card, "audit-key-0004", "rejected", "none", mcperr.CodeRateLimited}, // 7th call: burst 6 exhausted
	}
	var seqs []int64
	for i, s := range steps {
		meta := mcp.Meta{}
		if s.key != "" {
			meta["idempotency_key"] = s.key
		}
		res, err := call(p, refundTool, s.args, meta)
		if s.wantCode != 0 {
			if code(t, err) != s.wantCode {
				t.Fatalf("step %d: %v, want %d", i, err, s.wantCode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		seqs = append(seqs, gwMeta(res)["audit_seq"].(int64))
	}
	if err := w.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := sink.Events()
	if len(events) != len(steps) {
		t.Fatalf("audit events = %d, want %d (one per outcome)", len(events), len(steps))
	}
	bySeq := map[int64]audit.Event{}
	for _, e := range events {
		bySeq[e.Seq] = e
	}
	for i, e := range events {
		_ = i
		if e.Tenant != "t_alpha" || e.Server != "orders" || e.Tool != "refund_order" || len(e.ArgsSHA256) != 64 || e.LatencyMS < 0 {
			t.Fatalf("event %+v", e)
		}
		if strings.Contains(string(e.ArgsRedacted), "4111") {
			t.Fatalf("raw card number in audit: %s", e.ArgsRedacted)
		}
	}
	statuses := map[string]int{}
	for _, e := range events {
		statuses[e.Status+"/"+e.Decision]++
	}
	want := map[string]int{"ok/allow": 2, "replayed/allow": 1, "rejected/allow": 1, "denied/deny": 1, "rejected/none": 2}
	for k, v := range want {
		if statuses[k] != v {
			t.Fatalf("status/decision counts = %v, want %v", statuses, want)
		}
	}
	if e := bySeq[seqs[0]]; e.IdempotencyKey == nil || *e.IdempotencyKey != "audit-key-0001" || !strings.Contains(string(e.ArgsRedacted), "[REDACTED:CARD]") {
		t.Fatalf("first event = %+v", e)
	}
	// args_sha256 is computed on the unredacted canonical args.
	raw, _ := json.Marshal(card)
	if bySeq[seqs[0]].ArgsSHA256 == "" || string(raw) == string(bySeq[seqs[0]].ArgsRedacted) {
		t.Fatal("args hash or redaction missing")
	}
	if res := audit.VerifyChain(events, audit.GenesisHash); !res.OK {
		t.Fatalf("chain: %+v", res)
	}
}

func TestFailClosedAuditSaturated(t *testing.T) {
	up := &fakeUpstream{}
	stalled := &stallSink{}
	w := audit.NewWriter(stalled, audit.NewMemorySeq(), audit.Options{Buffer: 1, FlushInterval: time.Hour, EnqueueTimeout: time.Millisecond})
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{}, Idempotency: idempotency.NewMemory(clock.Real{}), Audit: w}, pipeline.Options{})
	if _, err := call(p, refundTool, refundArgs, mcp.Meta{"idempotency_key": "sat-key-0001"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(p, refundTool, refundArgs, mcp.Meta{"idempotency_key": "sat-key-0002"})
	if code(t, err) != mcperr.CodeAuditBackpressure {
		t.Fatalf("err = %v, want -32048", err)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d: a call ran without an audit slot", up.calls.Load())
	}
}

type stallSink struct{}

func (stallSink) Append(context.Context, []audit.Event) ([]audit.Event, error) {
	return nil, errors.New("database unavailable")
}
