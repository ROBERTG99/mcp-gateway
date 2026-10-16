// Package server assembles the gateway: MCP endpoint, admin API and console, metrics, and the
// background workers, all under one errgroup cancelled on shutdown.
package server

import (
	"context"
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
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
)

// Run serves until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	upstreams, err := config.LoadUpstreams(cfg.UpstreamsFile)
	if err != nil {
		return err
	}
	for i := range upstreams {
		if upstreams[i].Timeout == 0 {
			upstreams[i].Timeout = cfg.UpstreamTimeout
		}
	}
	verifier := authn.NewVerifier(authn.Options{JWKSURL: cfg.JWKSURL, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience,
		CacheTTL: cfg.JWKSCacheTTL, StaleLimit: cfg.JWKSStaleLimit})
	cat := catalog.New(upstreams, catalog.Options{Logger: logger, ReadRetries: 1})
	defer cat.Close()
	if err := cat.Refresh(ctx); err != nil {
		logger.Warn("initial catalog refresh failed", "error", err)
	}
	px := proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat), Logger: logger,
		CacheTTL: cfg.PrincipalCacheTTL, MaxBodyBytes: cfg.MaxBodyBytes})

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", verifier.Middleware(px))
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	adminMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	g, gctx := errgroup.WithContext(ctx)
	serve := func(addr string, h http.Handler) {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		g.Go(func() error {
			ln, err := (&net.ListenConfig{}).Listen(gctx, "tcp", addr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			logger.Info("listening", "addr", addr)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve %s: %w", addr, err)
			}
			return nil
		})
		g.Go(func() error {
			<-gctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			return srv.Shutdown(sctx) //nolint:contextcheck // shutdown must outlive the cancelled context
		})
	}
	serve(cfg.MCPAddr, mcpMux)
	serve(cfg.AdminAddr, adminMux)
	g.Go(func() error { return cat.Run(gctx, cfg.CatalogRefresh) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}
