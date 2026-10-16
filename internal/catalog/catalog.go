// Package catalog connects to the upstream MCP servers, discovers their tools and exposes
// them under namespaced names (<server>.<tool>). It refreshes periodically and when an
// upstream sends notifications/tools/list_changed, and notifies listeners on every change.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
)

// ServerNamePattern is the contract for upstream server names (spec §4.4).
var ServerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// ErrUpstreamUnavailable means the upstream could not be reached, timed out or its breaker is open.
var ErrUpstreamUnavailable = errors.New("upstream unavailable")

// UpstreamConfig describes one upstream MCP server.
type UpstreamConfig struct {
	Name    string        `yaml:"name" json:"name"`
	URL     string        `yaml:"url" json:"url"`
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

// Tool is an upstream tool exposed by the gateway.
type Tool struct {
	Server string    // upstream server name
	Name   string    // tool name at the upstream
	Def    *mcp.Tool // definition as exposed by the gateway: Name is "<server>.<tool>", the rest unchanged
}

// FullName is the namespaced tool name "<server>.<tool>".
func (t Tool) FullName() string { return t.Server + "." + t.Name }

// ReadOnly reports the tool's readOnlyHint annotation. Tools without the hint are writes.
func (t Tool) ReadOnly() bool { return t.Def.Annotations != nil && t.Def.Annotations.ReadOnlyHint }

// Annotations returns the tool annotations as a plain map (for policy input).
func (t Tool) Annotations() map[string]any {
	out := map[string]any{"readOnlyHint": t.ReadOnly()}
	if a := t.Def.Annotations; a != nil {
		if a.DestructiveHint != nil {
			out["destructiveHint"] = *a.DestructiveHint
		}
		out["idempotentHint"] = a.IdempotentHint
		if a.OpenWorldHint != nil {
			out["openWorldHint"] = *a.OpenWorldHint
		}
	}
	return out
}

// UpstreamStatus is the health of one upstream for /admin/upstreams.
type UpstreamStatus struct {
	Name        string       `json:"name"`
	URL         string       `json:"url"`
	Up          bool         `json:"up"`
	Breaker     BreakerState `json:"breaker"`
	Tools       []string     `json:"tools"`
	LastError   string       `json:"last_error,omitempty"`
	LastRefresh time.Time    `json:"last_refresh"`
}

// Options configure a Catalog.
type Options struct {
	HTTPClient       *http.Client
	Logger           *slog.Logger
	Clock            clock.Clock
	BreakerThreshold int           // consecutive failures before opening (default 5)
	BreakerCooldown  time.Duration // default 30 s
	// ReadRetries is how many times a failed read-only call is retried (spec: once).
	ReadRetries int
}

// Catalog is the live view of upstream tools. It is safe for concurrent use.
type Catalog struct {
	opts      Options
	baseCtx   context.Context // cancelled by Close; parent of background refreshes
	cancel    context.CancelFunc
	mu        sync.RWMutex
	upstreams []*upstream
	byName    map[string]*upstream
	tools     map[string]Tool
	listeners []func()
	closed    bool
}

type upstream struct {
	cfg     UpstreamConfig
	breaker *Breaker

	mu          sync.Mutex
	session     *mcp.ClientSession
	tools       []Tool
	up          bool
	lastErr     string
	lastRefresh time.Time
}

// New builds a catalog. Call Refresh (or Run) to connect.
func New(cfgs []UpstreamConfig, opts Options) *Catalog {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.BreakerThreshold == 0 {
		opts.BreakerThreshold = 5
	}
	if opts.BreakerCooldown == 0 {
		opts.BreakerCooldown = 30 * time.Second
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	c := &Catalog{opts: opts, baseCtx: baseCtx, cancel: cancel, byName: map[string]*upstream{}, tools: map[string]Tool{}}
	for _, cfg := range cfgs {
		if cfg.Timeout == 0 {
			cfg.Timeout = 10 * time.Second
		}
		u := &upstream{cfg: cfg, breaker: NewBreaker(opts.Clock, opts.BreakerThreshold, opts.BreakerCooldown)}
		c.upstreams = append(c.upstreams, u)
		c.byName[cfg.Name] = u
	}
	return c
}

// OnChange registers fn to be called (synchronously) after the tool set changes.
func (c *Catalog) OnChange(fn func()) {
	c.mu.Lock()
	c.listeners = append(c.listeners, fn)
	c.mu.Unlock()
}

// Tools returns the current tools sorted by full name.
func (c *Catalog) Tools() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Tool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out
}

// Lookup finds a tool by its namespaced name.
func (c *Catalog) Lookup(fullName string) (Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tools[fullName]
	return t, ok
}

// Status returns per-upstream health.
func (c *Catalog) Status() []UpstreamStatus {
	out := make([]UpstreamStatus, 0, len(c.upstreams))
	for _, u := range c.upstreams {
		u.mu.Lock()
		names := make([]string, 0, len(u.tools))
		for _, t := range u.tools {
			names = append(names, t.FullName())
		}
		out = append(out, UpstreamStatus{
			Name: u.cfg.Name, URL: u.cfg.URL, Up: u.up, Breaker: u.breaker.State(), Tools: names,
			LastError: u.lastErr, LastRefresh: u.lastRefresh,
		})
		u.mu.Unlock()
	}
	return out
}

// Refresh reconnects as needed and re-lists the tools of every upstream. Unreachable upstreams
// keep their last known tools out of the catalog (they are removed) and are reported down.
// It returns an error only when every upstream failed.
func (c *Catalog) Refresh(ctx context.Context) error {
	var errs []error
	for _, u := range c.upstreams {
		if err := c.refreshUpstream(ctx, u); err != nil {
			errs = append(errs, err)
		}
	}
	c.rebuild()
	if len(errs) > 0 && len(errs) == len(c.upstreams) {
		return errors.Join(errs...)
	}
	return nil
}

// Run refreshes every interval until ctx is cancelled.
func (c *Catalog) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.Refresh(ctx); err != nil {
				c.opts.Logger.Warn("catalog refresh failed", "error", err)
			}
		}
	}
}

