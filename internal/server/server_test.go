package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func readyz(t *testing.T, a *server.App) int {
	t.Helper()
	rec := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func TestReadyzReflectsPolicyBundle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	good, err := server.New(t.Context(), testConfig(t, "../../policies"), logger)
	if err != nil {
		t.Fatal(err)
	}
	if code := readyz(t, good); code != http.StatusOK {
		t.Fatalf("readyz with a good bundle = %d", code)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte("package mcpgw.authz\n\ndecision := {"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, err := server.New(t.Context(), testConfig(t, dir), logger)
	if err != nil {
		t.Fatal(err)
	}
	if code := readyz(t, bad); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a broken bundle = %d, want 503", code)
	}
}
