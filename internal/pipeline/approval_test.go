package pipeline_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

var approver = authn.Principal{Tenant: "t_alpha", Agent: "jo.approver", Scopes: []string{"mcpgw:approve"}}

type approvalFixture struct {
	p     *pipeline.Pipeline
	svc   *approvals.Service
	up    *fakeUpstream
	clock *clock.Fake
	sink  *audit.MemorySink
	w     *audit.Writer
}

func newApprovalFixture(t *testing.T) approvalFixture {
	t.Helper()
	engine, err := policy.New(t.Context(), policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	c := clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	svc := approvals.NewService(approvals.NewMemory(), approvals.Options{TTL: 15 * time.Minute, Clock: c})
	up := &fakeUpstream{}
	_, sink, w := auditedPipeline(t, up) // reuse the audit writer setup
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: engine, Idempotency: idempotency.NewMemory(c), Approvals: svc,
		Audit: w, Redactor: redact.New(), Clock: c}, pipeline.Options{})
	return approvalFixture{p: p, svc: svc, up: up, clock: c, sink: sink, w: w}
}

var heldArgs = map[string]any{"order_id": "O-00001", "amount_cents": 50000, "reason": "damaged"}

func heldResult(t *testing.T, res *mcp.CallToolResult, err error) (string, []any) {
	t.Helper()
	if err != nil {
		t.Fatalf("held call returned an error: %v", err)
	}
	var wire struct {
		IsError           bool           `json:"isError"`
		StructuredContent map[string]any `json:"structuredContent"`
	}
	raw, _ := json.Marshal(res) // as the agent sees it on the wire
	_ = json.Unmarshal(raw, &wire)
	sc := wire.StructuredContent
	if !res.IsError || sc["status"] != "approval_required" || sc["approval_id"] == "" {
		t.Fatalf("held result = %+v", res)
	}
	return sc["approval_id"].(string), sc["reasons"].([]any)
}

func TestApprovalLifecycle(t *testing.T) {
	f := newApprovalFixture(t)
	meta := mcp.Meta{"idempotency_key": "refund-held-0001"}
	id, reasons := held(t, f.p, heldArgs, meta)
	if len(reasons) != 1 || reasons[0] != "refund_requires_approval" {
		t.Fatalf("reasons = %v", reasons)
	}
	// While pending, the same call gets the same approval back.
	if again, _ := held(t, f.p, heldArgs, meta); again != id {
		t.Fatalf("second hold = %s, want %s", again, id)
	}
	// Retrying with the approval id before a decision is still held.
	withApproval := mcp.Meta{"idempotency_key": "refund-held-0001", "approval_id": id}
	if again, _ := held(t, f.p, heldArgs, withApproval); again != id {
		t.Fatal("pending retry changed the approval")
	}
	if f.up.calls.Load() != 0 {
		t.Fatal("held call reached the upstream")
	}
	if _, err := f.svc.Decide(t.Context(), id, approver, true, "ok"); err != nil {
		t.Fatal(err)
	}
	res, err := call(f.p, refundTool, heldArgs, withApproval)
	if err != nil || res.IsError {
		t.Fatalf("approved retry: %v %+v", err, res)
	}
	// Retrying again (same key) replays; it never executes twice.
	res2, err := call(f.p, refundTool, heldArgs, withApproval)
	if err != nil || gwMeta(res2)["idempotent_replay"] != true {
		t.Fatalf("second approved retry: %v %+v", err, res2)
	}
	if f.up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", f.up.calls.Load())
	}
	_ = f.w.Flush(t.Context())
	var statuses []string
	for _, e := range f.sink.Events() {
		if e.ApprovalID == nil || *e.ApprovalID != id {
			t.Fatalf("event without the approval id: %+v", e)
		}
		statuses = append(statuses, e.Status)
	}
	want := []string{"held", "held", "held", "ok", "replayed"}
	if len(statuses) != len(want) {
		t.Fatalf("audit statuses = %v, want %v", statuses, want)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("audit statuses = %v, want %v", statuses, want)
		}
	}
}

func held(t *testing.T, p *pipeline.Pipeline, args any, meta mcp.Meta) (string, []any) {
	t.Helper()
	res, err := call(p, refundTool, args, meta)
	return heldResult(t, res, err)
}

func approvalReason(t *testing.T, err error) string {
	t.Helper()
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != mcperr.CodeApprovalInvalid {
		t.Fatalf("err = %v, want -32045", err)
	}
	var data struct{ Reason string }
	_ = json.Unmarshal(je.Data, &data)
	return data.Reason
}

func TestApprovalBoundToArgsHash(t *testing.T) {
	f := newApprovalFixture(t)
	meta := mcp.Meta{"idempotency_key": "refund-held-0002"}
	id, _ := held(t, f.p, heldArgs, meta)
	if _, err := f.svc.Decide(t.Context(), id, approver, true, "ok"); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{"order_id": "O-00001", "amount_cents": 90000, "reason": "damaged"}
	_, err := call(f.p, refundTool, other, mcp.Meta{"idempotency_key": "refund-held-0002", "approval_id": id})
	if r := approvalReason(t, err); r != approvals.ReasonMismatch {
		t.Fatalf("other args: %s", r)
	}
	_, err = call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-9999", "approval_id": id})
	if r := approvalReason(t, err); r != approvals.ReasonMismatch {
		t.Fatalf("other key: %s", r)
	}
	_, err = call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0002", "approval_id": "00000000-0000-4000-8000-000000000000"})
	if r := approvalReason(t, err); r != approvals.ReasonNotFound {
		t.Fatalf("unknown: %s", r)
	}
	if f.up.calls.Load() != 0 {
		t.Fatal("a mismatched retry executed")
	}
	// Once consumed by its own call, any other reuse is approval_consumed; the same-key retry replays.
	if _, err := call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0002", "approval_id": id}); err != nil {
		t.Fatalf("approved retry: %v", err)
	}
	_, err = call(f.p, refundTool, other, mcp.Meta{"idempotency_key": "refund-held-0002", "approval_id": id})
	if r := approvalReason(t, err); r != approvals.ReasonConsumed {
		t.Fatalf("consumed, other args: %s", r)
	}
	_, err = call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0005", "approval_id": id})
	if r := approvalReason(t, err); r != approvals.ReasonConsumed {
		t.Fatalf("consumed, other key: %s", r)
	}
	res, err := call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0002", "approval_id": id})
	if err != nil || gwMeta(res)["idempotent_replay"] != true {
		t.Fatalf("same-key retry after consumption: %v", err)
	}
	if f.up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", f.up.calls.Load())
	}
	// Rejected and expired approvals.
	rid, _ := held(t, f.p, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0003"})
	if _, err := f.svc.Decide(t.Context(), rid, approver, false, "no"); err != nil {
		t.Fatal(err)
	}
	_, err = call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0003", "approval_id": rid})
	if r := approvalReason(t, err); r != approvals.ReasonRejected {
		t.Fatalf("rejected: %s", r)
	}
	eid, _ := held(t, f.p, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0004"})
	f.clock.Advance(16 * time.Minute)
	_, err = call(f.p, refundTool, heldArgs, mcp.Meta{"idempotency_key": "refund-held-0004", "approval_id": eid})
	if r := approvalReason(t, err); r != approvals.ReasonExpired {
		t.Fatalf("expired: %s", r)
	}
}
