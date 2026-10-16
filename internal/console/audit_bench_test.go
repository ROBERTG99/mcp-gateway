//go:build integration

package console_test

import (
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/pgtest"
)

// auditRows is the NFR-8 table size.
const auditRows = 100_000

// seedAuditRows bulk-inserts n synthetic events (two tenants, three tools, mixed statuses). The
// rows do not form a valid hash chain; the benchmark measures reads only.
const seedAuditRows = `
INSERT INTO audit_events (seq, chain_pos, ts, tenant, agent, server, tool, args_redacted, args_sha256,
  decision, approval_id, idempotency_key, status, latency_ms, prev_hash, hash)
SELECT i, i, timestamptz '2026-10-01 00:00:00+00' + i * interval '1 second',
  CASE WHEN i % 3 = 0 THEN 't_beta' ELSE 't_alpha' END,
  'agent-' || (i % 7),
  (ARRAY['orders', 'crm', 'ledger'])[1 + i % 3],
  (ARRAY['refund_order', 'get_customer', 'list_invoices'])[1 + i % 3],
  jsonb_build_object('customer_id', 'C-' || lpad((i % 5000)::text, 4, '0'), 'card', '[REDACTED:CARD]'),
  md5(i::text) || md5((i + 1)::text),
  (ARRAY['allow', 'allow', 'deny', 'require_approval'])[1 + i % 4],
  NULL, NULL,
  (ARRAY['ok', 'ok', 'denied', 'held', 'error'])[1 + i % 5],
  (i % 50) / 10.0,
  md5((i - 1)::text) || md5(i::text), md5(i::text) || md5((i + 2)::text)
FROM generate_series(1, $1::bigint) AS i`

// BenchmarkAuditPage renders the console audit list over 100,000 Postgres rows and reports the
// p95 page latency (NFR-8: p95 < 300 ms). Each iteration requests one of several typical views:
// the first page, filtered pages, a tenant-scoped page and a deep keyset page.
func BenchmarkAuditPage(b *testing.B) {
	ctx := b.Context()
	db := pgtest.New(b)
	if err := store.Migrate(ctx, db.URL, "up", io.Discard); err != nil {
		b.Fatal(err)
	}
	admin, err := store.Open(ctx, db.URL)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := admin.Exec(ctx, seedAuditRows, auditRows); err != nil {
		b.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "ANALYZE audit_events"); err != nil {
		b.Fatal(err)
	}
	admin.Close()
	if err := store.SetRolePasswords(ctx, db.URL, map[string]string{"mcpgw_readonly": "pw"}); err != nil {
		b.Fatal(err)
	}
	pool, err := store.Open(ctx, db.RoleURL("mcpgw_readonly", "pw"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)

	e := newEnvWith(b, audit.NewPostgresReader(pool))
	reviewer, approver := client(b), client(b)
	signIn(b, e, reviewer, "security-reviewer")
	signIn(b, e, approver, "approver")
	views := []struct {
		name  string
		query url.Values
		as    string
	}{
		{"first page", nil, "reviewer"},
		{"status", url.Values{"status": {"denied"}}, "reviewer"},
		{"tool", url.Values{"tool": {"orders.refund_order"}}, "reviewer"},
		{"agent and time", url.Values{"agent": {"agent-3"}, "from": {"2026-10-01T06:00"}, "to": {"2026-10-01T12:00"}}, "reviewer"},
		{"tenant scoped", nil, "approver"},
		{"deep page", url.Values{"cursor": {"1000"}}, "reviewer"},
		{"rare filter", url.Values{"tool": {"crm.get_customer"}, "status": {"held"}, "limit": {"200"}}, "reviewer"},
	}
	var samples []time.Duration
	for i := 0; b.Loop(); i++ {
		v := views[i%len(views)]
		c := reviewer
		if v.as == "approver" {
			c = approver
		}
		start := time.Now()
		resp, body := get(b, c, e.srv.URL+"/console/audit?"+v.query.Encode())
		samples = append(samples, time.Since(start))
		if resp.StatusCode != 200 || !strings.Contains(body, `class="row"`) {
			b.Fatalf("%s: %d %s", v.name, resp.StatusCode, body)
		}
	}
	b.StopTimer()
	slices.Sort(samples)
	p95 := samples[(len(samples)*95+99)/100-1]
	b.ReportMetric(float64(p95.Microseconds())/1000, "p95-ms")
	if p95 >= 300*time.Millisecond {
		b.Fatalf("NFR-8: p95 %v with %d audit rows, want < 300ms", p95, auditRows)
	}
}
