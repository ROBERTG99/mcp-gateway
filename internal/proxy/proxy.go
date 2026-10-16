// Package proxy serves the aggregated MCP endpoint. Each principal (tenant, agent, scopes) gets
// its own *mcp.Server holding only the tools it may see, so hidden tools are indistinguishable
// from nonexistent ones. Servers are cached for CacheTTL and resynchronised in place when the
// catalog or the policy changes, which makes the SDK emit notifications/tools/list_changed.
package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
)

// Call is one tools/call to execute on behalf of a principal.
type Call struct {
	Principal authn.Principal
	Tool      catalog.Tool
	Request   *mcp.CallToolRequest
}

// Caller executes tool calls (the governance pipeline in production).
type Caller interface {
	CallTool(ctx context.Context, call Call) (*mcp.CallToolResult, error)
}

// CallerFunc adapts a function to Caller.
type CallerFunc func(ctx context.Context, call Call) (*mcp.CallToolResult, error)

// CallTool calls f.
func (f CallerFunc) CallTool(ctx context.Context, call Call) (*mcp.CallToolResult, error) {
	return f(ctx, call)
}

// PassThrough forwards calls to the upstream unchanged (walking skeleton and tests).
func PassThrough(cat *catalog.Catalog) Caller {
	return CallerFunc(func(ctx context.Context, call Call) (*mcp.CallToolResult, error) {
		args := json.RawMessage("{}")
		if call.Request.Params != nil && len(call.Request.Params.Arguments) > 0 {
			args = call.Request.Params.Arguments
		}
		return cat.Call(ctx, call.Tool, args, nil)
	})
}

// Visibility decides whether a principal may see (list and call) a tool.
type Visibility func(ctx context.Context, p authn.Principal, t catalog.Tool) bool

// Options configure the proxy handler.
type Options struct {
	Catalog *catalog.Catalog
	Caller  Caller
	// Visible filters tools per principal; nil shows every tool.
	Visible Visibility
	// CacheTTL bounds how long an idle per-principal server is cached (default 60 s).
	CacheTTL time.Duration
	Clock    clock.Clock
	Logger   *slog.Logger
	// MaxBodyBytes limits request bodies (default 1 MiB, NFR-9).
	MaxBodyBytes int64
}

// Proxy is the MCP endpoint handler.
type Proxy struct {
	opts    Options
	handler http.Handler

	mu      sync.Mutex
	servers map[string]*entry
}

type entry struct {
	server    *mcp.Server
	principal authn.Principal
	lastUsed  time.Time // guarded by Proxy.mu

	syncMu sync.Mutex
	tools  map[string]string // full name -> definition fingerprint currently registered
}

// anonymous is used when no authentication middleware ran (tests, walking skeleton).
var anonymous = authn.Principal{Tenant: "anonymous", Agent: "anonymous"}

// New builds the proxy and subscribes it to catalog changes.
func New(opts Options) *Proxy {
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 60 * time.Second
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	p := &Proxy{opts: opts, servers: map[string]*entry{}}
	p.handler = mcp.NewStreamableHTTPHandler(p.serverFor, &mcp.StreamableHTTPOptions{
		MaxRequestBodyBytes:        opts.MaxBodyBytes,
		DisableLocalhostProtection: true, // the gateway authenticates every request with a bearer token
		Logger:                     opts.Logger,
	})
	opts.Catalog.OnChange(p.Invalidate)
	return p
}

// ServeHTTP serves POST, GET and DELETE /mcp. When an authenticated principal is in the request
// context it is also exposed to the SDK as token info, which binds sessions to that principal.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if pr, ok := authn.FromContext(r.Context()); ok {
		r = withTokenInfo(r, pr)
	}
	p.handler.ServeHTTP(w, r)
}

func withTokenInfo(r *http.Request, pr authn.Principal) *http.Request {
	var out *http.Request
	mw := auth.RequireBearerToken(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{Scopes: pr.Scopes, Expiration: pr.ExpiresAt, UserID: pr.Key()}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	mw(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) { out = req })).ServeHTTP(discard{}, r)
	if out == nil {
		return r
	}
	return out
}

// discard is a ResponseWriter for the token-info adapter, which never writes on success.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// Invalidate resynchronises every cached server with the current catalog and visibility
// (call after a policy reload). Connected sessions receive notifications/tools/list_changed.
func (p *Proxy) Invalidate() {
	p.mu.Lock()
	entries := make([]*entry, 0, len(p.servers))
	for _, e := range p.servers {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	for _, e := range entries {
		p.sync(context.Background(), e)
	}
}

func (p *Proxy) serverFor(r *http.Request) *mcp.Server {
	pr, ok := authn.FromContext(r.Context())
	if !ok {
		pr = anonymous
	}
	now := p.opts.Clock.Now()
	key := pr.Key()
	p.mu.Lock()
	p.evictLocked(now)
	e, ok := p.servers[key]
	if !ok {
		e = &entry{
			server: mcp.NewServer(&mcp.Implementation{Name: "mcp-gateway", Version: version.Version()},
				&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}}),
			principal: pr,
			tools:     map[string]string{},
		}
		p.servers[key] = e
	}
	e.lastUsed = now
	p.mu.Unlock()
	if !ok {
		p.sync(r.Context(), e)
	}
	return e.server
}

// evictLocked drops idle entries without live sessions. Callers hold p.mu.
func (p *Proxy) evictLocked(now time.Time) {
	for key, e := range p.servers {
		if now.Sub(e.lastUsed) < p.opts.CacheTTL {
			continue
		}
		live := false
		for range e.server.Sessions() {
			live = true
			break
		}
		if !live {
			delete(p.servers, key)
		}
	}
}

// sync makes the entry's server hold exactly the tools visible to its principal. Only actual
// changes touch the server, so unchanged tool sets send no list_changed notification.
func (p *Proxy) sync(ctx context.Context, e *entry) {
	want := map[string]catalog.Tool{}
	for _, t := range p.opts.Catalog.Tools() {
		if p.opts.Visible == nil || p.opts.Visible(ctx, e.principal, t) {
			want[t.FullName()] = t
		}
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	var remove []string
	for name := range e.tools {
		if _, ok := want[name]; !ok {
			remove = append(remove, name)
			delete(e.tools, name)
		}
	}
	if len(remove) > 0 {
		e.server.RemoveTools(remove...)
	}
	for name, t := range want {
		fp := fingerprint(t.Def)
		if e.tools[name] == fp {
			continue
		}
		e.server.AddTool(t.Def, p.toolHandler(e.principal, t))
		e.tools[name] = fp
	}
}

func fingerprint(def *mcp.Tool) string {
	raw, err := json.Marshal(def)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (p *Proxy) toolHandler(pr authn.Principal, t catalog.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tool := t
		// Re-resolve so a refreshed catalog definition is used.
		if fresh, ok := p.opts.Catalog.Lookup(t.FullName()); ok {
			tool = fresh
		}
		return p.opts.Caller.CallTool(ctx, Call{Principal: pr, Tool: tool, Request: req})
	}
}
