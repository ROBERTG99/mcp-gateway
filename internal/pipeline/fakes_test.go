package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
)

// fakeUpstream records calls and returns a canned result (or blocks until released).
type fakeUpstream struct {
	mu      sync.Mutex
	calls   atomic.Int64
	metas   []mcp.Meta
	args    []string
	result  func(tool catalog.Tool, args json.RawMessage) (*mcp.CallToolResult, error)
	block   chan struct{}
	started chan struct{} // receives once per call when set
}

func (f *fakeUpstream) Call(ctx context.Context, t catalog.Tool, args json.RawMessage, meta mcp.Meta) (*mcp.CallToolResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.metas = append(f.metas, meta)
	f.args = append(f.args, string(args))
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.result != nil {
		return f.result(t, args)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "done"}},
		StructuredContent: map[string]any{"refund_id": 7},
	}, nil
}

// allowAll is a permissive policy for stage-focused tests.
type allowAll struct{}

func (allowAll) Evaluate(context.Context, policy.Input) (policy.Decision, error) {
	return policy.Decision{Allow: true, Reasons: []string{"scope_granted"}}, nil
}

var (
	principal  = authn.Principal{Tenant: "t_alpha", Agent: "agent-support", Scopes: []string{"orders:write", "crm:read"}}
	refundTool = catalog.Tool{Server: "orders", Name: "refund_order", Def: &mcp.Tool{Name: "orders.refund_order",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}}}
	readTool = catalog.Tool{Server: "crm", Name: "get_customer", Def: &mcp.Tool{Name: "crm.get_customer",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}}
)

func request(args any, meta mcp.Meta, header http.Header) *mcp.CallToolRequest {
	raw, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "x", Arguments: raw, Meta: meta},
		Extra:  &mcp.RequestExtra{Header: header},
	}
}

func call(c proxy.Caller, t catalog.Tool, args any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	return c.CallTool(context.Background(), proxy.Call{Principal: principal, Tool: t, Request: request(args, meta, nil)})
}

func code(t *testing.T, err error) int64 {
	t.Helper()
	var je *jsonrpc.Error
	if !errors.As(err, &je) {
		t.Fatalf("err = %v, want a JSON-RPC error", err)
	}
	return je.Code
}

func gwMeta(res *mcp.CallToolResult) map[string]any {
	m, _ := res.Meta["mcpgw"].(map[string]any)
	return m
}

func pipelineCall(t catalog.Tool, req *mcp.CallToolRequest) proxy.Call {
	return proxy.Call{Principal: authn.Principal{Tenant: "t_alpha", Agent: "agent-collections",
		Scopes: []string{"ledger:write", "orders:write"}}, Tool: t, Request: req}
}
