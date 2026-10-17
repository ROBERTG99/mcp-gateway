//go:build integration

package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestMetricsAfterToolCalls (spec §14): after a write, its replay and a flagged CRM read, the
// metrics endpoint exposes every gateway metric with the expected series.
func TestMetricsAfterToolCalls(t *testing.T) {
	st := stack.Start(t, stack.Options{})
	sess, err := demo.Connect(t.Context(), st.MCPURL, st.Token(t, "t_alpha", "agent-support", "orders:read", "orders:write", "crm:read"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	refund := &mcp.CallToolParams{Name: "orders.refund_order", Arguments: map[string]any{"order_id": "O-00007", "amount_cents": 500, "reason": "metrics"},
		Meta: mcp.Meta{"idempotency_key": "metrics-test-0001"}}
	for range 2 { // the second call is a replay
		if res, err := sess.CallTool(t.Context(), refund); err != nil || res.IsError {
			t.Fatalf("refund: %v %+v", err, res)
		}
	}
	if _, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "crm.get_customer", Arguments: map[string]any{"customer_id": "C-0042"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	st.App.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`mcpgw_tool_calls_total{server="orders",status="ok",tenant="t_alpha",tool="refund_order"} 1`,
		`mcpgw_tool_calls_total{server="orders",status="replayed",tenant="t_alpha",tool="refund_order"} 1`,
		`mcpgw_idempotency_replays_total 1`,
		`mcpgw_policy_decisions_total{decision="allow"}`,
		`mcpgw_injection_flags_total{server="crm"} 1`,
		`mcpgw_overhead_seconds_bucket`,
		`mcpgw_audit_buffer_depth`, `mcpgw_audit_buffer_capacity 10000`,
		`mcpgw_approvals_pending 0`, `mcpgw_approvals_oldest_pending_seconds 0`,
		`mcpgw_upstream_up{upstream="orders"} 1`,
		`mcpgw_policy_loaded 1`, `mcpgw_policy_stale 0`, `mcpgw_policy_reload_failures_total 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, banned := range []string{"token", "cost_usd"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "mcpgw_") && strings.Contains(strings.SplitN(line, "{", 2)[0], banned) {
				t.Errorf("model-style metric %q (the gateway calls no model)", line)
			}
		}
	}
}
