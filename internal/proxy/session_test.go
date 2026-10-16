package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/testupstream"
)

// Sessions of the legacy (session-based) protocol are bound to the principal that created them.
func TestSessionBoundToPrincipal(t *testing.T) {
	echo := httptest.NewServer(testupstream.EchoHandler("echo"))
	t.Cleanup(echo.Close)
	cat := catalog.New([]catalog.UpstreamConfig{{Name: "echo", URL: echo.URL}}, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	px := proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat)})
	// Test authentication: the principal is named by a header.
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := authn.Principal{Tenant: "t_alpha", Agent: r.Header.Get("X-Agent"), Scopes: []string{"echo:read"}}
		px.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(gw.Close)

	post := func(agent, session, body string) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("X-Agent", agent)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"a","version":"0"}}}`
	resp := post("agent-a", "", init)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize: status %d, session %q", resp.StatusCode, sid)
	}
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	if got := post("agent-b", sid, list).StatusCode; got != http.StatusForbidden {
		t.Fatalf("other principal reusing the session: status %d, want 403", got)
	}
}
