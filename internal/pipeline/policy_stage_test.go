package pipeline_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

var creditTool = catalog.Tool{Server: "ledger", Name: "issue_credit_note", Def: &mcp.Tool{Name: "ledger.issue_credit_note",
	Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}}}

func realPolicyPipeline(t *testing.T, up *fakeUpstream) *pipeline.Pipeline {
	t.Helper()
	engine, err := policy.New(t.Context(), policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	return pipeline.New(pipeline.Deps{Upstream: up, Policy: engine, Idempotency: idempotency.NewMemory(clock.Real{})}, pipeline.Options{})
}

func TestPolicyStageWithRealBundle(t *testing.T) {
	up := &fakeUpstream{}
	p := realPolicyPipeline(t, up)
	for _, c := range []struct {
		amount float64
		allow  bool
		reason string
	}{{5000, true, ""}, {150000, false, "refund_over_limit"}} {
		_, err := call(p, refundTool, map[string]any{"order_id": "O-1", "amount_cents": c.amount, "reason": "r"},
			mcp.Meta{"idempotency_key": "policy-key-" + string(rune('a'+int(c.amount)%26)) + "00001"})
		if c.allow {
			if err != nil {
				t.Fatalf("%v: %v", c.amount, err)
			}
			continue
		}
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != mcperr.CodePolicyDenied {
			t.Fatalf("%v: err = %v, want -32044", c.amount, err)
		}
		var data struct{ Reasons []string }
		_ = json.Unmarshal(je.Data, &data)
		if len(data.Reasons) != 1 || data.Reasons[0] != c.reason {
			t.Fatalf("%v: data = %s", c.amount, je.Data)
		}
	}
}

// Regression (M2 review): "arguments": null must not look like tools/list to the policy.
func TestNullArgumentsDoNotBypassArgumentRules(t *testing.T) {
	up := &fakeUpstream{}
	p := realPolicyPipeline(t, up)
	for name, raw := range map[string]json.RawMessage{"null": json.RawMessage(`null`), "absent": nil} {
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "ledger.issue_credit_note", Arguments: raw,
			Meta: mcp.Meta{"idempotency_key": "credit-key-" + name + "-01"}}}
		_, err := p.CallTool(t.Context(), pipelineCall(creditTool, req))
		if err == nil {
			t.Fatalf("%s arguments: credit note executed without approval", name)
		}
	}
	for name, raw := range map[string]json.RawMessage{"array": json.RawMessage(`[1,2]`), "string": json.RawMessage(`"x"`)} {
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "ledger.issue_credit_note", Arguments: raw,
			Meta: mcp.Meta{"idempotency_key": "credit-key-" + name + "-01"}}}
		_, err := p.CallTool(t.Context(), pipelineCall(creditTool, req))
		if code(t, err) != jsonrpc.CodeInvalidParams {
			t.Fatalf("%s arguments: %v, want -32602", name, err)
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream called %d times", up.calls.Load())
	}
}
