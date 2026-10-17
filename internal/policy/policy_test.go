package policy_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

const bundle = "../../policies"

func TestDecisionContract(t *testing.T) {
	e, err := policy.New(t.Context(), policy.Options{Dir: bundle})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string          `json:"name"`
		Input policy.Input    `json:"input"`
		Want  policy.Decision `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 8 {
		t.Fatalf("only %d golden cases", len(cases))
	}
	for _, c := range cases {
		c.Input.Now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
		got, err := e.Evaluate(t.Context(), c.Input)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !reflect.DeepEqual(got, c.Want) {
			t.Errorf("%s: decision = %+v, want %+v", c.Name, got, c.Want)
		}
		js, _ := json.Marshal(got)
		var shape map[string]any
		_ = json.Unmarshal(js, &shape)
		if len(shape) != 3 || shape["reasons"] == nil {
			t.Errorf("%s: decision JSON %s is not {allow, require_approval, reasons}", c.Name, js)
		}
	}
	if !e.Ready() || len(e.BundleHash()) != 64 {
		t.Fatalf("ready=%v hash=%q", e.Ready(), e.BundleHash())
	}
}

func TestRegoUnitTests(t *testing.T) {
	results, err := policy.RunTests(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 20 {
		t.Fatalf("only %d Rego tests ran", len(results))
	}
	for _, r := range results {
		if !r.Pass {
			t.Errorf("rego test %s failed: %s", r.Name, r.Error)
		}
	}
}

func copyBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(bundle, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(bundle, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o750)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

var allowInput = policy.Input{Tenant: "t_alpha", Agent: "a", Scopes: []string{"crm:read"}, Server: "crm",
	Tool: "get_customer", Args: map[string]any{"customer_id": "C-1"}, Annotations: map[string]any{"readOnlyHint": true}}

func TestBrokenBundleDeniesEverything(t *testing.T) {
	dir := copyBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte("package mcpgw.authz\n\ndecision := {"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := policy.New(t.Context(), policy.Options{Dir: dir})
	if err == nil {
		t.Fatal("broken bundle compiled")
	}
	if e.Ready() {
		t.Fatal("engine ready without a bundle")
	}
	d, err := e.Evaluate(t.Context(), allowInput)
	if err == nil || d.Allow || d.RequireApproval || !reflect.DeepEqual(d.Reasons, []string{"policy_unavailable"}) {
		t.Fatalf("decision without bundle = %+v, %v", d, err)
	}
}

func TestReloadIsAtomicAndKeepsLastGood(t *testing.T) {
	dir := copyBundle(t)
	e, err := policy.New(t.Context(), policy.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	e.OnReload(func() { reloads++ })
	first := e.BundleHash()
	// A failing reload keeps the previous bundle.
	authz := filepath.Join(dir, "authz.rego")
	good, _ := os.ReadFile(authz)
	_ = os.WriteFile(authz, []byte("package mcpgw.authz\n\nnot rego {"), 0o600)
	if err := e.Reload(t.Context()); err == nil {
		t.Fatal("broken reload succeeded")
	}
	if d, err := e.Evaluate(t.Context(), allowInput); err != nil || !d.Allow || e.BundleHash() != first || !e.Ready() {
		t.Fatalf("after failed reload: %+v %v hash changed=%v", d, err, e.BundleHash() != first)
	}
	if e.LastError() == "" {
		t.Fatal("failed reload not reported")
	}
	// A good reload swaps in the new bundle: crm reads now require approval for t_alpha.
	extra := "package mcpgw.authz.limits\n\nhold contains \"crm_read_review\" if {\n\tinput.server == \"crm\"\n\targs_present\n}\n"
	_ = os.WriteFile(authz, good, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "extra.rego"), []byte(extra), 0o600)
	if err := e.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	d, _ := e.Evaluate(t.Context(), allowInput)
	if !d.RequireApproval || e.BundleHash() == first || reloads != 1 || e.LastError() != "" {
		t.Fatalf("after good reload: %+v reloads=%d", d, reloads)
	}
	if strings.Contains(strings.Join(d.Reasons, ","), "default_deny") {
		t.Fatal("unexpected default deny")
	}
}

// TestLoadBundleConfigMapLayout loads a bundle laid out like a Kubernetes ConfigMap volume (the Helm
// chart's policies.existingConfigMap): top-level symlinks into a hidden timestamped directory
// reached through a ..data symlink. Each file must be read once, under its top-level name.
func TestLoadBundleConfigMapLayout(t *testing.T) {
	src := "../../policies"
	dir := t.TempDir()
	ts := filepath.Join(dir, "..2026_10_17_12_00_00.000000001")
	if err := os.Mkdir(ts, 0o755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range []string{"authz.rego", "approval.rego", "limits.rego", "data/tenants.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		if err := os.WriteFile(filepath.Join(ts, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := os.Symlink(filepath.Base(ts), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.Symlink(filepath.Join("..data", n), filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := policy.LoadBundle(dir, false)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if got := slices.Sorted(maps.Keys(b.Modules)); !slices.Equal(got, []string{"approval.rego", "authz.rego", "limits.rego"}) {
		t.Fatalf("modules %v, want each file once under its top-level name", got)
	}
	if _, ok := b.Data["tenants"]; !ok {
		t.Fatal("data/tenants.json not loaded")
	}
	e, err := policy.New(context.Background(), policy.Options{Dir: dir})
	if err != nil {
		t.Fatalf("engine on ConfigMap layout: %v", err)
	}
	if e.BundleHash() != b.Hash {
		t.Fatalf("engine hash %q, want %q", e.BundleHash(), b.Hash)
	}
}
