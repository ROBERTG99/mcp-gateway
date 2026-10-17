package telemetry_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
)

// TestDashboardsUseExportedMetrics: every mcpgw_* series the Grafana dashboard and the alert
// rules query is a metric the gateway exports (no dangling panels or alerts).
func TestDashboardsUseExportedMetrics(t *testing.T) {
	m := telemetry.NewMetrics(telemetry.Probes{AuditDepth: func() int { return 0 }, AuditCapacity: func() int { return 1 },
		PolicyLoaded: func() bool { return true }, Upstreams: func() map[string]bool { return nil },
		PendingApproval: nil})
	descs := make(chan *prometheus.Desc, 64)
	go func() { m.Registry().Describe(descs); close(descs) }()
	fq := regexp.MustCompile(`fqName: "([a-z_]+)"`)
	exported := map[string]bool{"mcpgw_approvals_pending": true, "mcpgw_approvals_oldest_pending_seconds": true, "mcpgw_upstream_up": true}
	for d := range descs {
		if m := fq.FindStringSubmatch(d.String()); m != nil {
			exported[m[1]] = true
		}
	}
	used := regexp.MustCompile(`mcpgw_[a-z_]+`)
	for _, f := range []string{"../../deploy/grafana/dashboards/mcp-gateway.json", "../../deploy/prometheus/alerts.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		names := used.FindAllString(string(b), -1)
		slices.Sort(names)
		for _, n := range slices.Compact(names) {
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum"), "_count")
			if !exported[base] {
				t.Errorf("%s queries %s, which the gateway does not export", f, n)
			}
		}
	}
}
