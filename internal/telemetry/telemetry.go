// Package telemetry sets up OpenTelemetry tracing (spec §14), the trace-aware slog handler and the
// per-call step record. Spans: mcp.request (HTTP server), mcpgw.authn, mcpgw.policy.eval,
// mcpgw.idempotency, mcpgw.upstream.call (W3C context forwarded in _meta and HTTP headers) and
// mcpgw.audit.enqueue, plus pgx and go-redis. Attributes app.tenant_id, app.run_id, app.node.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Instrumentation scope and attribute keys.
const (
	ScopeName = "github.com/ROBERTG99/mcp-gateway"

	AttrTenant = attribute.Key("app.tenant_id")
	AttrRunID  = attribute.Key("app.run_id")
	AttrNode   = attribute.Key("app.node")
)

// Span names (spec §14).
const (
	SpanRequest     = "mcp.request"
	SpanAuthn       = "mcpgw.authn"
	SpanPolicy      = "mcpgw.policy.eval"
	SpanIdempotency = "mcpgw.idempotency"
	SpanUpstream    = "mcpgw.upstream.call"
	SpanAudit       = "mcpgw.audit.enqueue"
)

// Tracer is the gateway's tracer (from the global provider, so tests can install a recorder).
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Options configure Setup.
type Options struct {
	// Endpoint is the OTLP/HTTP endpoint (host:port or URL); empty records spans without exporting
	// them (trace ids still reach the logs).
	Endpoint    string
	ServiceName string
	Node        string // app.node (default: hostname)
	Version     string
}

// Setup installs the global tracer provider and the W3C propagators and returns a shutdown func.
func Setup(ctx context.Context, opts Options) (func(context.Context) error, error) {
	if opts.Node == "" {
		opts.Node, _ = os.Hostname()
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "mcp-gateway"
	}
	res := resource.NewSchemaless(attribute.String("service.name", opts.ServiceName),
		attribute.String("service.version", opts.Version), AttrNode.String(opts.Node))
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if opts.Endpoint != "" {
		exp, err := otlptracehttp.New(ctx, otlpEndpoint(opts.Endpoint)...)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	SetPropagators()
	return tp.Shutdown, nil
}

func otlpEndpoint(e string) []otlptracehttp.Option {
	if u := OTLPTracesURL(e); u != "" {
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(u)}
	}
	return []otlptracehttp.Option{otlptracehttp.WithEndpoint(e), otlptracehttp.WithInsecure()}
}

// OTLPTracesURL returns the traces URL for an endpoint given as a URL ("http://jaeger:4318" →
// "http://jaeger:4318/v1/traces"; an explicit path is kept), or "" for a bare host:port.
func OTLPTracesURL(e string) string {
	u, err := url.Parse(e)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String()
}

// SetPropagators installs W3C trace context and baggage.
func SetPropagators() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
}

// InjectMeta writes the W3C trace context of ctx into an MCP _meta map (keys "traceparent",
// "tracestate"), so an upstream can continue the trace whatever its transport does.
func InjectMeta(ctx context.Context, meta map[string]any) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		meta[k] = v
	}
}

// ExtractMeta returns ctx with the trace context found in an MCP _meta map.
func ExtractMeta(ctx context.Context, meta map[string]any) context.Context {
	carrier := propagation.MapCarrier{}
	for _, k := range []string{"traceparent", "tracestate", "baggage"} {
		if v, ok := meta[k].(string); ok {
			carrier[k] = v
		}
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// TraceHandler adds trace_id and span_id to every record logged with a context that carries a span.
type TraceHandler struct{ slog.Handler }

// Handle implements slog.Handler.
func (h TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r) //nolint:wrapcheck // passthrough
}

// WithAttrs implements slog.Handler.
func (h TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return TraceHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h TraceHandler) WithGroup(name string) slog.Handler {
	return TraceHandler{h.Handler.WithGroup(name)}
}

// StepRecord is the portfolio's shared step record (evalgate StepRecord): the gateway logs one per
// tool call with kind "tool", no model, and zero tokens and cost.
type StepRecord struct {
	RunID         string    `json:"run_id"`
	TaskID        string    `json:"task_id"`
	Step          int       `json:"step"`
	Node          string    `json:"node"`
	Kind          string    `json:"kind"`
	Model         *string   `json:"model"`
	PromptVersion *string   `json:"prompt_version"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	CostUSD       float64   `json:"cost_usd"`
	LatencyMS     float64   `json:"latency_ms"`
	Status        string    `json:"status"`
	Error         *string   `json:"error"`
	StartedAt     time.Time `json:"started_at"`
}

// LogStep logs the step record as one structured "step" line.
func LogStep(ctx context.Context, l *slog.Logger, s StepRecord) {
	l.LogAttrs(ctx, slog.LevelInfo, "step", slog.String("run_id", s.RunID), slog.String("task_id", s.TaskID),
		slog.Int("step", s.Step), slog.String("node", s.Node), slog.String("kind", s.Kind), slog.Any("model", s.Model),
		slog.Any("prompt_version", s.PromptVersion), slog.Int("input_tokens", s.InputTokens),
		slog.Int("output_tokens", s.OutputTokens), slog.Float64("cost_usd", s.CostUSD), slog.Float64("latency_ms", s.LatencyMS),
		slog.String("status", s.Status), slog.Any("error", s.Error), slog.Time("started_at", s.StartedAt))
}
