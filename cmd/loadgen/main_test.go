package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/ROBERTG99/mcp-gateway/internal/loadgen"
)

// TestChaosReportMatchesSchema: the chaos.v1 report is an evalgate.report.v1 document.
func TestChaosReportMatchesSchema(t *testing.T) {
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
	rep := chaosReport(t.Context(), loadgen.ChaosResult{RunID: "r", Calls: 2, Attempts: 3, Succeeded: 2, AuditedRatio: 1, ChainOK: true,
		Duration: time.Second, PerOrder: []loadgen.OrderResult{{OrderID: "O-00002", Keys: 2, Increase: 2}}})
	b, _ := json.Marshal(rep)
	var doc any
	_ = json.Unmarshal(b, &doc)
	if err := resolved.Validate(doc); err != nil {
		t.Fatalf("chaos report: %v", err)
	}
	if !rep.Metrics["duplicates"].Passed || !rep.Metrics["audited_ratio"].Passed {
		t.Fatalf("metrics = %+v", rep.Metrics)
	}
	rep = chaosReport(t.Context(), loadgen.ChaosResult{AuditedRatio: 1.0001})
	if rep.Metrics["audited_ratio"].Passed {
		t.Fatal("an audited ratio above 1 (double-audited calls) must not pass")
	}
}
