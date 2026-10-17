package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
)

// echoUpstream answers in-process with the arguments it received and records how long it took,
// so the benchmark can subtract upstream time from the total (NFR-1).
type echoUpstream struct{ last time.Duration }

func (e *echoUpstream) Call(_ context.Context, _ catalog.Tool, args json.RawMessage, _ mcp.Meta) (*mcp.CallToolResult, error) {
	start := time.Now()
	var v map[string]any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil, fmt.Errorf("echo: %w", err)
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Customer record for " + fmt.Sprint(v["customer_id"], v["order_id"]) +
		". Last contact by email; account in good standing."}}, StructuredContent: v}
	e.last = time.Since(start)
	return res, nil
}

// BenchmarkGatewayOverhead measures the gateway's own time per tools/call (total minus upstream)
// through every stage: argument limits and schema, rate limit, compiled policy from policies/,
// idempotency (memory store), audit reservation and enqueue (writer running), injection scoring,
// output marking, redaction, metrics and the step record. The mix is 80% reads and 20% writes
// with fresh idempotency keys. It reports overhead p50 and p95 in ms (NFR-1: p95 < 5 ms; budget
// checked by scripts/bench_budget.py).
func BenchmarkGatewayOverhead(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := policy.New(ctx, policy.Options{Dir: "../../policies"})
	if err != nil {
		b.Fatal(err)
	}
	w := audit.NewWriter(audit.NewMemorySink(), audit.NewMemorySeq(), audit.Options{})
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	b.Cleanup(func() { cancel(); <-done })

	c := clock.Real{}
	unlimited := map[string]config.Tenant{"t_alpha": {RatePerSecond: 1e9, Burst: 1 << 30, AgentRatePerSecond: 1e9, AgentBurst: 1 << 30,
		DailyBudget: 1 << 40}}
	up := &echoUpstream{}
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: engine, Idempotency: idempotency.NewMemory(c), Audit: w,
		Redactor: redact.New(), Scorer: injection.New(injection.Options{}), Limiter: ratelimit.NewMemory(unlimited, c),
		Metrics: telemetry.NewMetrics(telemetry.Probes{}), Clock: c}, pipeline.Options{Node: "bench"})
	who := authn.Principal{Tenant: "t_alpha", Agent: "agent-support", Scopes: []string{"orders:write", "crm:read"}}
	read := catalog.Tool{Server: "crm", Name: "get_customer", Def: &mcp.Tool{Name: "crm.get_customer",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: map[string]any{"type": "object", "required": []any{"customer_id"},
			"properties": map[string]any{"customer_id": map[string]any{"type": "string"}}}}}

	overhead := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		var call proxy.Call
		if i%5 == 4 {
			raw, _ := json.Marshal(map[string]any{"order_id": fmt.Sprintf("O-%05d", i%1000), "amount_cents": 2500, "reason": "damaged item"})
			call = proxy.Call{Principal: who, Tool: refundTool, Request: &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
				Name: "orders.refund_order", Arguments: raw, Meta: mcp.Meta{"idempotency_key": fmt.Sprintf("bench-key-%010d", i)}}}}
		} else {
			raw, _ := json.Marshal(map[string]any{"customer_id": fmt.Sprintf("C-%04d", i%5000)})
			call = proxy.Call{Principal: who, Tool: read, Request: &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
				Name: "crm.get_customer", Arguments: raw}}}
		}
		up.last = 0
		start := time.Now()
		res, err := p.CallTool(ctx, call)
		total := time.Since(start)
		if err != nil || res.IsError {
			b.Fatalf("call %d: %v", i, err)
		}
		overhead = append(overhead, total-up.last)
	}
	b.StopTimer()
	slices.Sort(overhead)
	pct := func(q float64) float64 {
		return float64(overhead[int(q*float64(len(overhead)-1))].Microseconds()) / 1000
	}
	b.ReportMetric(pct(0.50), "overhead-p50-ms")
	b.ReportMetric(pct(0.95), "overhead-p95-ms")
}
