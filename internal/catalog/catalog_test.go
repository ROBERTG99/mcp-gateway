package catalog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/testupstream"
	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
)

func serve(t *testing.T, s *mcp.Server) string {
	t.Helper()
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

func connect(t *testing.T, url string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, opts).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestProxyAggregatesNamespaces(t *testing.T) {
	var cfgs []catalog.UpstreamConfig
	direct := map[string]*mcp.Tool{}
	for _, name := range upstreams.Names() {
		// Tool definitions do not touch the database; listing needs no pool.
		s, err := upstreams.New(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		url := serve(t, s.MCP())
		cfgs = append(cfgs, catalog.UpstreamConfig{Name: name, URL: url, Timeout: 5 * time.Second})
		res, err := connect(t, url, nil).ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range res.Tools {
			direct[name+"."+tl.Name] = tl
		}
	}
	cat := catalog.New(cfgs, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat)}))
	t.Cleanup(gw.Close)
	res, err := connect(t, gw.URL, nil).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 7 {
		t.Fatalf("gateway lists %d tools, want 7", len(res.Tools))
	}
	for _, tl := range res.Tools {
		want, ok := direct[tl.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", tl.Name)
		}
		got := *tl
		got.Name = want.Name
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		if string(gj) != string(wj) {
			t.Errorf("%s changed by the gateway:\n got %s\nwant %s", tl.Name, gj, wj)
		}
	}
	st := cat.Status()
	if len(st) != 3 || !st[0].Up || len(st[0].Tools) == 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestCatalogListChanged(t *testing.T) {
	up := testupstream.EchoServer("echo")
	cat := catalog.New([]catalog.UpstreamConfig{{Name: "echo", URL: serve(t, up), Timeout: 5 * time.Second}}, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat)}))
	t.Cleanup(gw.Close)
	changed := make(chan struct{}, 8)
	cs := connect(t, gw.URL, &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
		changed <- struct{}{}
	}})
	if res, err := cs.ListTools(t.Context(), nil); err != nil || len(res.Tools) != 1 {
		t.Fatalf("initial tools: %v %v", res, err)
	}
	// The upstream gains a tool: upstream list_changed -> catalog refresh -> gateway list_changed.
	up.AddTool(&mcp.Tool{Name: "shout", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "HI"}}}, nil
		})
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no notifications/tools/list_changed reached the agent")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := cs.ListTools(t.Context(), nil)
		if err == nil && len(res.Tools) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools after change: %+v %v", res, err)
		}
	}
	if _, ok := cat.Lookup("echo.shout"); !ok {
		t.Fatal("catalog lacks echo.shout")
	}
}

// Concurrent refreshes, list_changed rebuilds and calls are race-free and leave one session.
func TestCatalogConcurrentRefresh(t *testing.T) {
	up := testupstream.EchoServer("echo")
	cat := catalog.New([]catalog.UpstreamConfig{{Name: "echo", URL: serve(t, up), Timeout: 5 * time.Second}}, catalog.Options{})
	t.Cleanup(cat.Close)
	var changes int
	var mu sync.Mutex
	cat.OnChange(func() { mu.Lock(); changes++; mu.Unlock() })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = cat.Refresh(context.Background()) })
	}
	wg.Wait()
	tl, ok := cat.Lookup("echo.echo")
	if !ok {
		t.Fatal("echo.echo missing")
	}
	res, err := cat.Call(t.Context(), tl, json.RawMessage(`{"text":"x"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	mu.Lock()
	defer mu.Unlock()
	if changes != 1 {
		t.Fatalf("change notifications = %d, want 1", changes)
	}
}

func TestCatalogUpstreamDown(t *testing.T) {
	cat := catalog.New([]catalog.UpstreamConfig{{Name: "gone", URL: "http://127.0.0.1:1/mcp", Timeout: time.Second}}, catalog.Options{})
	t.Cleanup(cat.Close)
	if err := cat.Refresh(t.Context()); err == nil {
		t.Fatal("refresh of an unreachable upstream succeeded")
	}
	st := cat.Status()
	if st[0].Up || st[0].LastError == "" {
		t.Fatalf("status = %+v", st[0])
	}
	if len(cat.Tools()) != 0 {
		t.Fatal("tools listed for a down upstream")
	}
}
