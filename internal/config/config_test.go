package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigRejectsBadServerName(t *testing.T) {
	for _, name := range []string{"Ledger", "1ledger", "l", "ledger-v2", "a234567890123456789012345678901234", ""} {
		dir := t.TempDir()
		path := filepath.Join(dir, "upstreams.yaml")
		body := "upstreams:\n  - name: \"" + name + "\"\n    url: http://x/mcp\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadUpstreams(path); err == nil {
			t.Errorf("LoadUpstreams accepted server name %q", name)
		} else if !strings.Contains(err.Error(), "server name") {
			t.Errorf("error %q does not mention the server name", err)
		}
	}
	ups, err := config.LoadUpstreams("../../config/upstreams.yaml")
	if err != nil {
		t.Fatalf("shipped upstreams.yaml: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no upstreams in config/upstreams.yaml")
	}
}

func TestConfigRejectsDuplicateServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "u.yaml")
	body := "upstreams:\n  - {name: crm, url: http://a/mcp}\n  - {name: crm, url: http://b/mcp}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadUpstreams(path); err == nil {
		t.Fatal("duplicate server accepted")
	}
}

func TestEnvOverrides(t *testing.T) {
	def, err := config.Load(env(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if def.MCPAddr != ":8210" || def.AdminAddr != ":8200" || def.MetricsAddr != ":8211" {
		t.Fatalf("default addrs = %s %s %s", def.MCPAddr, def.AdminAddr, def.MetricsAddr)
	}
	if def.InjectionThreshold != 0.5 || def.InjectionMode != "mark" || def.ApprovalTTL != 15*time.Minute ||
		def.UpstreamTimeout != 10*time.Second || def.AuditBuffer != 10000 || def.JWTAudience != "mcp-gateway" {
		t.Fatalf("unexpected defaults: %+v", def)
	}
	got, err := config.Load(env(map[string]string{
		"MCPGW_MCP_ADDR":            ":9999",
		"MCPGW_INJECTION_THRESHOLD": "0.7",
		"MCPGW_INJECTION_MODE":      "block",
		"MCPGW_APPROVAL_TTL":        "5m",
		"MCPGW_AUDIT_BUFFER":        "42",
		"MCPGW_DEV_ISSUER_ENABLED":  "true",
		"MCPGW_IDEMPOTENCY_STORE":   "dynamodb",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.MCPAddr != ":9999" || got.InjectionThreshold != 0.7 || got.InjectionMode != "block" ||
		got.ApprovalTTL != 5*time.Minute || got.AuditBuffer != 42 || !got.DevIssuerEnabled ||
		got.IdempotencyStore != "dynamodb" {
		t.Fatalf("overrides not applied: %+v", got)
	}
}

func TestEnvValidation(t *testing.T) {
	for k, v := range map[string]string{
		"MCPGW_INJECTION_THRESHOLD": "1.5",
		"MCPGW_INJECTION_MODE":      "shout",
		"MCPGW_IDEMPOTENCY_STORE":   "mysql",
		"MCPGW_AUDIT_BUFFER":        "many",
		"MCPGW_APPROVAL_TTL":        "soon",
		"MCPGW_DEV_ISSUER_ENABLED":  "perhaps",
	} {
		if _, err := config.Load(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: err = %v, want error naming the variable", k, v, err)
		}
	}
}

func TestTenantsFile(t *testing.T) {
	tenants, err := config.LoadTenants("../../config/tenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	beta, ok := tenants["t_beta"]
	if !ok {
		t.Fatal("t_beta missing")
	}
	if beta.RatePerSecond != 5 || beta.Burst != 10 {
		t.Fatalf("t_beta limits = %+v, want 5/s burst 10", beta)
	}
	for _, id := range []string{"t_alpha", "t_beta", "t_gamma"} {
		if tenants[id].DailyBudget <= 0 {
			t.Errorf("%s has no daily budget", id)
		}
	}
}

// Every MCPGW_ variable used anywhere in Go code is declared in the config table and
// documented in .env.example, and every declared variable is documented.
func TestEveryVariableDocumented(t *testing.T) {
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, v := range config.Vars() {
		declared[v.Name] = true
		if !regexp.MustCompile(`(?m)^#?\s*` + v.Name + `=`).Match(example) {
			t.Errorf("%s is not documented in .env.example", v.Name)
		}
		if v.Description == "" {
			t.Errorf("%s has no description", v.Name)
		}
	}
	re := regexp.MustCompile(`"(MCPGW_[A-Z0-9_]+)"`)
	_ = filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "bin") {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, _ := os.ReadFile(path)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if !declared[m[1]] && !regexp.MustCompile(`(?m)^#?\s*`+m[1]+`=`).Match(example) {
				t.Errorf("%s (used in %s) is not documented", m[1], path)
			}
		}
		return nil
	})
}
