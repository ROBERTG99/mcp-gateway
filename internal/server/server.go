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

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
)

// Config is the minimal walking-skeleton configuration.
type Config struct {
	MCPAddr         string
	AdminAddr       string
	Upstreams       []catalog.UpstreamConfig
	CatalogRefresh  time.Duration
	ShutdownTimeout time.Duration
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	cat := catalog.New(cfg.Upstreams, catalog.Options{Logger: logger, ReadRetries: 1})
	defer cat.Close()
	if err := cat.Refresh(ctx); err != nil {
		logger.Warn("initial catalog refresh failed", "error", err)
	}
	px := proxy.New(proxy.Options{Catalog: cat, Caller: proxy.PassThrough(cat), Logger: logger})

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", px)
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
