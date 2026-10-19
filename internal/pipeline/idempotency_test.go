package pipeline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
)

func newPipeline(up *fakeUpstream) *pipeline.Pipeline {
	return pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{},
		Idempotency: idempotency.NewMemory(clock.Real{})}, pipeline.Options{})
}

var refundArgs = map[string]any{"order_id": "O-00001", "amount_cents": 5000, "reason": "damaged"}

func TestIdempotencyReplay(t *testing.T) {
	up := &fakeUpstream{}
	p := newPipeline(up)
	meta := mcp.Meta{"idempotency_key": "refund-key-0001"}
	first, err := call(p, refundTool, refundArgs, meta)
	if err != nil || first.IsError {
		t.Fatalf("first: %v %+v", err, first)
	}
	// Same arguments, different key order and spacing: same canonical hash, so a replay.
	second, err := call(p, refundTool, map[string]any{"reason": "damaged", "amount_cents": 5000, "order_id": "O-00001"}, meta)
	if err != nil || second.IsError {
		t.Fatalf("second: %v %+v", err, second)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
	if gwMeta(first)["idempotent_replay"] != false || gwMeta(second)["idempotent_replay"] != true {
		t.Fatalf("replay flags = %v / %v", gwMeta(first)["idempotent_replay"], gwMeta(second)["idempotent_replay"])
	}
	// The key and the gateway context are forwarded upstream.
	fwd := up.metas[0]
	gw, _ := fwd["mcpgw"].(map[string]any)
	if fwd["idempotency_key"] != "refund-key-0001" || gw["tenant"] != "t_alpha" || gw["agent"] != "agent-support" || gw["request_id"] == "" {
		t.Fatalf("forwarded meta = %v", fwd)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	up := &fakeUpstream{}
	p := newPipeline(up)
	meta := mcp.Meta{"idempotency_key": "refund-key-0002"}
	if _, err := call(p, refundTool, refundArgs, meta); err != nil {
		t.Fatal(err)
	}
	_, err := call(p, refundTool, map[string]any{"order_id": "O-00001", "amount_cents": 6000, "reason": "damaged"}, meta)
	if c := code(t, err); c != mcperr.CodeIdempotencyConflict {
		t.Fatalf("code = %d, want -32040", c)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", up.calls.Load())
	}
}

func TestIdempotencyKeyRequired(t *testing.T) {
	up := &fakeUpstream{}
	p := newPipeline(up)
	for name, m := range map[string]mcp.Meta{"missing": nil, "too short": {"idempotency_key": "short"},
		"bad chars": {"idempotency_key": "has spaces in it"}} {
		_, err := call(p, refundTool, refundArgs, m)
		if c := code(t, err); c != mcperr.CodeIdempotencyKeyRequired {
			t.Errorf("%s: code = %d, want -32041", name, c)
		}
	}
	// Header and _meta must agree when both are sent; the header alone works.
	hdr := http.Header{"Idempotency-Key": {"header-key-0001"}}
	_, err := p.CallTool(context.Background(), proxy.Call{Principal: principal, Tool: refundTool,
		Request: request(refundArgs, mcp.Meta{"idempotency_key": "other-key-0001"}, hdr)})
	if c := code(t, err); c != mcperr.CodeIdempotencyKeyRequired {
		t.Fatalf("mismatch: code = %d, want -32041", c)
	}
	if _, err := p.CallTool(context.Background(), proxy.Call{Principal: principal, Tool: refundTool,
		Request: request(refundArgs, nil, hdr)}); err != nil {
		t.Fatalf("header key: %v", err)
	}
	// Reads need no key.
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); err != nil {
		t.Fatalf("read without key: %v", err)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.calls.Load())
	}
}

func TestIdempotencyInProgress(t *testing.T) {
	up := &fakeUpstream{block: make(chan struct{}), started: make(chan struct{}, 1)}
	p := newPipeline(up)
	meta := mcp.Meta{"idempotency_key": "refund-key-0003"}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = call(p, refundTool, refundArgs, meta) })
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first call never reached the upstream")
	}
	_, err := call(p, refundTool, refundArgs, meta)
	if c := code(t, err); c != mcperr.CodeIdempotencyInProgress {
		t.Fatalf("code = %d, want -32047", c)
	}
	close(up.block)
	wg.Wait()
}

func TestIdempotencyReleasedOnUpstreamFailure(t *testing.T) {
	failing := true
	up := &fakeUpstream{}
	up.result = func(catalog.Tool, json.RawMessage) (*mcp.CallToolResult, error) {
		if failing {
			return nil, catalog.ErrUpstreamUnavailable
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	p := newPipeline(up)
	meta := mcp.Meta{"idempotency_key": "refund-key-0004"}
	_, err := call(p, refundTool, refundArgs, meta)
	if c := code(t, err); c != mcperr.CodeUpstreamUnavailable {
		t.Fatalf("code = %d, want -32046", c)
	}
	failing = false
	if res, err := call(p, refundTool, refundArgs, meta); err != nil || res.IsError {
		t.Fatalf("retry after failure: %v %+v", err, res)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failure released the key)", up.calls.Load())
	}
}

func TestIdempotencyStoreDownFailsClosed(t *testing.T) {
	up := &fakeUpstream{}
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{}, Idempotency: downStore{}}, pipeline.Options{})
	_, err := call(p, refundTool, refundArgs, mcp.Meta{"idempotency_key": "refund-key-0005"})
	if c := code(t, err); c != mcperr.CodeDependencyUnavailable {
		t.Fatalf("code = %d, want -32049", c)
	}
	if up.calls.Load() != 0 {
		t.Fatal("write reached the upstream with the idempotency store down")
	}
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); err != nil {
		t.Fatalf("reads must not depend on the idempotency store: %v", err)
	}
}

type downStore struct{}

func (downStore) Acquire(context.Context, string, string, string, time.Duration) (idempotency.Acquisition, error) {
	return idempotency.Acquisition{}, context.DeadlineExceeded
}
func (downStore) Complete(context.Context, string, string, string, json.RawMessage, time.Duration) error {
	return context.DeadlineExceeded
}
func (downStore) Release(context.Context, string, string, string) error {
	return context.DeadlineExceeded
}
func (downStore) Ping(context.Context) error { return context.DeadlineExceeded }

// Regression (M2 review): the same key and arguments on another tool is a conflict, not a replay.
func TestIdempotencyKeyBoundToTool(t *testing.T) {
	up := &fakeUpstream{}
	p := newPipeline(up)
	meta := mcp.Meta{"idempotency_key": "shared-key-0001"}
	args := map[string]any{"customer_id": "C-1", "amount_cents": 100, "reason": "x"}
	if _, err := call(p, refundTool, args, meta); err != nil {
		t.Fatal(err)
	}
	other := catalog.Tool{Server: "ledger", Name: "record_promise_to_pay", Def: &mcp.Tool{Name: "ledger.record_promise_to_pay",
		Annotations: &mcp.ToolAnnotations{}}}
	_, err := call(p, other, args, meta)
	if code(t, err) != mcperr.CodeIdempotencyConflict {
		t.Fatalf("err = %v, want -32040", err)
	}
}
