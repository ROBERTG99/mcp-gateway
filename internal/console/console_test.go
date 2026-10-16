package console_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/console"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

type staticCatalog struct{}

func (staticCatalog) Status() []catalog.UpstreamStatus {
	return []catalog.UpstreamStatus{
		{Name: "orders", URL: "http://upstream-orders:8215/mcp", Up: true, Breaker: catalog.BreakerClosed,
			Tools: []string{"orders.get_order", "orders.refund_order"}, LastRefresh: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)},
		{Name: "ledger", URL: "http://upstream-ledger:8213/mcp", Up: false, Breaker: catalog.BreakerOpen,
			LastError: "dial tcp: connection refused", LastRefresh: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)},
	}
}

type env struct {
	srv    *httptest.Server
	issuer *devissuer.Server
	sink   *audit.MemorySink
	writer *audit.Writer
	svc    *approvals.Service
}

func newEnv(t testing.TB) env { return newEnvWith(t, envOpts{}) }

// envOpts adjust the test console: reader replaces the memory audit reader (the benchmark reads a
// seeded Postgres table), personas replace the defaults, clock drives session expiry, and noSignin
// leaves the token source unset.
type envOpts struct {
	reader   audit.Reader
	personas []devissuer.Persona
	clock    clock.Clock
	noSignin bool
}

