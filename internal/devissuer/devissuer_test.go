package devissuer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
)

func TestDevIssuerTokenVerifies(t *testing.T) {
	iss, err := devissuer.New(devissuer.Options{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(iss.Handler())
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(map[string]any{"tenant": "t_alpha", "agent": "agent-support", "scopes": []string{"crm:read"}})
	resp, err := http.Post(srv.URL+"/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("token: %d %v", resp.StatusCode, err)
	}
	if tok.TokenType != "Bearer" || tok.ExpiresIn <= 0 || tok.ExpiresIn > 3600 {
		t.Fatalf("token response = %+v", tok)
	}
	v := authn.NewVerifier(authn.Options{JWKSURL: srv.URL + "/.well-known/jwks.json",
		Issuer: devissuer.Issuer, Audience: devissuer.Audience})
	p, err := v.Verify(t.Context(), tok.AccessToken)
	if err != nil {
		t.Fatalf("gateway verifier rejected the dev token: %v", err)
	}
	if p.Tenant != "t_alpha" || p.Agent != "agent-support" || !p.HasScope("crm:read") {
		t.Fatalf("principal = %+v", p)
	}
	if time.Until(p.ExpiresAt) > time.Hour {
		t.Fatalf("lifetime too long: %s", time.Until(p.ExpiresAt))
	}
}

func TestDevIssuerRejectsBadRequests(t *testing.T) {
	iss, err := devissuer.New(devissuer.Options{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := iss.Handler()
	for _, body := range []string{`{}`, `{"tenant":"t_alpha"}`, `{"tenant":"t alpha","agent":"a"}`,
		`{"tenant":"t_alpha","agent":"a","ttl_seconds":7200}`, `not json`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/token", bytes.NewBufferString(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/personas", nil))
	var personas []devissuer.Persona
	if err := json.Unmarshal(rec.Body.Bytes(), &personas); err != nil || len(personas) < 3 {
		t.Fatalf("personas: %v %s", err, rec.Body.String())
	}
}

func TestDevIssuerDisabledByDefault(t *testing.T) {
	if _, err := devissuer.New(devissuer.Options{}); !errors.Is(err, devissuer.ErrDisabled) {
		t.Fatalf("New with zero options: err = %v, want ErrDisabled", err)
	}
}
