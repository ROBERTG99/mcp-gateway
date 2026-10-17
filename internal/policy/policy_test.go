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

// TestMalformedDecisionDenies: a bundle whose decision does not have the contract shape
// {allow, require_approval, reasons} denies with policy_error instead of guessing.
func TestMalformedDecisionDenies(t *testing.T) {
	for name, rule := range map[string]string{
		"not an object":           `decision := "allow"`,
		"missing reasons":         `decision := {"allow": true, "require_approval": false}`,
		"extra key":               `decision := {"allow": true, "require_approval": false, "reasons": [], "x": 1}`,
		"reason not a string":     `decision := {"allow": true, "require_approval": false, "reasons": [1]}`,
		"allow and hold together": `decision := {"allow": true, "require_approval": true, "reasons": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte("package mcpgw.authz\n\n"+rule+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e, err := policy.New(context.Background(), policy.Options{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			d, err := e.Evaluate(context.Background(), policy.Input{Tenant: "t_alpha", Agent: "a", Server: "crm", Tool: "get_customer"})
			if err == nil || d.Allow || d.RequireApproval || !slices.Equal(d.Reasons, []string{"policy_error"}) {
				t.Fatalf("decision %+v, err %v: want a policy_error denial", d, err)
			}
		})
	}
}

// TestWatchReloadsChangedBundle: Watch reloads when the bundle's content changes and reports a
// broken bundle through onError while the last good one keeps serving; it stops with ctx.
func TestWatchReloadsChangedBundle(t *testing.T) {
	dir := t.TempDir()
	write := func(src string) {
		if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("package mcpgw.authz\n\ndecision := {\"allow\": false, \"require_approval\": false, \"reasons\": [\"v1\"]}\n")
	e, err := policy.New(context.Background(), policy.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Watch(context.Background(), 0, nil); err != nil {
		t.Fatalf("Watch with interval 0 = %v, want nil (disabled)", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 16)
	done := make(chan error, 1)
	// Non-blocking: while the broken bundle is on disk every tick reports it, and a full channel
	// must not stall the watcher before the test cancels it.
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	go func() { done <- e.Watch(ctx, 5*time.Millisecond, report) }()
	reason := func() string {
		d, _ := e.Evaluate(context.Background(), policy.Input{Tenant: "t_alpha"})
		return strings.Join(d.Reasons, ",")
	}
	write("package mcpgw.authz\n\ndecision := {\"allow\": false, \"require_approval\": false, \"reasons\": [\"v2\"]}\n")
	for deadline := time.Now().Add(5 * time.Second); reason() != "v2"; {
		if time.Now().After(deadline) {
			t.Fatal("changed bundle not reloaded")
		}
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // polling the watcher with a deadline
	}
	write("package mcpgw.authz\n\ndecision := {")
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("onError called with nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broken bundle not reported")
	}
	if reason() != "v2" {
		t.Fatal("the last good bundle stopped serving")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Watch = %v after cancel", err)
	}
}

func TestRunTestsReportsBrokenTestModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte("package mcpgw.authz\n\nx := 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authz_test.rego"), []byte("package mcpgw.authz_test\n\ntest_x if {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.RunTests(context.Background(), dir); err == nil {
		t.Fatal("RunTests accepted a test module that does not parse")
	}
	if _, err := policy.RunTests(context.Background(), t.TempDir()); err == nil {
		t.Fatal("RunTests accepted an empty directory")
	}
}

// TestNonDeterministicBuiltinsRejected: a bundle that calls the network, the clock or a random
// source does not compile, so a hot reload of such a bundle fails and the last good one keeps
// serving (.claude/rules/rego.md: decisions depend on the input only).
func TestNonDeterministicBuiltinsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"http.send":   `x := http.send({"method": "get", "url": "http://example.invalid"})`,
		"time.now_ns": `x := time.now_ns()`,
		"rand.intn":   `x := rand.intn("k", 10)`,
		"uuid":        `x := uuid.rfc4122("k")`,
		"opa.runtime": `x := opa.runtime()`,
		"net":         `x := net.lookup_ip_addr("example.invalid")`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := "package mcpgw.authz\n\n" + body + "\n\ndecision := {\"allow\": false, \"require_approval\": false, \"reasons\": [\"x\"]}\n"
			if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			e, err := policy.New(context.Background(), policy.Options{Dir: dir})
			if err == nil && e.Ready() {
				t.Fatalf("bundle using %s compiled", name)
			}
		})
	}
}
