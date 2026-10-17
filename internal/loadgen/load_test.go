//go:build integration

package loadgen_test

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/loadgen"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// TestLoadSmall (NFR-2, NFR-7 harness): a short load run against the in-process stack answers
// every call, keeps the 80/20 read/write mix and samples RSS from the metrics endpoint.
func TestLoadSmall(t *testing.T) {
	st := stack.Start(t, stack.Options{Configure: func(c *config.Config) {
		c.TenantsFile = filepath.Join(stack.Root(), "config", "tenants.loadtest.yaml")
	}})
	metrics := httptest.NewServer(st.App.MetricsHandler())
	t.Cleanup(metrics.Close)
	res, err := loadgen.Load(t.Context(), loadgen.LoadConfig{GatewayURL: st.MCPURL, IssuerURL: st.IssuerURL, MetricsURL: metrics.URL + "/metrics",
		Duration: 2 * time.Second, Warmup: 500 * time.Millisecond, Concurrency: 8, Sessions: 4, RunID: "small"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
	if res.Calls < 100 || res.Errors != 0 || res.FirstError != "" {
		t.Fatalf("calls %d, errors %d (%s)", res.Calls, res.Errors, res.FirstError)
	}
	if share := float64(res.Writes) / float64(res.Calls); share < 0.1 || share > 0.3 {
		t.Fatalf("write share %.2f, want about 0.2", share)
	}
	if res.RSSSamples == 0 || res.RSSMaxMB <= 0 || res.CallsPerSecond <= 0 {
		t.Fatalf("rss %v MB over %d samples, %v calls/s", res.RSSMaxMB, res.RSSSamples, res.CallsPerSecond)
	}
}
