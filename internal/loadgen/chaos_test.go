//go:build integration

package loadgen_test

import (
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/loadgen"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// TestChaosSmall (FR-5, FR-7): with 10% of refund keys stalled past the gateway timeout, client
// retries and 20% deliberate re-sends, every key executes exactly once and every call is audited.
func TestChaosSmall(t *testing.T) {
	// The breaker cooldown is shortened so the small run is not dominated by 30 s waits; the
	// breaker still opens and rejects calls during the run.
	st := stack.Start(t, stack.Options{FaultRate: 0.1, Stall: 1500 * time.Millisecond, UpstreamTimeout: time.Second,
		Configure: func(c *config.Config) { c.BreakerCooldown = 2 * time.Second }})
	res, err := loadgen.Chaos(t.Context(), loadgen.ChaosConfig{GatewayURL: st.MCPURL, IssuerURL: st.IssuerURL, AdminURL: st.AdminURL,
		Calls: 300, Concurrency: 16, Sessions: 4, Orders: 30, RunID: "small", SettleWait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
	if res.Duplicates != 0 || res.Lost != 0 || res.Unanswered != 0 {
		t.Fatalf("duplicates %d, lost %d, unanswered %d", res.Duplicates, res.Lost, res.Unanswered)
	}
	if res.AuditedRatio != 1 || !res.ChainOK {
		t.Fatalf("audited ratio %.4f (events %d), chain ok %v", res.AuditedRatio, res.AuditEvents, res.ChainOK)
	}
	if res.Timeouts == 0 || res.Replayed == 0 || res.DeliberateDups != 60 || res.OtherErrors != 0 {
		t.Fatalf("the run did not exercise the faults: %+v", res)
	}
}
