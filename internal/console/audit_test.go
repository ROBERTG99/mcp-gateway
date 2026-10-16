package console_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
)

func seedAudit(t *testing.T, e env) {
	t.Helper()
	ctx := context.Background()
	add := func(tenant, server, tool, decision, status string) {
		tk, err := e.writer.Reserve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tk.Commit(audit.Event{TS: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), Tenant: tenant, Agent: "agent-support", Server: server,
			Tool: tool, ArgsRedacted: json.RawMessage(`{"customer_id":"C-0042","card":"[REDACTED:CARD]"}`),
			ArgsSHA256: strings.Repeat("a", 64), Decision: decision, Status: status, LatencyMS: 2.5})
	}
	add("t_alpha", "orders", "refund_order", "allow", "ok")
	add("t_alpha", "crm", "get_customer", "deny", "denied")
	add("t_beta", "crm", "get_customer", "allow", "ok")
	if err := e.writer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuditPageFiltersAndEmptyState(t *testing.T) {
	e := newEnv(t)
	seedAudit(t, e)
	reviewer := client(t)
	signIn(t, e, reviewer, "security-reviewer")
	_, body := get(t, reviewer, e.srv.URL+"/console/audit")
	if strings.Count(body, `class="row"`) != 3 || !strings.Contains(body, "orders.refund_order") || !strings.Contains(body, "t_beta") {
		t.Fatalf("reviewer list: %s", body)
	}
	for _, want := range []string{`<label for="f-status"`, `<label for="f-tool"`, `aria-label="Audit filters"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("filters form lacks %s", want)
		}
	}
	_, body = get(t, reviewer, e.srv.URL+"/console/audit?status=denied")
	if strings.Count(body, `class="row"`) != 1 || !strings.Contains(body, "crm.get_customer") {
		t.Fatalf("status filter: %s", body)
	}
	_, body = get(t, reviewer, e.srv.URL+"/console/audit?tool=nope.nothing")
	if !strings.Contains(body, "No audit events match") || strings.Contains(body, `class="row"`) {
		t.Fatalf("empty state: %s", body)
	}
	_, body = get(t, reviewer, e.srv.URL+"/console/audit?from=2099-01-01T00:00")
	if !strings.Contains(body, "No audit events match") {
		t.Fatal("from filter (datetime-local) not applied")
	}
	_, body = get(t, reviewer, e.srv.URL+"/console/audit?limit=bogus")
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "Retry") {
		t.Fatalf("error banner: %s", body)
	}
	_, body = get(t, reviewer, e.srv.URL+"/console/audit?verify=1")
	if !strings.Contains(body, `role="status"`) || !strings.Contains(body, "Chain verified: 3 events") {
		t.Fatalf("verify: %s", body)
	}
	// The approver of t_alpha sees only t_alpha.
	approver := client(t)
	signIn(t, e, approver, "approver")
	_, body = get(t, approver, e.srv.URL+"/console/audit")
	if strings.Count(body, `class="row"`) != 2 || strings.Contains(body, "t_beta") {
		t.Fatalf("tenant scoping: %s", body)
	}
	// Chain verification spans every tenant: no button and no result for a tenant-scoped viewer.
	_, body = get(t, approver, e.srv.URL+"/console/audit?verify=1")
	if strings.Contains(body, "Verify chain") || strings.Contains(body, "Chain verified") {
		t.Fatalf("tenant-scoped verify: %s", body)
	}
	// Event detail: all fields, monospace hashes, redacted args, other tenants hidden.
	resp, body := get(t, reviewer, e.srv.URL+"/console/audit/1")
	if resp.StatusCode != 200 || !strings.Contains(body, `class="mono hash"`) || !strings.Contains(body, "[REDACTED:CARD]") ||
		!strings.Contains(body, "prev_hash") || !strings.Contains(body, "args_sha256") {
		t.Fatalf("event page: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, approver, e.srv.URL+"/console/audit/3"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant's event: %d", resp.StatusCode)
	}
	if resp, _ := get(t, reviewer, e.srv.URL+"/console/audit/999"); resp.StatusCode != http.StatusNotFound {
		t.Fatal("missing event")
	}
}

func TestAuditPagePagination(t *testing.T) {
	e := newEnv(t)
	for range 4 {
		seedAudit(t, e) // 12 events
	}
	c := client(t)
	signIn(t, e, c, "security-reviewer")
	_, body := get(t, c, e.srv.URL+"/console/audit?limit=5")
	if strings.Count(body, `class="row"`) != 5 || !strings.Contains(body, "Older events") {
		t.Fatalf("first page: %s", body)
	}
	i := strings.Index(body, "cursor=")
	cursor := body[i+7 : i+7+strings.IndexAny(body[i+7:], `&"`)]
	_, body = get(t, c, fmt.Sprintf("%s/console/audit?limit=5&cursor=%s", e.srv.URL, cursor))
	if strings.Count(body, `class="row"`) != 5 {
		t.Fatalf("second page: %s", body)
	}
}
