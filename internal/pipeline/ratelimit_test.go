package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
)

type downLimiter struct{}

func (downLimiter) Allow(context.Context, string, string) (ratelimit.Verdict, error) {
	return ratelimit.Verdict{}, errors.New("redis: connection refused")
}

func TestRateLimitStage(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	tenants := map[string]config.Tenant{"t_alpha": {RatePerSecond: 1, Burst: 1, DailyBudget: 2}}
	up := &fakeUpstream{}
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{}, Clock: c, Limiter: ratelimit.NewMemory(tenants, c)}, pipeline.Options{})
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil)
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != mcperr.CodeRateLimited {
		t.Fatalf("err = %v, want -32042", err)
	}
	var data map[string]any
	_ = json.Unmarshal(je.Data, &data)
	if data["retry_after_ms"] != float64(1000) {
		t.Fatalf("data = %s", je.Data)
	}
	c.Advance(time.Second)
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Second)
	_, err = call(p, readTool, map[string]any{"customer_id": "C-1"}, nil)
	if !errors.As(err, &je) || je.Code != mcperr.CodeBudgetExhausted {
		t.Fatalf("err = %v, want -32043", err)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d", up.calls.Load())
	}
}

func TestRateLimiterDownFailsClosedForWrites(t *testing.T) {
	up := &fakeUpstream{}
	c := clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	local := ratelimit.NewMemory(map[string]config.Tenant{"t_alpha": {RatePerSecond: 1, Burst: 1, DailyBudget: 10}}, c)
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{}, Clock: c, Limiter: downLimiter{}, LocalLimiter: local,
		Idempotency: idempotency.NewMemory(c)}, pipeline.Options{})
	_, err := call(p, refundTool, refundArgs, mcp.Meta{"idempotency_key": "refund-key-0100"})
	if code(t, err) != mcperr.CodeDependencyUnavailable {
		t.Fatalf("write with limiter down: %v", err)
	}
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); err != nil {
		t.Fatalf("read with limiter down must use the local bucket: %v", err)
	}
	if _, err := call(p, readTool, map[string]any{"customer_id": "C-1"}, nil); code(t, err) != mcperr.CodeRateLimited {
		t.Fatalf("local bucket not enforced: %v", err)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
}
