package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/server"
)

func testConfig(t *testing.T, policyDir string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamsFile = "../../config/upstreams.yaml"
	cfg.TenantsFile = "../../config/tenants.yaml"
	cfg.PolicyDir = policyDir
	return cfg
}

// readyz returns the status code and the policy check (Postgres and Redis are not running in
// this unit test, so overall readiness is 503 either way; the policy check is what is asserted).
func readyz(t *testing.T, a *server.App) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Checks map[string]string `json:"checks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.Checks["policy"]
}

func TestReadyzReflectsPolicyBundle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	good, err := server.New(t.Context(), testConfig(t, "../../policies"), logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, check := readyz(t, good); check != "ok" {
		t.Fatalf("policy check with a good bundle = %q", check)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte("package mcpgw.authz\n\ndecision := {"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, err := server.New(t.Context(), testConfig(t, dir), logger)
	if err != nil {
		t.Fatal(err)
	}
	if code, check := readyz(t, bad); code != http.StatusServiceUnavailable || !strings.HasPrefix(check, "not loaded") {
		t.Fatalf("readyz with a broken bundle = %d %q, want 503 not loaded", code, check)
	}
}

// TestReadyzWaitsForAuditSequence: until the first block of audit seqs is allocated (RunBackground
// warms it), every call would fail with -32048, so the gateway reports not ready.
func TestReadyzWaitsForAuditSequence(t *testing.T) {
	a, err := server.New(t.Context(), testConfig(t, "../../policies"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || body.Checks["audit"] != "sequence not allocated yet" {
		t.Fatalf("readyz before warm-up = %d %v, want 503 with audit not allocated", rec.Code, body.Checks)
	}
}
