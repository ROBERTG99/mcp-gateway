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
	"os"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/console"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
)

// App is an assembled gateway.
type App struct {
	cfg      config.Config
	logger   *slog.Logger
	verifier *authn.Verifier
	catalog  *catalog.Catalog
	policy   *policy.Engine
	proxy    *proxy.Proxy
	pipeline *pipeline.Pipeline
	redis    *redis.Client
	db       *pgxpool.Pool
	audit    *audit.Writer
	seq      *audit.PostgresSeq
	redactor *redact.Redactor

	approvalStore *approvals.Postgres
	approvals     *approvals.Service
	console       *console.Console
	metrics       *telemetry.Metrics
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
	if cfg.Node == "" {
		cfg.Node, _ = os.Hostname() // app.node on spans and step records
	}
	a := &App{cfg: cfg, logger: logger}
	// Metrics probes read the components at scrape time, so they may be wired after this point.
	a.metrics = telemetry.NewMetrics(telemetry.Probes{
		AuditDepth:      func() int { return a.audit.Depth() },
		AuditCapacity:   func() int { return a.audit.Capacity() },
		PendingApproval: func(ctx context.Context) (int64, time.Duration, error) { return a.approvals.PendingStats(ctx) },
		PolicyLoaded:    func() bool { return a.policy.Ready() },
		Upstreams: func() map[string]bool {
			up := map[string]bool{}
			for _, u := range a.catalog.Status() {
				up[u.Name] = u.Up && u.Breaker != catalog.BreakerOpen
			}
			return up
		},
	})
	a.verifier = authn.NewVerifier(authn.Options{JWKSURL: cfg.JWKSURL, Issuer: cfg.JWTIssuer, Audience: cfg.JWTAudience,
		CacheTTL: cfg.JWKSCacheTTL, StaleLimit: cfg.JWKSStaleLimit})
	// Upstream MCP requests carry W3C trace headers (the pipeline also forwards them in _meta).
	upstreamHTTP := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	a.catalog = catalog.New(upstreams, catalog.Options{Logger: logger, ReadRetries: 1, HTTPClient: upstreamHTTP,
		BreakerThreshold: cfg.BreakerThreshold, BreakerCooldown: cfg.BreakerCooldown})
	a.policy, err = policy.New(ctx, policy.Options{Dir: cfg.PolicyDir, OnFailure: func(error) { a.metrics.PolicyFailure() }})
	if err != nil {
		logger.Error("policy bundle failed to load; denying all calls", "error", err)
	}
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("MCPGW_REDIS_URL: %w", err)
	}
	a.redis = redis.NewClient(redisOpts)
	if err := redisotel.InstrumentTracing(a.redis); err != nil {
		return nil, fmt.Errorf("redis tracing: %w", err)
	}
	idem, err := idempotency.Open(ctx, idempotency.OpenOptions{Backend: cfg.IdempotencyStore, Redis: a.redis,
		DynamoDBEndpoint: cfg.DynamoDBEndpoint, DynamoDBTable: cfg.DynamoDBTable, DynamoDBRegion: cfg.DynamoDBRegion})
	if err != nil {
		return nil, fmt.Errorf("idempotency store: %w", err)
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("MCPGW_DATABASE_URL: %w", err)
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
	a.db, err = pgxpool.NewWithConfig(ctx, poolCfg) // connects lazily; readiness pings it
	if err != nil {
		return nil, fmt.Errorf("MCPGW_DATABASE_URL: %w", err)
	}
	tenants, err := config.LoadTenants(cfg.TenantsFile)
	if err != nil {
		return nil, err
	}
	a.seq = audit.NewPostgresSeq(a.db, 1024)
	a.audit = audit.NewWriter(audit.NewPostgresSink(a.db), a.seq, audit.Options{Buffer: cfg.AuditBuffer,
		FlushInterval: cfg.AuditFlushInterval, Batch: cfg.AuditBatch, EnqueueTimeout: cfg.AuditEnqueueTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout, Logger: logger,
		OnFlush: func(_ []audit.Event, d time.Duration) { a.metrics.AuditFlush(d) }})
	a.redactor = redact.New()
	a.approvalStore = approvals.NewPostgres(a.db)
	a.approvals = approvals.NewService(a.approvalStore, approvals.Options{TTL: cfg.ApprovalTTL, Redactor: a.redactor,
		Audit: a.audit, Logger: logger})
	var tokens console.TokenSource // persona sign-in exists only with a dev issuer (development)
	if cfg.DevIssuerURL != "" {
		issuerClient := &http.Client{Timeout: 5 * time.Second}
		tokens = func(ctx context.Context, p devissuer.Persona) (string, error) {
			return devissuer.RequestToken(ctx, issuerClient, cfg.DevIssuerURL, p.Tenant, p.Subject, p.Scopes)
		}
	}
	a.console, err = console.New(console.Deps{Verifier: a.verifier, Audit: audit.NewPostgresReader(a.db), Approvals: a.approvals,
		Policy: a.policy, Catalog: a.catalog, Flush: a.audit.Flush, Tokens: tokens},
		console.Options{SecureCookies: cfg.ConsoleSecureCookies, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	a.pipeline = pipeline.New(pipeline.Deps{Upstream: a.catalog, Policy: a.policy, Idempotency: idem, Logger: logger, Metrics: a.metrics,
		Audit: a.audit, Redactor: a.redactor, Approvals: a.approvals,
		Limiter: ratelimit.NewRedis(a.redis, tenants, nil), LocalLimiter: ratelimit.NewMemory(tenants, nil),
		Scorer: injection.New(injection.Options{}), Findings: store.NewFindings(a.db)},
		pipeline.Options{Node: cfg.Node, IdempotencyTTL: cfg.IdempotencyTTL, IdempotencyLockTTL: cfg.IdempotencyLockTTL,
			MaxStoredResult: int(cfg.MaxStoredResultBytes), InjectionThreshold: cfg.InjectionThreshold,
			InjectionMode: cfg.InjectionMode})
	a.proxy = proxy.New(proxy.Options{Catalog: a.catalog, Caller: a.pipeline, Logger: logger,
		Visible:  proxy.PolicyVisibility(a.policy),
		CacheTTL: cfg.PrincipalCacheTTL, MaxBodyBytes: cfg.MaxBodyBytes})
	a.policy.OnReload(a.proxy.Invalidate)
	return a, nil
}

// MCPHandler serves /mcp (authenticated).
func (a *App) MCPHandler() http.Handler {
	mux := http.NewServeMux()
	node := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.SpanFromContext(r.Context()).SetAttributes(telemetry.AttrNode.String(a.cfg.Node))
		a.verifier.Middleware(a.proxy).ServeHTTP(w, r)
	})
	mux.Handle("/mcp", otelhttp.NewHandler(node, "mcp.request",
		otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return "mcp.request" })))
	return mux
}

