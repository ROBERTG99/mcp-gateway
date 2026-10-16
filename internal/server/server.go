// Package server assembles the gateway: MCP endpoint, admin API and console, metrics, and the
// background workers, all under one errgroup cancelled on shutdown.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
)

// App is an assembled gateway.
type App struct {
	cfg      config.Config
	logger   *slog.Logger
	verifier *authn.Verifier
	catalog  *catalog.Catalog
	policy   *policy.Engine
	proxy    *proxy.Proxy
}

// New wires the gateway components. A policy bundle that fails to compile does not stop the
// gateway: it starts not-ready and denies every call until a reload succeeds.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	upstreams, err := config.LoadUpstreams(cfg.UpstreamsFile)
	if err != nil {
		return nil, err
	}
	for i := range upstreams {
		if upstreams[i].Timeout == 0 {
			upstreams[i].Timeout = cfg.UpstreamTimeout
		}
	}
	a := &App{cfg: cfg, logger: logger}
	a.verifier = authn.NewVerifier(authn.Options{JWKSURL: cfg.JWKSURL, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience,
		CacheTTL: cfg.JWKSCacheTTL, StaleLimit: cfg.JWKSStaleLimit})
	a.catalog = catalog.New(upstreams, catalog.Options{Logger: logger, ReadRetries: 1})
	a.policy, err = policy.New(ctx, policy.Options{Dir: cfg.PolicyDir})
	if err != nil {
		logger.Error("policy bundle failed to load; denying all calls", "error", err)
	}
	a.proxy = proxy.New(proxy.Options{Catalog: a.catalog, Caller: proxy.PassThrough(a.catalog), Logger: logger,
		CacheTTL: cfg.PrincipalCacheTTL, MaxBodyBytes: cfg.MaxBodyBytes})
	a.policy.OnReload(a.proxy.Invalidate)
	return a, nil
}

// MCPHandler serves /mcp (authenticated).
func (a *App) MCPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", a.verifier.Middleware(a.proxy))
	return mux
}

// AdminHandler serves the admin API, console and health endpoints.
func (a *App) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", a.readyz)
	return mux
}

func (a *App) readyz(w http.ResponseWriter, _ *http.Request) {
	checks := map[string]string{"policy": "ok"}
	ready := true
	if !a.policy.Ready() {
		checks["policy"], ready = "not loaded: "+a.policy.LastError(), false
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// Run serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	defer a.catalog.Close()
	if err := a.catalog.Refresh(ctx); err != nil {
		a.logger.Warn("initial catalog refresh failed", "error", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	a.serve(g, gctx, a.cfg.MCPAddr, a.MCPHandler())
	a.serve(g, gctx, a.cfg.AdminAddr, a.AdminHandler())
	g.Go(func() error { return a.catalog.Run(gctx, a.cfg.CatalogRefresh) })
	g.Go(func() error {
		return a.policy.Watch(gctx, a.cfg.PolicyReload, func(err error) {
			a.logger.Error("policy reload failed; keeping the previous bundle", "error", err)
		})
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}

func (a *App) serve(g *errgroup.Group, ctx context.Context, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	g.Go(func() error {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		a.logger.Info("listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve %s: %w", addr, err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(sctx)
	})
}

// Run builds and runs the gateway.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	a, err := New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
