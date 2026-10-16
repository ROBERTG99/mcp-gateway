package datagen_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
)

// TestEvalDatasetsCountsAndSplits (spec §11): suite sizes and dev/test splits, class balance,
// unique ids, determinism, test-partition protection, and the committed files match the generator.
func TestEvalDatasetsCountsAndSplits(t *testing.T) {
	d := datagen.GenerateEvals(datagen.DefaultSeed)
	for _, c := range []struct {
		suite     string
		dev, test int
	}{
		{datagen.PolicySuite, len(d.Policy.Dev), len(d.Policy.Test)},
		{datagen.InjectionSuite, len(d.Injection.Dev), len(d.Injection.Test)},
		{datagen.RedactionSuite, len(d.Redaction.Dev), len(d.Redaction.Test)},
	} {
		want := map[string][2]int{datagen.PolicySuite: {90, 60}, datagen.InjectionSuite: {120, 80}, datagen.RedactionSuite: {72, 48}}[c.suite]
		if c.dev != want[0] || c.test != want[1] {
			t.Errorf("%s: dev/test = %d/%d, want %d/%d", c.suite, c.dev, c.test, want[0], want[1])
		}
	}

	// Injection: 100 malicious (30 obfuscated) and 100 benign, stratified 60/40.
	count := func(cases []datagen.InjectionCase, label string, obf bool) int {
		n := 0
		for _, c := range cases {
			if c.Label == label && c.Obfuscated == obf {
				n++
			}
		}
		return n
	}
	for _, part := range []struct {
		name  string
		cases []datagen.InjectionCase
		want  [3]int // plain malicious, obfuscated malicious, benign
	}{{"dev", d.Injection.Dev, [3]int{42, 18, 60}}, {"test", d.Injection.Test, [3]int{28, 12, 40}}} {
		got := [3]int{count(part.cases, "malicious", false), count(part.cases, "malicious", true), count(part.cases, "benign", false)}
		if got != part.want {
			t.Errorf("injection %s: plain/obfuscated/benign = %v, want %v", part.name, got, part.want)
		}
	}

	// Policy: every decision occurs in both partitions.
	for name, cases := range map[string][]datagen.PolicyCase{"dev": d.Policy.Dev, "test": d.Policy.Test} {
		seen := map[string]int{}
		for _, c := range cases {
			seen[c.Expected]++
		}
		if seen["allow"] == 0 || seen["hold"] == 0 || seen["deny"] == 0 {
			t.Errorf("policy %s decisions = %v, want all of allow, hold, deny", name, seen)
		}
	}

	// Redaction: positives carry spans, negatives only decoys.
	for _, c := range append(append([]datagen.RedactionCase{}, d.Redaction.Dev...), d.Redaction.Test...) {
		if len(c.Decoys) == 0 {
			t.Errorf("%s has no decoy", c.ID)
		}
		for _, s := range c.Spans {
			if s.Value == "" || s.Type == "" {
				t.Errorf("%s: empty span %+v", c.ID, s)
			}
		}
	}

	files, err := d.EvalFiles()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for name, b := range files {
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			i := strings.Index(line, `"id":"`)
			id := line[i+6 : i+6+strings.Index(line[i+6:], `"`)]
			if ids[id] {
				t.Errorf("duplicate id %s", id)
			}
			ids[id] = true
		}
	}
	if len(ids) != datagen.PolicyCases+datagen.InjectionCases+datagen.RedactionCases {
		t.Errorf("%d ids, want %d", len(ids), datagen.PolicyCases+datagen.InjectionCases+datagen.RedactionCases)
	}

	// Deterministic: a second generation is byte-identical.
	again, _ := datagen.GenerateEvals(datagen.DefaultSeed).EvalFiles()
	for name, b := range files {
		if !bytes.Equal(b, again[name]) {
			t.Errorf("%s differs between two generations", name)
		}
	}

	// The committed datasets are exactly what the generator writes.
	for name, b := range files {
		committed, err := os.ReadFile(filepath.Join("..", "..", "evals", "datasets", name))
		if err != nil || !bytes.Equal(committed, b) {
			t.Errorf("evals/datasets/%s is missing or stale (run `go run ./cmd/datagen evals`): %v", name, err)
		}
	}

	// An existing test file is never changed.
	dir := t.TempDir()
	if n, err := datagen.WriteEvalDatasets(dir, datagen.DefaultSeed); err != nil || n != len(files) {
		t.Fatalf("first write: %d files, %v", n, err)
	}
	if n, err := datagen.WriteEvalDatasets(dir, datagen.DefaultSeed); err != nil || n != 0 {
		t.Fatalf("rewrite of identical files: %d written, %v", n, err)
	}
	if _, err := datagen.WriteEvalDatasets(dir, datagen.DefaultSeed+1); !errors.Is(err, datagen.ErrTestPartitionChanged) {
		t.Fatalf("other seed over an existing test partition: %v, want ErrTestPartitionChanged", err)
	}
}