// AdminHandler serves the admin API, console and health endpoints.
func (a *App) AdminHandler() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", a.readyz)
	api := admin.New(a.adminDeps())
	r.Group(func(r chi.Router) {
		r.Use(a.verifier.Middleware)
		api.Mount(r)
	})
	if a.console != nil {
		a.console.Mount(r)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/console", http.StatusSeeOther) })
	}
	return r
}

func (a *App) adminDeps() admin.Deps {
	return admin.Deps{Verifier: a.verifier, Audit: audit.NewPostgresReader(a.db), Approvals: a.approvals,
		Policy: a.policy, Catalog: a.catalog, Flush: a.audit.Flush}
}

func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"policy": "ok", "postgres": "ok", "redis": "ok"}
	ready := true
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if err := a.db.Ping(ctx); err != nil {
		checks["postgres"], ready = "unreachable", false
	}
	if err := a.redis.Ping(ctx).Err(); err != nil {
		checks["redis"], ready = "unreachable", false
	}
	switch {
	case !a.policy.Ready():
		checks["policy"], ready = "not loaded: "+a.policy.LastError(), false
	case a.policy.LastError() != "":
		// The last good bundle keeps serving (docs/adr/0002); the failure is visible here and in the logs.
		checks["policy"] = "stale: last reload failed: " + a.policy.LastError()
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// Run serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	defer a.Close()
	g, gctx := errgroup.WithContext(ctx)
	a.serve(g, gctx, a.cfg.MCPAddr, a.MCPHandler())
	a.serve(g, gctx, a.cfg.AdminAddr, a.AdminHandler())
	if a.cfg.MetricsAddr != "" {
		a.serve(g, gctx, a.cfg.MetricsAddr, a.MetricsHandler())
	}
	g.Go(func() error { return a.RunBackground(gctx) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return nil
}

// RunBackground performs the startup work (catalog refresh, audit sequence warm-up) and runs the
// background workers until ctx is cancelled: audit writer, catalog refresh, webhook outbox,
// approval maintenance and policy watch. Tests use it with MCPHandler and AdminHandler.
func (a *App) RunBackground(ctx context.Context) error {
	if err := a.catalog.Refresh(ctx); err != nil {
		a.logger.Warn("initial catalog refresh failed", "error", err)
	}
	if err := a.seq.Warm(ctx); err != nil {
		a.logger.Error("audit sequence unavailable; calls fail with -32048 until Postgres is reachable", "error", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.audit.Run(gctx) })
	g.Go(func() error { return a.catalog.Run(gctx, a.cfg.CatalogRefresh) })
	if a.cfg.ApprovalWebhookURL != "" {
		ow := approvals.NewOutboxWorker(a.approvalStore,
			approvals.NewWebhookNotifier(a.cfg.ApprovalWebhookURL, a.cfg.ApprovalWebhookSecret, nil),
			approvals.OutboxOptions{Logger: a.logger})
		g.Go(func() error { return ow.Run(gctx, time.Second) })
	}
	g.Go(func() error { return a.maintain(gctx) })
	g.Go(func() error {
		return a.policy.Watch(gctx, a.cfg.PolicyReload, func(err error) {
			a.logger.Error("policy reload failed; keeping the previous bundle", "error", err)
		})
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("gateway background: %w", err)
	}
	return nil
}

// Upstreams reports upstream health (used by tests to wait for the catalog).
func (a *App) Upstreams() []catalog.UpstreamStatus { return a.catalog.Status() }

// MetricsHandler serves Prometheus metrics (MCPGW_METRICS_ADDR, 8211).
func (a *App) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", a.metrics.Handler())
	return mux
}

// Close releases the connection pools and upstream sessions.
func (a *App) Close() {
	a.catalog.Close()
	_ = a.redis.Close()
	a.db.Close()
}

// maintain expires overdue approvals every minute and applies retention every RetentionInterval.
func (a *App) maintain(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.approvals.Maintain(ctx); err != nil {
				a.logger.Warn("approval maintenance failed", "error", err)
			}
		}
	}
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
