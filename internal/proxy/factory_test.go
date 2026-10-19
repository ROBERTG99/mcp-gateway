package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
)

type gateway struct {
	url    string
	px     *proxy.Proxy
	engine *policy.Engine
}

// newGateway serves the three example tool sets (listing only, no database) behind a proxy that
// filters with the real policy bundle. The principal comes from X-Tenant / X-Scopes headers.
func newGateway(t *testing.T, policyDir string) gateway {
	t.Helper()
	var cfgs []catalog.UpstreamConfig
	for _, name := range upstreams.Names() {
		s, err := upstreams.New(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCP() }, nil))
		t.Cleanup(srv.Close)
		cfgs = append(cfgs, catalog.UpstreamConfig{Name: name, URL: srv.URL, Timeout: 5 * time.Second})
	}
	cat := catalog.New(cfgs, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(t.Context(), policy.Options{Dir: policyDir})
	if err != nil {
		t.Fatal(err)
	}
	px := proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat), Visible: proxy.PolicyVisibility(engine)})
	engine.OnReload(px.Invalidate)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := authn.Principal{Tenant: r.Header.Get("X-Tenant"), Agent: "agent-readonly", Scopes: splitScopes(r.Header.Get("X-Scopes"))}
		px.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(gw.Close)
	return gateway{url: gw.URL, px: px, engine: engine}
}

func splitScopes(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

type headers struct {
	h    http.Header
	next http.RoundTripper
}

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.h {
		r.Header[k] = v
	}
	return h.next.RoundTrip(r)
}

func agent(t *testing.T, url, tenant, scopes string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: headers{h: http.Header{"X-Tenant": {tenant}, "X-Scopes": {scopes}}, next: http.DefaultTransport}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, opts).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func names(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range res.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestToolsListFilteredByPolicy(t *testing.T) {
	gw := newGateway(t, "../../policies")
	if got := names(t, agent(t, gw.url, "t_alpha", "crm:read", nil)); len(got) != 1 || got[0] != "crm.get_customer" {
		t.Fatalf("crm:read agent sees %v, want [crm.get_customer]", got)
	}
	all := "ledger:read,ledger:write,crm:read,crm:write,orders:read,orders:write"
	if got := names(t, agent(t, gw.url, "t_alpha", all, nil)); len(got) != 7 {
		t.Fatalf("all-scopes agent sees %d tools (%v), want 7", len(got), got)
	}
	// t_gamma's allowlist has no ledger.
	for _, n := range names(t, agent(t, gw.url, "t_gamma", all, nil)) {
		if n[:7] == "ledger." {
			t.Fatalf("t_gamma sees %s", n)
		}
	}
}

func TestHiddenToolLooksUnknown(t *testing.T) {
	gw := newGateway(t, "../../policies")
	cs := agent(t, gw.url, "t_alpha", "crm:read", nil)
	call := func(name string) *jsonrpc.Error {
		_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"customer_id": "C-1"}})
		var je *jsonrpc.Error
		if !errors.As(err, &je) {
			t.Fatalf("%s: err = %v, want a JSON-RPC error", name, err)
		}
		return je
	}
	hidden, missing := call("ledger.get_balance"), call("ledger.no_such_tool")
	if hidden.Code != jsonrpc.CodeInvalidParams || missing.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("codes = %d / %d, want -32602", hidden.Code, missing.Code)
	}
	// Same shape: only the echoed name differs.
	if hidden.Message != `unknown tool "ledger.get_balance"` || missing.Message != `unknown tool "ledger.no_such_tool"` || len(hidden.Data) != len(missing.Data) {
		t.Fatalf("hidden %+v vs missing %+v", hidden, missing)
	}
}

func TestVisibilityFollowsPolicyReload(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"authz.rego", "limits.rego", "approval.rego", "data/tenants.json"} {
		b, err := os.ReadFile(filepath.Join("../../policies", f))
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o750)
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gw := newGateway(t, dir)
	changed := make(chan struct{}, 4)
	cs := agent(t, gw.url, "t_alpha", "crm:read", &mcp.ClientOptions{ToolListChangedHandler: func(_ context.Context, _ *mcp.ToolListChangedRequest) {
		changed <- struct{}{}
	}})
	if got := names(t, cs); len(got) != 1 {
		t.Fatalf("before reload: %v", got)
	}
	// Remove crm from t_alpha's allowlist and reload: the tool disappears from the cached server.
	if err := os.WriteFile(filepath.Join(dir, "data/tenants.json"), []byte(`{"t_alpha": {"upstreams": ["ledger"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gw.engine.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no list_changed after policy reload")
	}
	if got := names(t, cs); len(got) != 0 {
		t.Fatalf("after reload: %v, want none", got)
	}
}
