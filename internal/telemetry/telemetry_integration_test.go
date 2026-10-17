//go:build integration

package telemetry_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// TestSpanTreeForToolCall (spec §14): one trace covers a tools/call from the HTTP request through
// authentication, every pipeline stage and Redis, into the upstream, which continues the trace
// from the W3C context the gateway forwards.
func TestSpanTreeForToolCall(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	telemetry.SetPropagators()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	st := stack.Start(t, stack.Options{})
	sess, err := demo.Connect(t.Context(), st.MCPURL, st.Token(t, "t_alpha", "agent-support", "orders:read", "orders:write"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "orders.refund_order",
		Arguments: map[string]any{"order_id": "O-00007", "amount_cents": 500, "reason": "trace test"},
		Meta:      mcp.Meta{"idempotency_key": "trace-test-0001", "run_id": "run-trace-1"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if err := tp.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	var upstream sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == telemetry.SpanUpstream {
			upstream = s
		}
	}
	if upstream == nil {
		t.Fatalf("no %s span among %d spans", telemetry.SpanUpstream, len(spans))
	}
	traceID := upstream.SpanContext().TraceID()
	inTrace := map[string]sdktrace.ReadOnlySpan{}
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	redis := false
	for _, s := range spans {
		if s.SpanContext().TraceID() != traceID {
			continue
		}
		inTrace[s.Name()] = s
		byID[s.SpanContext().SpanID()] = s
		for _, a := range s.Attributes() {
			if a.Key == "db.system" && a.Value.AsString() == "redis" || a.Key == "db.system.name" && a.Value.AsString() == "redis" {
				redis = true
			}
		}
	}
	for _, name := range []string{telemetry.SpanRequest, telemetry.SpanAuthn, telemetry.SpanPolicy, telemetry.SpanIdempotency,
		telemetry.SpanUpstream, telemetry.SpanAudit, "mcpserver.tool refund_order"} {
		if _, ok := inTrace[name]; !ok {
			names := []string{}
			for n := range inTrace {
				names = append(names, n)
			}
			t.Fatalf("trace lacks %s; it has %v", name, names)
		}
	}
	if !redis {
		t.Error("no go-redis span in the trace (idempotency acquire)")
	}
	// The upstream's tool span descends from the gateway's upstream call.
	for s := inTrace["mcpserver.tool refund_order"]; ; {
		parent, ok := byID[s.Parent().SpanID()]
		if !ok {
			t.Fatalf("upstream span is not under %s", telemetry.SpanUpstream)
		}
		if parent.Name() == telemetry.SpanUpstream {
			break
		}
		s = parent
	}
	// Attributes: tenant and node on the request, tenant, node and run id on every stage span.
	attrs := func(s sdktrace.ReadOnlySpan) map[string]string {
		m := map[string]string{}
		for _, a := range s.Attributes() {
			m[string(a.Key)] = a.Value.String()
		}
		return m
	}
	if a := attrs(inTrace[telemetry.SpanRequest]); a["app.tenant_id"] != "t_alpha" || a["app.node"] == "" {
		t.Fatalf("mcp.request attributes = %v", a)
	}
	for _, name := range []string{telemetry.SpanPolicy, telemetry.SpanIdempotency, telemetry.SpanUpstream, telemetry.SpanAudit} {
		if a := attrs(inTrace[name]); a["app.tenant_id"] != "t_alpha" || a["app.run_id"] != "run-trace-1" || a["app.node"] == "" {
			t.Fatalf("%s attributes = %v", name, a)
		}
	}
}
