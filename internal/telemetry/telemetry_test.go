package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
)

func TestLogsCarryTraceID(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	var buf bytes.Buffer
	l := slog.New(telemetry.TraceHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	l.InfoContext(ctx, "hello", "request_id", "r-1")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() || line["span_id"] == "" {
		t.Fatalf("log line = %v", line)
	}
	buf.Reset()
	l.Info("no span")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) {
		t.Fatalf("trace_id without a span: %s", buf.Bytes())
	}
}

// TestStepRecord: one "step" line per call with the shared step-record fields, kind "tool", no
// model, zero tokens and cost.
func TestStepRecord(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	telemetry.LogStep(context.Background(), l, telemetry.StepRecord{RunID: "run-1", TaskID: "req-1", Node: "gw-1", Kind: "tool",
		Status: "ok", LatencyMS: 3.2, StartedAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)})
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_id", "task_id", "step", "node", "kind", "model", "prompt_version", "input_tokens",
		"output_tokens", "cost_usd", "latency_ms", "status", "error", "started_at"} {
		if _, ok := line[k]; !ok {
			t.Errorf("step record lacks %s: %v", k, line)
		}
	}
	if line["msg"] != "step" || line["kind"] != "tool" || line["model"] != nil || line["cost_usd"] != 0.0 || line["input_tokens"] != 0.0 {
		t.Fatalf("step record = %v", line)
	}
}

func TestMetaPropagation(t *testing.T) {
	telemetry.SetPropagators()
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	meta := map[string]any{}
	telemetry.InjectMeta(ctx, meta)
	if _, ok := meta["traceparent"].(string); !ok {
		t.Fatalf("meta = %v", meta)
	}
	got := telemetry.ExtractMeta(context.Background(), meta)
	_, child := tp.Tracer("test").Start(got, "child")
	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Fatal("trace id not carried through _meta")
	}
	child.End()
}

func TestOTLPTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://jaeger:4318":          "http://jaeger:4318/v1/traces",
		"http://jaeger:4318/":         "http://jaeger:4318/v1/traces",
		"https://otel.example/custom": "https://otel.example/custom",
		"otel-collector:4318":         "",
		"":                            "",
	} {
		if got := telemetry.OTLPTracesURL(in); got != want {
			t.Errorf("OTLPTracesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
