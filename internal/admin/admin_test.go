package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

type fixture struct {
	h      http.Handler
	issuer *devissuer.Server
	svc    *approvals.Service
	held   approvals.Approval
	writer *audit.Writer
}

type staticCatalog struct{}

func (staticCatalog) Status() []catalog.UpstreamStatus {
	return []catalog.UpstreamStatus{{Name: "orders", URL: "http://upstream-orders:8215/mcp", Up: true, Breaker: catalog.BreakerClosed,
		Tools: []string{"orders.get_order", "orders.refund_order"}}}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	iss, err := devissuer.New(devissuer.Options{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(iss.Handler())
	t.Cleanup(jwks.Close)
	verifier := authn.NewVerifier(authn.Options{JWKSURL: jwks.URL + "/.well-known/jwks.json", Issuer: devissuer.Issuer, Audience: devissuer.Audience})
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{FlushInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for _, tenant := range []string{"t_alpha", "t_beta"} {
		tk, _ := w.Reserve(ctx)
		tk.Commit(audit.Event{TS: time.Now(), Tenant: tenant, Agent: "agent-support", Server: "crm", Tool: "get_customer",
			ArgsRedacted: json.RawMessage(`{}`), ArgsSHA256: strings.Repeat("a", 64), Decision: "allow", Status: "ok"})
	}
	_ = w.Flush(ctx)
	svc := approvals.NewService(approvals.NewMemory(), approvals.Options{Audit: w})
	held, err := svc.Hold(ctx, approvals.HoldRequest{Binding: approvals.Binding{Tenant: "t_alpha", Agent: "agent-support",
		Server: "orders", Tool: "refund_order", ArgsSHA256: strings.Repeat("b", 64), IdempotencyKey: "k-00000001"},
		Args: json.RawMessage(`{"amount_cents":50000}`), Reasons: []string{"refund_requires_approval"}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(ctx, policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	h := admin.NewRouter(admin.Deps{Verifier: verifier, Audit: audit.MemoryReader{Sink: sink}, Approvals: svc, Policy: engine,
		Catalog: staticCatalog{}, Flush: w.Flush})
	return fixture{h: h, issuer: iss, svc: svc, held: held, writer: w}
}

func (f fixture) token(t *testing.T, tenant, sub string, scopes ...string) string {
	t.Helper()
	tok, _, err := f.issuer.Mint(tenant, sub, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f fixture) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func TestAdminRBAC(t *testing.T) {
	f := newFixture(t)
	admin := f.token(t, "*", "alex.platform", "mcpgw:admin", "mcpgw:audit:read")
	reviewer := f.token(t, "*", "sam.security", "mcpgw:audit:read")
	approver := f.token(t, "t_alpha", "jo.approver", "mcpgw:approve")
	betaApprover := f.token(t, "t_beta", "kim.approver", "mcpgw:approve")
	agent := f.token(t, "t_alpha", "agent-support", "orders:write")
	approve := "/admin/approvals/" + f.held.ID + ":approve"

	cases := []struct {
		name, method, path, token string
		body                      any
		want                      int
	}{
		{"no token", http.MethodGet, "/admin/audit", "", nil, 401},
		{"agent token on audit", http.MethodGet, "/admin/audit", agent, nil, 403},
		{"approver on policy reload", http.MethodPost, "/admin/policy/reload", approver, nil, 403},
		{"reviewer on policy reload", http.MethodPost, "/admin/policy/reload", reviewer, nil, 403},
		{"reviewer approves", http.MethodPost, approve, reviewer, map[string]any{"note": "x"}, 403},
		{"other tenant approves", http.MethodPost, approve, betaApprover, map[string]any{"note": "x"}, 403},
		{"reject without reason", http.MethodPost, "/admin/approvals/" + f.held.ID + ":reject", approver, map[string]any{}, 400},
		{"unknown approval", http.MethodPost, "/admin/approvals/00000000-0000-4000-8000-000000000000:approve", approver, nil, 404},
		{"bad action", http.MethodPost, "/admin/approvals/" + f.held.ID + ":delete", approver, nil, 404},
		{"reviewer reads audit", http.MethodGet, "/admin/audit", reviewer, nil, 200},
		{"admin reads upstreams", http.MethodGet, "/admin/upstreams", admin, nil, 200},
		{"agent reads upstreams", http.MethodGet, "/admin/upstreams", agent, nil, 403},
		{"admin evaluates policy", http.MethodPost, "/admin/policy/evaluate", admin, map[string]any{"tenant": "t_alpha",
			"agent": "a", "scopes": []string{"orders:write"}, "server": "orders", "tool": "refund_order",
			"args": map[string]any{"order_id": "O-1", "amount_cents": 50000}, "annotations": map[string]any{"readOnlyHint": false}}, 200},
		{"admin reloads policy", http.MethodPost, "/admin/policy/reload", admin, nil, 200},
		{"approver approves", http.MethodPost, approve, approver, map[string]any{"note": "ok"}, 200},
		{"second decision", http.MethodPost, approve, approver, map[string]any{"note": "ok"}, 409},
		{"verify chain", http.MethodGet, "/admin/audit/verify", reviewer, nil, 200},
	}
	for _, c := range cases {
		rec := f.do(t, c.method, c.path, c.token, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %s %s = %d, want %d (%s)", c.name, c.method, c.path, rec.Code, c.want, rec.Body.String())
			continue
		}
		if c.want >= 400 && c.want != 401 && rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: error content type %q, want application/problem+json", c.name, rec.Header().Get("Content-Type"))
		}
	}
	// The approve response contract.
	rec := f.do(t, http.MethodGet, "/admin/approvals/"+f.held.ID, approver, nil)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "approved" || got["decided_by"] != "jo.approver" {
		t.Fatalf("approval after decision = %s", rec.Body.String())
	}
}

func TestAdminTenantScoping(t *testing.T) {
	f := newFixture(t)
	alphaApprover := f.token(t, "t_alpha", "jo.approver", "mcpgw:approve", "mcpgw:audit:read")
	betaReader := f.token(t, "t_beta", "kim.reader", "mcpgw:audit:read")
	reviewer := f.token(t, "*", "sam.security", "mcpgw:audit:read")
	count := func(tok, query string) int {
		rec := f.do(t, http.MethodGet, "/admin/audit"+query, tok, nil)
		var page audit.Page
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		return len(page.Events)
	}
	if n := count(reviewer, ""); n != 2 {
		t.Fatalf("cross-tenant reviewer sees %d events, want 2", n)
	}
	if n := count(alphaApprover, ""); n != 1 {
		t.Fatalf("t_alpha viewer sees %d events, want 1", n)
	}
	if n := count(alphaApprover, "?tenant=t_beta"); n != 1 {
		t.Fatalf("t_alpha viewer asking for t_beta sees %d events, want only its own (1)", n)
	}
	if rec := f.do(t, http.MethodGet, "/admin/approvals/"+f.held.ID, betaReader, nil); rec.Code != 404 {
		t.Fatalf("t_beta reading a t_alpha approval: %d, want 404", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/admin/audit?limit=500", reviewer, nil); rec.Code != 400 {
		t.Fatalf("limit 500: %d, want 400", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/admin/audit?cursor=!!", reviewer, nil); rec.Code != 400 {
		t.Fatalf("bad cursor: %d, want 400", rec.Code)
	}
}
