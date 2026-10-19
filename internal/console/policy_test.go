package console_test

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPolicyTesterShowsMatchedRules (FR-4, FR-12): the built-in examples evaluate against the
// loaded bundle to allow, hold and deny, each with the rules that matched.
func TestPolicyTesterShowsMatchedRules(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	signIn(t, e, c, "platform-engineer")
	resp, body := get(t, c, e.srv.URL+"/console/policy")
	if resp.StatusCode != 200 || !strings.Contains(body, `<label for="input"`) || !strings.Contains(body, "Large refund") {
		t.Fatalf("policy page: %d %s", resp.StatusCode, body)
	}
	csrf := csrfOf(t, body)
	for _, tc := range []struct{ example, decision, rule string }{
		{"refund-small", "allow", "scope_granted"},
		{"refund-large", "hold", "refund_requires_approval"},
		{"refund-over-limit", "deny", "refund_over_limit"},
		{"read-without-scope", "deny", "missing_scope"},
		{"tenant-without-upstream", "deny", "tenant_not_allowed"},
		{"refund-large-approved", "allow", "approval_granted"},
	} {
		_, page := get(t, c, e.srv.URL+"/console/policy?example="+tc.example)
		input := textarea(t, page, "input")
		resp, body := post(t, c, e.srv.URL+"/console/policy", url.Values{"csrf": {csrf}, "input": {input}})
		if resp.StatusCode != 200 || !strings.Contains(body, `role="status"`) {
			t.Fatalf("%s: %d %s", tc.example, resp.StatusCode, body)
		}
		if !strings.Contains(body, `class="status `+tc.decision+`"`) || !strings.Contains(body, "<li class=\"mono\">"+tc.rule+"</li>") {
			t.Errorf("%s: want %s with rule %s: %s", tc.example, tc.decision, tc.rule, body)
		}
		if textarea(t, body, "input") != input {
			t.Errorf("%s: the evaluated input is not kept in the form", tc.example)
		}
	}
	resp, body = post(t, c, e.srv.URL+"/console/policy", url.Values{"csrf": {csrf}, "input": {`{"tenant": `}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("invalid input: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, c, e.srv.URL+"/console/policy?example=nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown example: %d", resp.StatusCode)
	}
}

func TestUpstreamsPage(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	signIn(t, e, c, "security-reviewer")
	_, body := get(t, c, e.srv.URL+"/console/upstreams")
	for _, want := range []string{"orders", "orders.refund_order", `class="status up"`, `class="status down"`, `class="status open"`,
		"connection refused", "http://upstream-orders:8215/mcp", `<span class="chip">untrusted output</span> dial tcp`} {
		if !strings.Contains(body, want) {
			t.Fatalf("upstreams page lacks %q: %s", want, body)
		}
	}
	if strings.Count(body, `class="row"`) != 2 {
		t.Fatalf("rows: %s", body)
	}
}

// textarea returns the unescaped content of the textarea with the given id.
func textarea(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `<textarea id="`+id+`"`)
	if start < 0 {
		t.Fatalf("no textarea %s", id)
	}
	open := strings.Index(body[start:], ">") + start + 1
	end := strings.Index(body[open:], "</textarea>") + open
	return html.UnescapeString(strings.TrimPrefix(body[open:end], "\n"))
}
