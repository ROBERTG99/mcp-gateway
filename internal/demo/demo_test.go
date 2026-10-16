//go:build integration

package demo_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

func TestDemoScenarioSteps(t *testing.T) {
	st := stack.Start(t, stack.Options{UpstreamTimeout: time.Second, Stall: 2 * time.Second})
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results, err := demo.Run(ctx, demo.Config{
		GatewayURL: st.MCPURL, AdminURL: st.AdminURL, IssuerURL: st.IssuerURL, WebhookAdminURL: st.SinkURL,
		StallPrefix: "demo-stall-", Out: &out, RunID: "test",
	})
	t.Log("\n" + out.String())
	if err != nil {
		t.Fatalf("scenario: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("results = %d, want 6", len(results))
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("step %d (%s) failed: %s", r.Step, r.Name, r.Detail)
		}
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(out.String(), "PASS step "+string(rune('0'+i))) {
			t.Fatalf("output lacks PASS step %d", i)
		}
	}
	// A second run (new keys) passes too: the scenario is repeatable on the same data.
	results, err = demo.Run(ctx, demo.Config{GatewayURL: st.MCPURL, AdminURL: st.AdminURL, IssuerURL: st.IssuerURL,
		StallPrefix: "demo-stall-", Out: &out, RunID: "test2"})
	if err != nil {
		t.Fatalf("second run: %v (%+v)", err, results)
	}
}