// Close closes every upstream session.
func (c *Catalog) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	for _, u := range c.upstreams {
		u.mu.Lock()
		if u.session != nil {
			_ = u.session.Close()
			u.session = nil
		}
		u.mu.Unlock()
	}
}

func (c *Catalog) connect(ctx context.Context, u *upstream) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-gateway", Version: version.Version()}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			// Re-list in the background: the handler runs on the session's read loop.
			go func() {
				rctx, cancel := context.WithTimeout(c.baseCtx, u.cfg.Timeout)
				defer cancel()
				if err := c.refreshUpstream(rctx, u); err != nil {
					c.opts.Logger.Warn("upstream list_changed refresh failed", "upstream", u.cfg.Name, "error", err)
				}
				c.rebuild()
			}()
		},
	})
	cctx, cancel := context.WithTimeout(ctx, u.cfg.Timeout)
	defer cancel()
	session, err := client.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: u.cfg.URL, HTTPClient: c.opts.HTTPClient}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", u.cfg.Name, err)
	}
	return session, nil
}

func (c *Catalog) refreshUpstream(ctx context.Context, u *upstream) error {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil
	}
	u.mu.Lock()
	session := u.session
	u.mu.Unlock()
	if session == nil {
		s, err := c.connect(ctx, u)
		if err != nil {
			u.markDown(err, c.opts.Clock.Now())
			return err
		}
		session = s
		u.mu.Lock()
		u.session = s
		u.mu.Unlock()
	}
	lctx, cancel := context.WithTimeout(ctx, u.cfg.Timeout)
	defer cancel()
	var defs []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for {
		res, err := session.ListTools(lctx, params)
		if err != nil {
			u.mu.Lock()
			if u.session == session {
				_ = session.Close()
				u.session = nil
			}
			u.mu.Unlock()
			err = fmt.Errorf("list tools %s: %w", u.cfg.Name, err)
			u.markDown(err, c.opts.Clock.Now())
			return err
		}
		defs = append(defs, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		params = &mcp.ListToolsParams{Cursor: res.NextCursor}
	}
	tools := make([]Tool, 0, len(defs))
	for _, d := range defs {
		def := *d // shallow copy: schemas and annotations are shared, unchanged
		def.Name = u.cfg.Name + "." + d.Name
		tools = append(tools, Tool{Server: u.cfg.Name, Name: d.Name, Def: &def})
	}
	u.mu.Lock()
	u.tools, u.up, u.lastErr, u.lastRefresh = tools, true, "", c.opts.Clock.Now()
	u.mu.Unlock()
	return nil
}

