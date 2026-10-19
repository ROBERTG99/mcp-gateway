package console_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
)

func holdApproval(t *testing.T, e env, tenant, key string) approvals.Approval {
	t.Helper()
	a, err := e.svc.Hold(context.Background(), approvals.HoldRequest{Binding: approvals.Binding{Tenant: tenant, Agent: "agent-support",
		Server: "orders", Tool: "refund_order", ArgsSHA256: strings.Repeat("b", 64), IdempotencyKey: key},
		Args:    json.RawMessage(`{"order_id":"O-00042","amount_cents":75000,"reason":"card 4111 1111 1111 1111 charged twice"}`),
		Reasons: []string{"refund_requires_approval"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// csrfOf returns the session CSRF token rendered in a page (the sign-out form carries it).
func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("page has no CSRF token")
	}
	return m[1]
}

func status(t *testing.T, e env, id string) string {
	t.Helper()
	a, err := e.svc.Get(context.Background(), id, authn.Principal{Tenant: "*"})
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

// TestConsoleApprovalsRBAC (FR-12): the approver of t_alpha approves and rejects held calls from
// the console, decisions are audited, and the security reviewer sees the queue but gets no
// buttons and a 403 when posting a decision anyway.
func TestConsoleApprovalsRBAC(t *testing.T) {
	e := newEnv(t)
	held := holdApproval(t, e, "t_alpha", "refund-key-0001")
	toReject := holdApproval(t, e, "t_alpha", "refund-key-0002")
	beta := holdApproval(t, e, "t_beta", "refund-key-0003")

	reviewer := client(t)
	signIn(t, e, reviewer, "security-reviewer")
	_, body := get(t, reviewer, e.srv.URL+"/console/approvals")
	if strings.Count(body, `class="row"`) != 3 || !strings.Contains(body, "orders.refund_order") {
		t.Fatalf("reviewer queue: %s", body)
	}
	resp, body := get(t, reviewer, e.srv.URL+"/console/approvals/"+held.ID)
	if resp.StatusCode != 200 || strings.Contains(body, ">Approve<") || strings.Contains(body, ">Reject<") {
		t.Fatalf("reviewer detail must have no decision buttons: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "[REDACTED:CARD]") || strings.Contains(body, "4111") || !strings.Contains(body, held.ArgsSHA256) ||
		!strings.Contains(body, "refund-key-0001") || !strings.Contains(body, "refund_requires_approval") ||
		!strings.Contains(body, "What will execute") {
		t.Fatalf("detail lacks tool, redacted args, hash, key, reasons or the execution summary: %s", body)
	}
	resp, _ = post(t, reviewer, e.srv.URL+"/console/approvals/"+held.ID+"/approve", url.Values{"csrf": {csrfOf(t, body)}})
	if resp.StatusCode != http.StatusForbidden || status(t, e, held.ID) != approvals.StatusPending {
		t.Fatalf("reviewer approve: %d, status %s", resp.StatusCode, status(t, e, held.ID))
	}
	// Permission is checked before the input, as in the admin API.
	resp, _ = post(t, reviewer, e.srv.URL+"/console/approvals/"+held.ID+"/reject", url.Values{"csrf": {csrfOf(t, body)}, "reason": {strings.Repeat("x", 3000)}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reviewer reject with a long reason: %d, want 403", resp.StatusCode)
	}

	approver := client(t)
	signIn(t, e, approver, "approver")
	_, body = get(t, approver, e.srv.URL+"/console/approvals")
	if strings.Count(body, `class="row"`) != 2 || strings.Contains(body, beta.ID) {
		t.Fatalf("approver queue is tenant-scoped: %s", body)
	}
	if resp, _ := get(t, approver, e.srv.URL+"/console/approvals/"+beta.ID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant's approval: %d", resp.StatusCode)
	}
	_, body = get(t, approver, e.srv.URL+"/console/approvals/"+held.ID)
	for _, want := range []string{">Approve<", ">Reject<", `<label for="note"`, `<label for="reason"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("approver detail lacks %s: %s", want, body)
		}
	}
	csrf := csrfOf(t, body)
	if resp, _ := post(t, approver, e.srv.URL+"/console/approvals/"+held.ID+"/approve", url.Values{"note": {"ok"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("approve without CSRF: %d", resp.StatusCode)
	}
	if resp, _ := post(t, approver, e.srv.URL+"/console/approvals/"+beta.ID+"/approve", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("approve other tenant: %d", resp.StatusCode)
	}
	// A rejection needs a reason.
	resp, body = post(t, approver, e.srv.URL+"/console/approvals/"+toReject.ID+"/reject", url.Values{"csrf": {csrf}, "reason": {""}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `role="alert"`) || status(t, e, toReject.ID) != approvals.StatusPending {
		t.Fatalf("reject without reason: %d %s", resp.StatusCode, body)
	}
	resp, _ = post(t, approver, e.srv.URL+"/console/approvals/"+toReject.ID+"/reject", url.Values{"csrf": {csrf}, "reason": {"duplicate request"}})
	if resp.StatusCode != http.StatusSeeOther || status(t, e, toReject.ID) != approvals.StatusRejected {
		t.Fatalf("reject: %d, status %s", resp.StatusCode, status(t, e, toReject.ID))
	}
	resp, _ = post(t, approver, e.srv.URL+"/console/approvals/"+held.ID+"/approve", url.Values{"csrf": {csrf}, "note": {"verified with the customer"}})
	if resp.StatusCode != http.StatusSeeOther || status(t, e, held.ID) != approvals.StatusApproved {
		t.Fatalf("approve: %d, status %s", resp.StatusCode, status(t, e, held.ID))
	}
	resp, body = get(t, approver, e.srv.URL+resp.Header.Get("Location"))
	if resp.StatusCode != 200 || !strings.Contains(body, `role="status"`) || !strings.Contains(body, "approved") || strings.Contains(body, ">Approve<") {
		t.Fatalf("after approval: %d %s", resp.StatusCode, body)
	}
	// Deciding twice is a conflict, shown as an error banner.
	resp, body = post(t, approver, e.srv.URL+"/console/approvals/"+held.ID+"/approve", url.Values{"csrf": {csrf}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("second decision: %d %s", resp.StatusCode, body)
	}
	// The pending queue no longer lists decided approvals; the "all" view does.
	_, body = get(t, approver, e.srv.URL+"/console/approvals")
	if strings.Count(body, `class="row"`) != 0 || !strings.Contains(body, "No approvals are waiting") {
		t.Fatalf("pending queue after decisions: %s", body)
	}
	_, body = get(t, approver, e.srv.URL+"/console/approvals?status=all")
	if strings.Count(body, `class="row"`) != 2 {
		t.Fatalf("all approvals: %s", body)
	}
	// Both decisions are audited with the approver as agent.
	if err := e.writer.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, ev := range e.sink.Events() {
		tools[ev.Tool] = ev.Agent
	}
	if tools["approval.approve"] != "jo.approver" || tools["approval.reject"] != "jo.approver" {
		t.Fatalf("decision audit events: %v", tools)
	}
}

// TestTemplatesHaveNoInlineCode: the CSP allows only same-origin scripts and styles, so inline
// style attributes, <style> blocks, inline scripts and event handlers would be silently ignored.
func TestTemplatesHaveNoInlineCode(t *testing.T) {
	bad := regexp.MustCompile(`(?i)\sstyle=|<style|<script>|\son[a-z]+=`)
	files, _ := filepath.Glob("templates/*.html")
	if len(files) == 0 {
		t.Fatal("no templates found")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := bad.FindIndex(b); loc != nil {
			t.Errorf("%s: inline code %q", f, b[loc[0]:min(loc[1]+20, len(b))])
		}
	}
}
