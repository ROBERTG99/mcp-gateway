package upstreams

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

// Build constructs an upstream server from a pool (lets cmd/upstream-orders add fault options).
type Build func(pool *pgxpool.Pool) (*mcpserver.Server, error)

// Main runs an example upstream binary until ctx is cancelled and returns the exit code.
func Main(ctx context.Context, name, defaultAddr string, args []string, stderr io.Writer, build Build) int {
	fs := flag.NewFlagSet("upstream-"+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", envOr("MCPGW_UPSTREAM_ADDR", defaultAddr), "listen address")
	dbURL := fs.String("database-url", os.Getenv("MCPGW_UPSTREAM_DATABASE_URL"), "Postgres URL (role mcpgw_upstream)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil)).With("upstream", name)
	pool, err := store.Open(ctx, *dbURL)
	if err != nil {
		logger.Error("database", "error", err)
		return 1
	}
	defer pool.Close()
	if build == nil {
		build = func(p *pgxpool.Pool) (*mcpserver.Server, error) { return New(name, p) }
	}
	srv, err := build(pool)
	if err != nil {
		logger.Error("build", "error", err)
		return 1
	}
	if err := serve(ctx, *addr, Handler(srv, pool), logger); err != nil {
		logger.Error("serve", "error", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("listening", "addr", addr)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