// newEnvWith builds the console over an in-memory audit writer.
func newEnvWith(t testing.TB, o envOpts) env {
	t.Helper()
	iss, err := devissuer.New(devissuer.Options{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(iss.Handler())
	t.Cleanup(jwks.Close)
	verifier := authn.NewVerifier(authn.Options{JWKSURL: jwks.URL + "/.well-known/jwks.json", Issuer: devissuer.Issuer, Audience: devissuer.Audience})
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	engine, err := policy.New(ctx, policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	svc := approvals.NewService(approvals.NewMemory(), approvals.Options{Audit: w})
	reader := o.reader
	if reader == nil {
		reader = audit.MemoryReader{Sink: sink}
	}
	tokens := console.TokenSource(func(_ context.Context, p devissuer.Persona) (string, error) {
		tok, _, err := iss.Mint(p.Tenant, p.Subject, p.Scopes, 0)
		return tok, err
	})
	if o.noSignin {
		tokens = nil
	}
	c, err := console.New(console.Deps{
		Verifier: verifier, Audit: reader, Approvals: svc, Policy: engine, Catalog: staticCatalog{},
		Flush: w.Flush, Tokens: tokens, Personas: o.personas,
	}, console.Options{Clock: o.clock})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return env{srv: srv, issuer: iss, sink: sink, writer: w, svc: svc}
}

func client(t testing.TB) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func get(t testing.TB, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func post(t testing.TB, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// signIn signs in as persona and returns the page CSRF token.
func signIn(t testing.TB, e env, c *http.Client, persona string) {
	t.Helper()
	_, body := get(t, c, e.srv.URL+"/console/signin")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("sign-in page has no CSRF field: %s", body)
	}
	resp, _ := post(t, c, e.srv.URL+"/console/signin", url.Values{"csrf": {m[1]}, "persona": {persona}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in as %s: %d", persona, resp.StatusCode)
	}
}

func TestConsoleRequiresSession(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	for _, path := range []string{"/console", "/console/audit", "/console/approvals", "/console/policy", "/console/upstreams"} {
		resp, _ := get(t, c, e.srv.URL+path)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/console/signin") {
			t.Fatalf("%s without session: %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	resp, body := get(t, c, e.srv.URL+"/console/signin")
	if resp.StatusCode != 200 || !strings.Contains(body, "Security reviewer") || !strings.Contains(body, "Approver") {
		t.Fatalf("sign-in page: %d", resp.StatusCode)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Errorf("CSP = %q", resp.Header.Get("Content-Security-Policy"))
	}
	signIn(t, e, c, "security-reviewer")
	resp, body = get(t, c, e.srv.URL+"/console/audit")
	if resp.StatusCode != 200 || !strings.Contains(body, "sam.security") {
		t.Fatalf("after sign-in: %d %s", resp.StatusCode, body)
	}
	cookie := resp.Request.Header.Get("Cookie")
	if !strings.Contains(cookie, "mcpgw_session=") {
		t.Fatalf("no session cookie sent: %q", cookie)
	}
	// Sign out invalidates the session.
	m := csrfRe.FindStringSubmatch(body)
	post(t, c, e.srv.URL+"/console/signout", url.Values{"csrf": {m[1]}})
	if resp, _ := get(t, c, e.srv.URL+"/console/audit"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after sign-out: %d", resp.StatusCode)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	_, body := get(t, c, e.srv.URL+"/console/signin")
	m := csrfRe.FindStringSubmatch(body)
	resp, _ := post(t, c, e.srv.URL+"/console/signin", url.Values{"csrf": {m[1]}, "persona": {"approver"}})
	var session *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "mcpgw_session" {
			session = ck
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || session.Path != "/console" {
		t.Fatalf("session cookie = %+v", session)
	}
}

func TestCSRFRejected(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	// Sign-in without (or with a wrong) CSRF token.
	get(t, c, e.srv.URL+"/console/signin")
	for _, form := range []url.Values{{"persona": {"approver"}}, {"persona": {"approver"}, "csrf": {"forged"}}} {
		if resp, _ := post(t, c, e.srv.URL+"/console/signin", form); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("sign-in with %v: %d, want 403", form, resp.StatusCode)
		}
	}
	signIn(t, e, c, "approver")
	for _, form := range []url.Values{{}, {"csrf": {"forged"}}} {
		resp, _ := post(t, c, e.srv.URL+"/console/signout", form)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("sign-out with %v: %d, want 403", form, resp.StatusCode)
		}
	}
	// The session survived the forged requests.
	if resp, _ := get(t, c, e.srv.URL+"/console/audit"); resp.StatusCode != 200 {
		t.Fatalf("session lost: %d", resp.StatusCode)
	}
	// Expired or forged sign-in forms and unknown personas get a page with a fresh form, not plain text.
	c2 := client(t)
	_, body := get(t, c2, e.srv.URL+"/console/signin")
	resp, body := post(t, c2, e.srv.URL+"/console/signin", url.Values{"csrf": {csrfOf(t, body)}, "persona": {"nobody"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Unknown persona") || !csrfRe.MatchString(body) {
		t.Fatalf("unknown persona: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, client(t), e.srv.URL+"/console/signin", url.Values{"persona": {"approver"}, "csrf": {"stale"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "expired") {
		t.Fatalf("expired sign-in form: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, e.srv.URL+"/console/signout", url.Values{"csrf": {"forged"}})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "<h1>") {
		t.Fatalf("forged POST in a session: %d %s", resp.StatusCode, body)
	}
}

func TestThemesAndStatic(t *testing.T) {
	e := newEnv(t)
	c := client(t)
	resp, css := get(t, c, e.srv.URL+"/console/static/tokens.css")
	if resp.StatusCode != 200 || !strings.Contains(css, "#F5F6F4") || !strings.Contains(css, "#111317") ||
		!strings.Contains(css, "prefers-color-scheme: dark") || !strings.Contains(css, `[data-theme="dark"]`) {
		t.Fatalf("tokens.css missing light/dark tokens (%d)", resp.StatusCode)
	}
	resp, js := get(t, c, e.srv.URL+"/console/static/console.js")
	if resp.StatusCode != 200 || len(js) > 3*1024 {
		t.Fatalf("console.js: %d, %d bytes (budget 3 KB)", resp.StatusCode, len(js))
	}
	if resp, _ := get(t, c, e.srv.URL+"/console/static/fonts/public-sans-latin-400-normal.woff2"); resp.StatusCode != 200 {
		t.Fatal("self-hosted font missing")
	}
}

// TestConsoleRequiresScope: pages need the scopes of the matching admin API routes, not only a
// session; navigation lists only the pages the principal may open.
func TestConsoleRequiresScope(t *testing.T) {
	personas := append(devissuer.Personas(), devissuer.Persona{ID: "crm-agent", Title: "CRM agent", Tenant: "t_beta",
		Subject: "agent-crm", Scopes: []string{"crm:read"}})
	e := newEnvWith(t, envOpts{personas: personas})
	c := client(t)
	signIn(t, e, c, "crm-agent")
	for _, path := range []string{"/console", "/console/audit", "/console/audit/1", "/console/approvals", "/console/policy", "/console/upstreams"} {
		resp, body := get(t, c, e.srv.URL+path)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `role="alert"`) {
			t.Fatalf("%s without scope: %d", path, resp.StatusCode)
		}
	}
	approver := client(t)
	signIn(t, e, approver, "approver")
	_, body := get(t, approver, e.srv.URL+"/console/approvals")
	if strings.Contains(body, `href="/console/upstreams"`) == false || !strings.Contains(body, `href="/console/approvals"`) {
		t.Fatalf("approver navigation: %s", body)
	}
}

func TestConsoleSessionExpires(t *testing.T) {
	fake := clock.NewFake(time.Now())
	e := newEnvWith(t, envOpts{clock: fake})
	c := client(t)
	signIn(t, e, c, "security-reviewer")
	if resp, _ := get(t, c, e.srv.URL+"/console/audit"); resp.StatusCode != 200 {
		t.Fatalf("fresh session: %d", resp.StatusCode)
	}
	fake.Advance(2 * time.Hour)
	if resp, _ := get(t, c, e.srv.URL+"/console/audit"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/console/signin" {
		t.Fatalf("expired session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSigninDisabledWithoutIssuer(t *testing.T) {
	e := newEnvWith(t, envOpts{noSignin: true})
	c := client(t)
	resp, body := get(t, c, e.srv.URL+"/console/signin")
	if resp.StatusCode != 200 || !strings.Contains(body, "Sign-in is not configured") || strings.Contains(body, `name="persona"`) {
		t.Fatalf("sign-in page: %d %s", resp.StatusCode, body)
	}
	if resp, _ := post(t, c, e.srv.URL+"/console/signin", url.Values{"persona": {"platform-engineer"}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sign-in POST: %d", resp.StatusCode)
	}
}
