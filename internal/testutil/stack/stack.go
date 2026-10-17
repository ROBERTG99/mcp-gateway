// Package stack starts a complete gateway in process for integration tests: real Postgres and
// Redis (testcontainers or compose), the dev issuer, the three example upstreams (the orders one
// with the stall fault), a WireMock-compatible webhook sink and the real server.App.
package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/server"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/redistest"
	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

// Options tune the stack.
type Options struct {
	UpstreamTimeout time.Duration // gateway upstream timeout (default 2 s)
	StallPrefix     string        // orders keys that stall (default "demo-stall-")
	Stall           time.Duration // stall duration (default 3 s)
	FaultRate       float64       // orders chaos timeout rate
	// ExtraUpstreams are appended to the upstreams file (name -> MCP URL).
	ExtraUpstreams map[string]string
	// PolicyDir overrides the policy bundle (default the repository's policies/).
	PolicyDir string
	// LogWriter receives the gateway's logs from the production logger at debug level (default:
	// discarded).
	LogWriter io.Writer
	// Configure adjusts the gateway config before start.
	Configure func(*config.Config)
}

// Stack is a running in-process gateway.
type Stack struct {
	MCPURL    string // .../mcp
	AdminURL  string
	IssuerURL string
	SinkURL   string // webhook sink base URL (WireMock-like /__admin/requests)
	Issuer    *devissuer.Server
	App       *server.App
	DB        *pgxpool.Pool // superuser pool on the test database
	Sink      *Sink
	Config    config.Config // the gateway's configuration (dependencies of a second App)
}

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// Start brings the stack up and registers its shutdown with t.Cleanup.
func Start(t testing.TB, opts Options) *Stack {
	t.Helper()
	if opts.UpstreamTimeout == 0 {
		opts.UpstreamTimeout = 2 * time.Second
	}
	if opts.StallPrefix == "" {
		opts.StallPrefix = "demo-stall-"
	}
	if opts.Stall == 0 {
		opts.Stall = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	db := pgtest.New(t)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_app": "app", "mcpgw_upstream": "up"}); err != nil {
		t.Fatal(err)
	}
	super, err := store.Open(ctx, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := datagen.Seed(ctx, super, datagen.Generate(datagen.DefaultSeed)); err != nil {
		t.Fatal(err)
	}
	upPool, err := store.Open(ctx, db.RoleURL("mcpgw_upstream", "up"))
	if err != nil {
		t.Fatal(err)
	}

	st := &Stack{DB: super, Sink: &Sink{}}
	st.Issuer, err = devissuer.New(devissuer.Options{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	issuerSrv := httptest.NewServer(st.Issuer.Handler())
	st.IssuerURL = issuerSrv.URL
	sinkSrv := httptest.NewServer(st.Sink)
	st.SinkURL = sinkSrv.URL

	var lines []string
	var servers []*httptest.Server
	for _, name := range upstreams.Names() {
		var srv *mcpserver.Server
		if name == "orders" {
			dedup, err := mcpserver.NewPostgresDedup(upPool, "orders")
			if err != nil {
				t.Fatal(err)
			}
			srv = mcpserver.New("orders", version.Version(), dedup)
			upstreams.RegisterOrders(srv, upPool, upstreams.OrdersOptions{
				BeforeRefund: upstreams.StallFault(opts.FaultRate, opts.StallPrefix, opts.Stall)})
		} else if srv, err = upstreams.New(name, upPool); err != nil {
			t.Fatal(err)
		}
		hs := httptest.NewServer(upstreams.Handler(srv, upPool))
		servers = append(servers, hs)
		lines = append(lines, fmt.Sprintf("  - name: %s\n    url: %s/mcp\n", name, hs.URL))
	}
	for name, url := range opts.ExtraUpstreams {
		lines = append(lines, fmt.Sprintf("  - name: %s\n    url: %s\n", name, url))
	}
	upFile := filepath.Join(t.TempDir(), "upstreams.yaml")
	if err := os.WriteFile(upFile, []byte("upstreams:\n"+strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	rc := redistest.New(t)
	cfg.DatabaseURL = db.RoleURL("mcpgw_app", "app")
	cfg.RedisURL = fmt.Sprintf("redis://%s/%d", rc.Options().Addr, rc.Options().DB)
	cfg.JWKSURL = issuerSrv.URL + "/.well-known/jwks.json"
	cfg.UpstreamsFile = upFile
	cfg.TenantsFile = filepath.Join(Root(), "config", "tenants.yaml")
	cfg.PolicyDir = filepath.Join(Root(), "policies")
	if opts.PolicyDir != "" {
		cfg.PolicyDir = opts.PolicyDir
	}
	cfg.UpstreamTimeout = opts.UpstreamTimeout
	cfg.ApprovalWebhookURL = sinkSrv.URL + "/approvals"
	cfg.ApprovalWebhookSecret = "stack-webhook-secret"
	cfg.PolicyReload = 0
	if opts.Configure != nil {
		opts.Configure(&cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.LogWriter != nil {
		logger = telemetry.NewLogger(opts.LogWriter, "debug")
	}
	st.Config = cfg
	st.App, err = server.New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = st.App.RunBackground(ctx)
	}()
	mcpSrv := httptest.NewServer(st.App.MCPHandler())
	adminSrv := httptest.NewServer(st.App.AdminHandler())
	st.MCPURL, st.AdminURL = mcpSrv.URL+"/mcp", adminSrv.URL
	deadline := time.Now().Add(30 * time.Second)
	for !allUp(st.App) {
		if time.Now().After(deadline) {
			t.Fatalf("stack: upstreams not up: %+v", st.App.Upstreams())
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // waiting for the background catalog refresh
	}
	t.Cleanup(func() {
		mcpSrv.Close()
		adminSrv.Close()
		cancel()
		<-done
		st.App.Close()
		for _, s := range servers {
			s.Close()
		}
		issuerSrv.Close()
		sinkSrv.Close()
		upPool.Close()
		super.Close()
	})
	return st
}

func allUp(a *server.App) bool {
	for _, u := range a.Upstreams() {
		if !u.Up {
			return false
		}
	}
	return len(a.Upstreams()) > 0
}

// Token mints a dev token.
func (s *Stack) Token(t testing.TB, tenant, sub string, scopes ...string) string {
	t.Helper()
	tok, _, err := s.Issuer.Mint(tenant, sub, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Sink records webhook deliveries and answers WireMock's GET /__admin/requests.
type Sink struct {
	mu       sync.Mutex
	requests []SinkRequest
}

// SinkRequest is one recorded delivery.
type SinkRequest struct {
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// ServeHTTP implements http.Handler.
func (s *Sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/__admin/requests" {
		s.mu.Lock()
		out := make([]map[string]any, 0, len(s.requests))
		for _, rq := range s.requests {
			out = append(out, map[string]any{"request": rq})
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": out})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.requests = append(s.requests, SinkRequest{URL: r.URL.Path, Body: string(body),
		Headers: map[string]string{"X-MCPGW-Signature": r.Header.Get("X-MCPGW-Signature")}})
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Requests returns the recorded deliveries.
func (s *Sink) Requests() []SinkRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SinkRequest(nil), s.requests...)
}
