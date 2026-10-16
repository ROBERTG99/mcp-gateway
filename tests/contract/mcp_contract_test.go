// Package contract checks the §4.4 contract shapes of mcp-gateway v0.1.0 against golden JSON.
package contract_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

func golden(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func keys(t *testing.T, v any) []string {
	t.Helper()
	raw, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestErrorCodes(t *testing.T) {
	var want map[string]string
	golden(t, "errors.json", &want)
	for code, msg := range want {
		n, _ := strconv.ParseInt(code, 10, 64)
		if got := mcperr.Message(n); got != msg {
			t.Errorf("code %s message = %q, want %q", code, got, msg)
		}
		if e := mcperr.New(n, nil); e.Code != n || e.Message != msg {
			t.Errorf("mcperr.New(%d) = %+v", n, e)
		}
	}
}

type upstream struct{}

func (upstream) Call(context.Context, catalog.Tool, json.RawMessage, mcp.Meta) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}

func TestCallShapes(t *testing.T) {
	var shapes map[string][]string
	golden(t, "shapes.json", &shapes)
	ctx := context.Background()
	engine, err := policy.New(ctx, policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	store := approvals.NewMemory()
	svc := approvals.NewService(store, approvals.Options{})
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{FlushInterval: time.Millisecond})
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(rctx) }()
	p := pipeline.New(pipeline.Deps{Upstream: upstream{}, Policy: engine, Idempotency: idempotency.NewMemory(clock.Real{}),
		Approvals: svc, Audit: w, Redactor: redact.New(), Scorer: injection.New(injection.Options{})}, pipeline.Options{})
	tool := catalog.Tool{Server: "orders", Name: "refund_order", Def: &mcp.Tool{Name: "orders.refund_order", Annotations: &mcp.ToolAnnotations{}}}
	principal := authn.Principal{Tenant: "t_alpha", Agent: "agent-support", Scopes: []string{"orders:write"}}
	callWith := func(amount int, key string) *mcp.CallToolResult {
		raw, _ := json.Marshal(map[string]any{"order_id": "O-00001", "amount_cents": amount, "reason": "r"})
		res, err := p.CallTool(ctx, proxy.Call{Principal: principal, Tool: tool, Request: &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Name: "orders.refund_order", Arguments: raw, Meta: mcp.Meta{"idempotency_key": key}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	held := callWith(50000, "contract-key-0001")
	if !held.IsError || !slices.Equal(keys(t, held.StructuredContent), shapes["approval_required"]) {
		t.Fatalf("approval_required shape = %v", keys(t, held.StructuredContent))
	}
	ok := callWith(5000, "contract-key-0002")
	meta := ok.Meta["mcpgw"]
	if got := keys(t, meta); !slices.Equal(got, shapes["result_meta_mcpgw"]) {
		t.Fatalf("_meta.mcpgw keys = %v", got)
	}
	if got := keys(t, meta.(map[string]any)["injection"]); !slices.Equal(got, shapes["injection"]) {
		t.Fatalf("injection keys = %v", got)
	}
	if sc, _ := ok.StructuredContent.(map[string]any); sc["untrusted"] != true {
		t.Fatalf("structuredContent.untrusted missing: %v", ok.StructuredContent)
	}
	if text := ok.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "<<untrusted tool output>>\n") {
		t.Fatalf("text not prefixed: %q", text)
	}
	_ = w.Flush(ctx)
	events := sink.Events()
	if len(events) != 2 || !slices.Equal(keys(t, events[0]), shapes["audit_event"]) {
		t.Fatalf("audit event keys = %v", keys(t, events[0]))
	}
	if res := audit.VerifyChain(events, audit.GenesisHash); !res.OK {
		t.Fatalf("chain: %+v", res)
	}
	due, _ := store.DueOutbox(ctx, time.Now().Add(time.Minute), 10)
	if len(due) != 1 || !slices.Equal(keys(t, due[0].Payload), shapes["webhook"]) {
		t.Fatalf("webhook payload = %+v", due)
	}
	d, _ := engine.Evaluate(ctx, policy.Input{Tenant: "t_alpha", Scopes: []string{}, Server: "crm", Tool: "get_customer"})
	if got := keys(t, d); !slices.Equal(got, shapes["policy_decision"]) {
		t.Fatalf("decision keys = %v", got)
	}
}
