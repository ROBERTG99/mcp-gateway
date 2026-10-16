package mcpserver_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

type addIn struct {
	CustomerID string `json:"customer_id"`
	Text       string `json:"text"`
}

type addOut struct {
	NoteID int64  `json:"note_id"`
	Tenant string `json:"tenant"`
}

// connect starts the server in memory and returns a client session.
func connect(t *testing.T, s *mcpserver.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "gw", Version: "v0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func newNotesServer(executions *atomic.Int64) *mcpserver.Server {
	s := mcpserver.New("crm", "v0.1.0", mcpserver.NewMemoryDedup())
	mcpserver.WriteTool(s, &mcp.Tool{Name: "add_note", Description: "append a note"},
		func(_ context.Context, w mcpserver.WriteContext, in addIn) (addOut, error) {
			n := executions.Add(1)
			return addOut{NoteID: 100 + n, Tenant: w.Tenant}, nil
		})
	return s
}

func meta(tenant, key string) mcp.Meta {
	m := mcp.Meta{"mcpgw": map[string]any{"tenant": tenant, "agent": "agent-support", "request_id": "r1"}}
	if key != "" {
		m["idempotency_key"] = key
	}
	return m
}

func TestWriteToolDedup(t *testing.T) {
	var executions atomic.Int64
	cs := connect(t, newNotesServer(&executions))
	params := &mcp.CallToolParams{Name: "add_note", Arguments: addIn{CustomerID: "C-0001", Text: "hi"},
		Meta: meta("t_alpha", "key-00000001")}
	first, err := cs.CallTool(t.Context(), params)
	if err != nil || first.IsError {
		t.Fatalf("first call: %v %+v", err, first)
	}
	second, err := cs.CallTool(t.Context(), params)
	if err != nil || second.IsError {
		t.Fatalf("second call: %v %+v", err, second)
	}
	if executions.Load() != 1 {
		t.Fatalf("handler executed %d times, want 1", executions.Load())
	}
	if first.StructuredContent == nil || second.Content[0].(*mcp.TextContent).Text != first.Content[0].(*mcp.TextContent).Text {
		t.Fatalf("replayed result differs: %+v vs %+v", first, second)
	}
	// Same key, different arguments: conflict.
	params.Arguments = addIn{CustomerID: "C-0001", Text: "different"}
	_, err = cs.CallTool(t.Context(), params)
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != mcpserver.CodeIdempotencyConflict {
		t.Fatalf("reused key with other args: err = %v, want code %d", err, mcpserver.CodeIdempotencyConflict)
	}
	// The same key in another tenant is a different operation.
	params.Arguments = addIn{CustomerID: "C-0001", Text: "hi"}
	params.Meta = meta("t_beta", "key-00000001")
	if res, err := cs.CallTool(t.Context(), params); err != nil || res.IsError {
		t.Fatalf("other tenant: %v %+v", err, res)
	}
	if executions.Load() != 2 {
		t.Fatalf("executions = %d, want 2", executions.Load())
	}
}

func TestWriteToolConcurrentDedup(t *testing.T) {
	var executions atomic.Int64
	cs := connect(t, newNotesServer(&executions))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "add_note",
				Arguments: addIn{CustomerID: "C-1", Text: "x"}, Meta: meta("t_alpha", "same-key-123")})
		})
	}
	wg.Wait()
	if executions.Load() != 1 {
		t.Fatalf("handler executed %d times under concurrency, want 1", executions.Load())
	}
}

func TestWriteToolRequiresKey(t *testing.T) {
	var executions atomic.Int64
	cs := connect(t, newNotesServer(&executions))
	for _, m := range []mcp.Meta{meta("t_alpha", ""), meta("", "key-00000002"), meta("t_alpha", "bad key!"), nil} {
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "add_note",
			Arguments: addIn{CustomerID: "C-1", Text: "x"}, Meta: m})
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("meta %v: err = %v, want invalid params", m, err)
		}
	}
	if executions.Load() != 0 {
		t.Fatalf("handler ran %d times without a valid key/tenant", executions.Load())
	}
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a := tools.Tools[0].Annotations; a == nil || a.ReadOnlyHint {
		t.Fatalf("write tool annotations = %+v, want readOnlyHint false", a)
	}
}

func TestReadToolTenantContext(t *testing.T) {
	s := mcpserver.New("crm", "v0.1.0", mcpserver.NewMemoryDedup())
	type in struct {
		CustomerID string `json:"customer_id"`
	}
	type out struct {
		Tenant string `json:"tenant"`
	}
	mcpserver.ReadTool(s, &mcp.Tool{Name: "get_customer"}, func(_ context.Context, c mcpserver.CallInfo, _ in) (out, error) {
		return out{Tenant: c.Tenant}, nil
	})
	cs := connect(t, s)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_customer", Arguments: in{CustomerID: "C-1"},
		Meta: meta("t_gamma", "")})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if got := res.StructuredContent.(map[string]any)["tenant"]; got != "t_gamma" {
		t.Fatalf("tenant = %v", got)
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_customer", Arguments: in{CustomerID: "C-1"}}); err == nil {
		t.Fatal("read without tenant succeeded")
	}
	tools, _ := cs.ListTools(t.Context(), nil)
	if !tools.Tools[0].Annotations.ReadOnlyHint {
		t.Fatal("read tool must have readOnlyHint")
	}
}
