package authn_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type fixture struct {
	key     *rsa.PrivateKey
	jwks    *httptest.Server
	fetches atomic.Int64
	down    atomic.Bool
	clock   *clock.Fake
	v       *authn.Verifier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{key: key, clock: clock.NewFake(now)}
	f.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.jwks.Close)
	f.v = authn.NewVerifier(authn.Options{
		JWKSURL: f.jwks.URL, Issuer: "mcp-gateway dev-issuer", Audience: "mcp-gateway",
		CacheTTL: 5 * time.Minute, StaleLimit: time.Hour, Clock: f.clock,
	})
	return f
}

func (f *fixture) claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "mcp-gateway dev-issuer", "aud": "mcp-gateway", "sub": "agent-support", "tenant": "t_alpha",
		"scopes": []string{"crm:read", "orders:write"}, "iat": now.Unix(), "exp": now.Add(30 * time.Minute).Unix(),
	}
}

func (f *fixture) sign(t *testing.T, c jwt.MapClaims, kid string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTAcceptsValidToken(t *testing.T) {
	f := newFixture(t)
	p, err := f.v.Verify(t.Context(), f.sign(t, f.claims(), "k1"))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if p.Tenant != "t_alpha" || p.Agent != "agent-support" || !p.HasScope("orders:write") || p.HasScope("crm:write") {
		t.Fatalf("principal = %+v", p)
	}
}

func TestJWTRejectsWrongAudience(t *testing.T) {
	f := newFixture(t)
	c := f.claims()
	c["aud"] = "another-service"
	assertRejected(t, f, f.sign(t, c, "k1"))
}

func TestJWTRejectsAlgNone(t *testing.T) {
	f := newFixture(t)
	none := jwt.NewWithClaims(jwt.SigningMethodNone, f.claims())
	none.Header["kid"] = "k1"
	s, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected(t, f, s)
	// HS256 signed with the public modulus (key confusion) is rejected too.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims())
	hs.Header["kid"] = "k1"
	hsTok, _ := hs.SignedString(f.key.N.Bytes())
	assertRejected(t, f, hsTok)
}

func TestJWTRejectsOtherInvalidTokens(t *testing.T) {
	f := newFixture(t)
	mutate := map[string]func(jwt.MapClaims){
		"expired":       func(c jwt.MapClaims) { c["exp"] = now.Add(-31 * time.Second).Unix() },
		"no exp":        func(c jwt.MapClaims) { delete(c, "exp") },
		"wrong issuer":  func(c jwt.MapClaims) { c["iss"] = "evil" },
		"no tenant":     func(c jwt.MapClaims) { delete(c, "tenant") },
		"no sub":        func(c jwt.MapClaims) { delete(c, "sub") },
		"scopes string": func(c jwt.MapClaims) { c["scopes"] = "crm:read" },
		"lifetime > 1h": func(c jwt.MapClaims) { c["exp"] = now.Add(2 * time.Hour).Unix() },
		"not yet valid": func(c jwt.MapClaims) { c["nbf"] = now.Add(time.Minute).Unix() },
	}
	for name, m := range mutate {
		c := f.claims()
		m(c)
		if _, err := f.v.Verify(t.Context(), f.sign(t, c, "k1")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.v.Verify(t.Context(), f.sign(t, f.claims(), "")); err == nil {
		t.Error("missing kid accepted")
	}
	if _, err := f.v.Verify(t.Context(), f.sign(t, f.claims(), "unknown")); err == nil {
		t.Error("unknown kid accepted")
	}
	// Within the 30 s skew an expired token is still accepted.
	c := f.claims()
	c["exp"] = now.Add(-20 * time.Second).Unix()
	if _, err := f.v.Verify(t.Context(), f.sign(t, c, "k1")); err != nil {
		t.Errorf("token within skew rejected: %v", err)
	}
}

func TestJWKSCacheAndStaleLimit(t *testing.T) {
	f := newFixture(t)
	tok := func() string {
		c := f.claims()
		c["iat"], c["exp"] = f.clock.Now().Unix(), f.clock.Now().Add(30*time.Minute).Unix()
		return f.sign(t, c, "k1")
	}
	for range 3 {
		if _, err := f.v.Verify(t.Context(), tok()); err != nil {
			t.Fatal(err)
		}
	}
	if f.fetches.Load() != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (cached)", f.fetches.Load())
	}
	f.down.Store(true)
	f.clock.Advance(10 * time.Minute) // cache expired, issuer down: stale keys still used
	if _, err := f.v.Verify(t.Context(), tok()); err != nil {
		t.Fatalf("stale keys not used while issuer down: %v", err)
	}
	f.clock.Advance(2 * time.Hour) // beyond the stale limit
	if _, err := f.v.Verify(t.Context(), tok()); err == nil {
		t.Fatal("token accepted with keys older than the stale limit")
	}
}

// Regression (found in review): a hung issuer must not block verification while cached keys are
// within the stale limit.
func TestJWKSHungIssuerDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	if _, err := f.v.Verify(t.Context(), f.sign(t, f.claims(), "k1")); err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	defer close(hang)
	f.jwks.Config.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang })
	f.clock.Advance(10 * time.Minute) // cache stale, within the stale limit
	for range 5 {
		c := f.claims()
		c["iat"], c["exp"] = f.clock.Now().Unix(), f.clock.Now().Add(30*time.Minute).Unix()
		start := time.Now()
		if _, err := f.v.Verify(t.Context(), f.sign(t, c, "k1")); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("verification blocked for %s on a hung issuer", d)
		}
	}
}

func TestMiddlewareWWWAuthenticate(t *testing.T) {
	f := newFixture(t)
	h := f.v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := authn.FromContext(r.Context())
		_, _ = w.Write([]byte(p.Tenant))
	}))
	c := f.claims()
	c["aud"] = "nope"
	for name, header := range map[string]string{
		"bad token": "Bearer " + f.sign(t, c, "k1"),
		"garbage":   "Bearer abc.def.ghi",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", header)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		if strings.Contains(rec.Body.String(), "eyJ") {
			t.Errorf("%s: response echoes the token", name)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("missing token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+f.sign(t, f.claims(), "k1"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "t_alpha" {
		t.Errorf("valid: %d %q", rec.Code, rec.Body.String())
	}
}

// assertRejected checks that a token fails verification and that the middleware answers 401 with
// the invalid_token challenge.
func assertRejected(t *testing.T, f *fixture, token string) {
	t.Helper()
	if _, err := f.v.Verify(t.Context(), token); err == nil {
		t.Fatal("token accepted")
	}
	h := f.v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached") }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
		t.Fatalf("middleware: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
