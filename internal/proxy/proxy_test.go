package proxy_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/testupstream"
)

func TestProxyForwardsCall(t *testing.T) {
	ctx := t.Context()
	echo := httptest.NewServer(testupstream.EchoHandler("echo"))
	t.Cleanup(echo.Close)

	cat := catalog.New([]catalog.UpstreamConfig{{Name: "echo", URL: echo.URL, Timeout: 5 * time.Second}}, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	gw := httptest.NewServer(proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat)}))
	t.Cleanup(gw.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo.echo" {
		t.Fatalf("tools = %+v, want [echo.echo]", tools.Tools)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo.echo", Arguments: map[string]any{"text": "hello"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("result is error: %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("content = %#v, want text hello", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if string(raw) != `{"text":"hello"}` {
		t.Fatalf("structured = %s", raw)
	}
}