func (u *upstream) markDown(err error, now time.Time) {
	u.mu.Lock()
	u.up, u.lastErr, u.lastRefresh, u.tools = false, err.Error(), now, nil
	u.mu.Unlock()
}

// rebuild recomputes the tool map and notifies listeners when it changed.
func (c *Catalog) rebuild() {
	next := map[string]Tool{}
	for _, u := range c.upstreams {
		u.mu.Lock()
		for _, t := range u.tools {
			next[t.FullName()] = t
		}
		u.mu.Unlock()
	}
	c.mu.Lock()
	changed := !sameTools(c.tools, next)
	c.tools = next
	listeners := slices.Clone(c.listeners)
	c.mu.Unlock()
	if changed {
		for _, fn := range listeners {
			fn()
		}
	}
}

func sameTools(a, b map[string]Tool) bool {
	if len(a) != len(b) {
		return false
	}
	for name, ta := range a {
		tb, ok := b[name]
		if !ok {
			return false
		}
		ja, _ := json.Marshal(ta.Def)
		jb, _ := json.Marshal(tb.Def)
		if string(ja) != string(jb) {
			return false
		}
	}
	return true
}

// Call forwards a tool call to its upstream. Read-only tools are retried once on transport
// failure; writes are never retried by the gateway (clients retry with the same key).
// Transport failures, timeouts and an open breaker return an error wrapping ErrUpstreamUnavailable.
func (c *Catalog) Call(ctx context.Context, t Tool, args json.RawMessage, meta mcp.Meta) (*mcp.CallToolResult, error) {
	u, ok := c.byName[t.Server]
	if !ok {
		return nil, fmt.Errorf("%w: unknown upstream %q", ErrUpstreamUnavailable, t.Server)
	}
	attempts := 1
	if t.ReadOnly() {
		attempts += max(c.opts.ReadRetries, 0)
	}
	var lastErr error
	for range attempts {
		res, err := c.callOnce(ctx, u, t, args, meta)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil || !errors.Is(err, ErrUpstreamUnavailable) {
			break
		}
	}
	return nil, lastErr
}

func (c *Catalog) callOnce(ctx context.Context, u *upstream, t Tool, args json.RawMessage, meta mcp.Meta) (*mcp.CallToolResult, error) {
	if !u.breaker.Allow() {
		return nil, fmt.Errorf("%w: %s breaker open", ErrUpstreamUnavailable, u.cfg.Name)
	}
	u.mu.Lock()
	session := u.session
	u.mu.Unlock()
	if session == nil {
		s, err := c.connect(ctx, u)
		if err != nil {
			u.breaker.Failure()
			return nil, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
		}
		u.mu.Lock()
		if u.session == nil {
			u.session = s
		} else {
			_ = s.Close()
		}
		session = u.session
		u.mu.Unlock()
	}
	cctx, cancel := context.WithTimeout(ctx, u.cfg.Timeout)
	defer cancel()
	res, err := session.CallTool(cctx, &mcp.CallToolParams{Name: t.Name, Arguments: args, Meta: meta})
	var protocolErr *jsonrpc.Error
	if errors.As(err, &protocolErr) && cctx.Err() == nil {
		// The upstream answered with a JSON-RPC error: it is reachable, pass the error through.
		u.breaker.Success()
		return nil, protocolErr
	}
	if err != nil {
		u.breaker.Failure()
		if isTransportError(err) || cctx.Err() != nil {
			u.mu.Lock()
			if u.session == session {
				_ = session.Close()
				u.session = nil
			}
			u.mu.Unlock()
		}
		return nil, fmt.Errorf("%w: %s.%s: %w", ErrUpstreamUnavailable, u.cfg.Name, t.Name, err)
	}
	u.breaker.Success()
	return res, nil
}

func isTransportError(err error) bool {
	return errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, context.DeadlineExceeded)
}
