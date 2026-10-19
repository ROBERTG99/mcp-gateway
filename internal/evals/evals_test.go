package evals_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/ROBERTG99/mcp-gateway/internal/evals"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

func runner(t *testing.T) *evals.Runner {
	t.Helper()
	engine, err := policy.New(t.Context(), policy.Options{Dir: "../../policies"})
	if err != nil {
		t.Fatal(err)
	}
	return &evals.Runner{Policy: engine, Scorer: injection.New(injection.Options{}), Redactor: redact.New(), InjectionThreshold: 0.5,
		GitSHA: "0123456789abcdef", Now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC),
		PromptHashes: map[string]string{"policy_bundle": engine.BundleHash()}}
}

// TestReportMatchesEvalgateSchema: every suite's report validates against evalgate.report.v1
// (evals/schemas/, transcribed from the evalgate v0.1.0 contract), has offline mode, no models,
// a metric per gate, one case per dataset row and no cost.
func TestReportMatchesEvalgateSchema(t *testing.T) {
	raw, err := os.ReadFile("../../evals/schemas/evalgate.report.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := runner(t)
	ds, err := evals.LoadDatasets("../../evals/datasets", "dev")
	if err != nil {
		t.Fatal(err)
	}
	lat, err := r.RunLatency(t.Context(), "dev", ds.Policy)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"policy.v1": {"decision_accuracy"}, "injection.v1": {"recall", "false_positive_rate"},
		"redaction.v1": {"recall", "precision"}, evals.LatencySuite: {"policy_eval_p95_ms"},
	}
	reports := []evals.Report{r.RunPolicy(t.Context(), "dev", ds.Policy), r.RunInjection("dev", ds.Injection), r.RunRedaction("dev", ds.Redaction), lat}
	for _, rep := range reports {
		b, _ := json.Marshal(rep)
		var instance any
		if err := json.Unmarshal(b, &instance); err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(instance); err != nil {
			t.Errorf("%s: report does not match evalgate.report.v1: %v", rep.Suite, err)
		}
		if rep.Mode != "offline" || len(rep.Models) != 0 || rep.CostUSDTotal != 0 || rep.NCases != len(rep.Cases) {
			t.Errorf("%s: mode %q models %v cost %v n_cases %d cases %d", rep.Suite, rep.Mode, rep.Models, rep.CostUSDTotal, rep.NCases, len(rep.Cases))
		}
		for _, m := range want[rep.Suite] {
			if _, ok := rep.Metrics[m]; !ok {
				t.Errorf("%s: missing metric %s", rep.Suite, m)
			}
		}
	}
	// A schema violation is caught (the validator is not a no-op).
	bad := map[string]any{"schema": "evalgate.report.v1", "suite": "x"}
	if err := resolved.Validate(bad); err == nil {
		t.Fatal("an incomplete report validated")
	}
}

// TestPolicyEvalIsExact: decision accuracy is 1.0 on dev (the gate), and a wrong label is caught.
func TestPolicyEvalIsExact(t *testing.T) {
	r := runner(t)
	ds, err := evals.LoadDatasets("../../evals/datasets", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if m := r.RunPolicy(t.Context(), "dev", ds.Policy).Metrics["decision_accuracy"]; m.Value != 1 || !m.Passed {
		t.Fatalf("dev decision accuracy = %+v", m)
	}
	ds.Policy[0].Expected = map[string]string{"allow": "deny", "deny": "hold", "hold": "allow"}[ds.Policy[0].Expected]
	if m := r.RunPolicy(t.Context(), "dev", ds.Policy).Metrics["decision_accuracy"]; m.Passed {
		t.Fatalf("a wrong label passed: %+v", m)
	}
}

func TestMarkdownCitesReports(t *testing.T) {
	r := runner(t)
	ds, err := evals.LoadDatasets("../../evals/datasets", "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rep := r.RunRedaction("test", ds.Redaction)
	if err := evals.WriteReport(dir, rep, "test"); err != nil {
		t.Fatal(err)
	}
	md, err := evals.Markdown(dir, filepath.Join(dir, "none"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`redaction.v1` | precision", "`redaction.v1` | recall", "Not run in this report: policy.v1", "do not edit by hand"} {
		if !strings.Contains(md, want) {
			t.Errorf("REPORT.md lacks %q:\n%s", want, md)
		}
	}
}
