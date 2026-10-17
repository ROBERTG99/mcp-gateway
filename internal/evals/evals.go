// Package evals runs the offline eval suites of spec §11 (policy.v1, injection.v1, redaction.v1,
// latency.v1) against the committed datasets and writes evalgate.report.v1 reports. Graders are
// deterministic: there is no free text to judge, so no model and no LLM-as-judge is involved.
package evals

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

// Report contract (evalgate v0.1.0, schemas/evalgate.report.v1.json; copy in evals/schemas/).
const (
	ReportSchema = "evalgate.report.v1"
	Higher       = "higher"
	Lower        = "lower"

	// Bootstrap settings for the reported 95% confidence intervals.
	BootstrapResamples = 2000
	BootstrapSeed      = 1337

	// LatencyEvaluations is the size of latency.v1 (spec §11).
	LatencyEvaluations = 10000
)

// Metric is one gated metric.
type Metric struct {
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Direction string  `json:"direction"`
	Passed    bool    `json:"passed"`
	CILow     float64 `json:"ci_low"`
	CIHigh    float64 `json:"ci_high"`
}

// Case is one graded case.
type Case struct {
	ID        string             `json:"id"`
	Split     string             `json:"split"`
	Scores    map[string]float64 `json:"scores"`
	Passed    bool               `json:"passed"`
	CostUSD   float64            `json:"cost_usd"`
	LatencyMS float64            `json:"latency_ms"`
	Error     *string            `json:"error"`
}

// Report is an evalgate.report.v1 document.
type Report struct {
	Schema         string            `json:"schema"`
	Suite          string            `json:"suite"`
	RunID          string            `json:"run_id"`
	CreatedAt      string            `json:"created_at"`
	GitSHA         string            `json:"git_sha"`
	Mode           string            `json:"mode"`
	Models         []string          `json:"models"`
	PromptHashes   map[string]string `json:"prompt_hashes"`
	NCases         int               `json:"n_cases"`
	Metrics        map[string]Metric `json:"metrics"`
	CostUSDTotal   float64           `json:"cost_usd_total"`
	CostUSDPerCase float64           `json:"cost_usd_per_case"`
	LatencyP50MS   float64           `json:"latency_p50_ms"`
	LatencyP95MS   float64           `json:"latency_p95_ms"`
	Cases          []Case            `json:"cases"`
	Extensions     map[string]any    `json:"extensions,omitempty"`
}

// Datasets are the committed eval datasets of one split.
type Datasets struct {
	Policy    []datagen.PolicyCase
	Injection []datagen.InjectionCase
	Redaction []datagen.RedactionCase
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path) //nolint:gosec // G304: dataset paths come from the CLI flag
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var c T
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

// LoadDatasets reads dir/<split>/{policy,injection,redaction}.v1.jsonl.
func LoadDatasets(dir, split string) (Datasets, error) {
	var d Datasets
	var err error
	if d.Policy, err = readJSONL[datagen.PolicyCase](filepath.Join(dir, split, datagen.PolicySuite+".jsonl")); err != nil {
		return d, err
	}
	if d.Injection, err = readJSONL[datagen.InjectionCase](filepath.Join(dir, split, datagen.InjectionSuite+".jsonl")); err != nil {
		return d, err
	}
	d.Redaction, err = readJSONL[datagen.RedactionCase](filepath.Join(dir, split, datagen.RedactionSuite+".jsonl"))
	return d, err
}

// Runner holds what every suite needs.
type Runner struct {
	Policy             *policy.Engine
	Scorer             *injection.Scorer
	Redactor           *redact.Redactor
	InjectionThreshold float64
	GitSHA             string
	Now                time.Time
	PromptHashes       map[string]string
}

func (r *Runner) report(suite, split string, cases []Case, metrics map[string]Metric) Report {
	lat := make([]float64, len(cases))
	for i, c := range cases {
		lat[i] = c.LatencyMS
	}
	created := r.Now.UTC().Format(time.RFC3339)
	return Report{Schema: ReportSchema, Suite: suite, RunID: fmt.Sprintf("%s-%s-%s", suite, split, r.Now.UTC().Format("20060102T150405Z")),
		CreatedAt: created, GitSHA: r.GitSHA, Mode: "offline", Models: []string{}, PromptHashes: r.PromptHashes,
		NCases: len(cases), Metrics: metrics, LatencyP50MS: Percentile(lat, 50), LatencyP95MS: Percentile(lat, 95), Cases: cases}
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Nanoseconds())/1e3) / 1e3 }

func decisionLabel(d policy.Decision) string {
	switch {
	case d.Allow:
		return "allow"
	case d.RequireApproval:
		return "hold"
	default:
		return "deny"
	}
}

// policyInput converts a dataset input (decoded with UseNumber) to the engine's input type the
// same way the admin API does: through JSON.
func policyInput(m map[string]any) (policy.Input, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return policy.Input{}, fmt.Errorf("encode input: %w", err)
	}
	var in policy.Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return policy.Input{}, fmt.Errorf("decode input: %w", err)
	}
	if in.Scopes == nil {
		in.Scopes = []string{}
	}
	return in, nil
}

// RunPolicy grades decision accuracy (gate = 1.0).
func (r *Runner) RunPolicy(ctx context.Context, split string, ds []datagen.PolicyCase) Report {
	cases := make([]Case, 0, len(ds))
	for _, c := range ds {
		in, err := policyInput(c.Input)
		start := time.Now()
		var d policy.Decision
		if err == nil {
			d, err = r.Policy.Evaluate(ctx, in)
		}
		lat := ms(time.Since(start))
		got := decisionLabel(d)
		ok := err == nil && got == c.Expected
		cc := Case{ID: c.ID, Split: split, Scores: map[string]float64{"decision_correct": b2f(ok)}, Passed: ok, LatencyMS: lat}
		if err != nil {
			msg := err.Error()
			cc.Error = &msg
		}
		cases = append(cases, cc)
	}
	acc := ratioMetric(cases, func(c Case) (float64, float64) { return c.Scores["decision_correct"], 1 })
	return r.report(datagen.PolicySuite, split, cases, map[string]Metric{"decision_accuracy": gate(acc, 1.0, Higher)})
}

// RunInjection grades recall on malicious cases (>= 0.80) and the false-positive rate on benign
// cases (<= 0.05) at the configured threshold.
func (r *Runner) RunInjection(split string, ds []datagen.InjectionCase) Report {
	cases := make([]Case, 0, len(ds))
	for _, c := range ds {
		start := time.Now()
		res := r.Scorer.Score(c.Text)
		lat := ms(time.Since(start))
		flagged := res.Score >= r.InjectionThreshold
		malicious := c.Label == "malicious"
		cases = append(cases, Case{ID: c.ID, Split: split, LatencyMS: lat, Passed: flagged == malicious,
			Scores: map[string]float64{"score": res.Score, "flagged": b2f(flagged), "malicious": b2f(malicious)}})
	}
	recall := ratioMetric(cases, func(c Case) (float64, float64) {
		return c.Scores["flagged"] * c.Scores["malicious"], c.Scores["malicious"]
	})
	fpr := ratioMetric(cases, func(c Case) (float64, float64) {
		benign := 1 - c.Scores["malicious"]
		return c.Scores["flagged"] * benign, benign
	})
	rep := r.report(datagen.InjectionSuite, split, cases, map[string]Metric{
		"recall": gate(recall, 0.80, Higher), "false_positive_rate": gate(fpr, 0.05, Lower)})
	rep.Extensions = map[string]any{"threshold": r.InjectionThreshold}
	return rep
}

// RunRedaction grades span recall (>= 0.98: the labelled value no longer appears) and precision
// (>= 0.95: markers that stand for labelled spans, not for decoys or other text).
func (r *Runner) RunRedaction(split string, ds []datagen.RedactionCase) Report {
	cases := make([]Case, 0, len(ds))
	for _, c := range ds {
		raw, _ := json.Marshal(c.Args)
		start := time.Now()
		out := string(r.Redactor.JSON(raw))
		lat := ms(time.Since(start))
		removed := 0
		for _, s := range c.Spans {
			if !strings.Contains(out, s.Value) {
				removed++
			}
		}
		decoysLost := 0
		for _, d := range c.Decoys {
			if !strings.Contains(out, d) {
				decoysLost++
			}
		}
		markers := strings.Count(out, "[REDACTED:")
		tp := min(removed, markers)
		cases = append(cases, Case{ID: c.ID, Split: split, LatencyMS: lat,
			Passed: removed == len(c.Spans) && markers == removed && decoysLost == 0,
			Scores: map[string]float64{"spans": float64(len(c.Spans)), "spans_removed": float64(removed),
				"markers": float64(markers), "true_markers": float64(tp), "decoys_lost": float64(decoysLost)}})
	}
	recall := ratioMetric(cases, func(c Case) (float64, float64) { return c.Scores["spans_removed"], c.Scores["spans"] })
	precision := ratioMetric(cases, func(c Case) (float64, float64) { return c.Scores["true_markers"], c.Scores["markers"] })
	return r.report(datagen.RedactionSuite, split, cases, map[string]Metric{
		"recall": gate(recall, 0.98, Higher), "precision": gate(precision, 0.95, Higher)})
}

// LatencySuite is the name of the policy-latency suite.
const LatencySuite = "latency.v1"

// RunLatency evaluates LatencyEvaluations policy inputs (cycling through the policy cases) and
// gates the p95 evaluation time (< 1 ms). Each case reports its inputs' median time.
func (r *Runner) RunLatency(ctx context.Context, split string, ds []datagen.PolicyCase) (Report, error) {
	inputs := make([]policy.Input, len(ds))
	for i, c := range ds {
		in, err := policyInput(c.Input)
		if err != nil {
			return Report{}, err
		}
		inputs[i] = in
	}
	for i := range min(200, LatencyEvaluations) { // warm-up, not measured
		_, _ = r.Policy.Evaluate(ctx, inputs[i%len(inputs)])
	}
	all := make([]float64, 0, LatencyEvaluations)
	per := make([][]float64, len(ds))
	for i := range LatencyEvaluations {
		k := i % len(inputs)
		start := time.Now()
		if _, err := r.Policy.Evaluate(ctx, inputs[k]); err != nil {
			return Report{}, fmt.Errorf("evaluate %s: %w", ds[k].ID, err)
		}
		d := float64(time.Since(start).Nanoseconds()) / 1e6
		all = append(all, d)
		per[k] = append(per[k], d)
	}
	cases := make([]Case, len(ds))
	for k, c := range ds {
		med := Percentile(per[k], 50)
		cases[k] = Case{ID: c.ID, Split: split, LatencyMS: round3(med), Passed: med < 1, Scores: map[string]float64{"latency_ms": round3(med)}}
	}
	p95 := Percentile(all, 95)
	lo, hi := bootstrapPercentile(all, 95)
	rep := r.report(LatencySuite, split, cases, map[string]Metric{
		"policy_eval_p95_ms": {Value: round3(p95), Threshold: 1.0, Direction: Lower, Passed: p95 < 1.0, CILow: round3(lo), CIHigh: round3(hi)}})
	rep.LatencyP50MS, rep.LatencyP95MS = round3(Percentile(all, 50)), round3(p95)
	rep.Extensions = map[string]any{"evaluations": LatencyEvaluations}
	return rep, nil
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// ratio is a micro-averaged metric over cases with its bootstrap CI.
type ratio struct{ value, lo, hi float64 }

// ratioMetric computes sum(num)/sum(den) over cases and a percentile-bootstrap 95% CI
// (resampling cases). An empty denominator yields 1 for a "higher" metric's numerator count of
// zero; callers only use it with non-empty denominators.
func ratioMetric(cases []Case, f func(Case) (num, den float64)) ratio {
	num, den := make([]float64, len(cases)), make([]float64, len(cases))
	for i, c := range cases {
		num[i], den[i] = f(c)
	}
	value := safeDiv(sum(num), sum(den))
	rng := rand.New(rand.NewPCG(BootstrapSeed, BootstrapSeed)) //nolint:gosec // G404: seeded, reproducible bootstrap, not security
	stats := make([]float64, BootstrapResamples)
	for b := range stats {
		var n, d float64
		for range cases {
			k := rng.IntN(len(cases))
			n += num[k]
			d += den[k]
		}
		stats[b] = safeDiv(n, d)
	}
	return ratio{value: round4(value), lo: round4(Percentile(stats, 2.5)), hi: round4(Percentile(stats, 97.5))}
}

func bootstrapPercentile(xs []float64, p float64) (lo, hi float64) {
	rng := rand.New(rand.NewPCG(BootstrapSeed, BootstrapSeed)) //nolint:gosec // G404: seeded, reproducible bootstrap, not security
	stats := make([]float64, 200)                              // the latency sample is large; 200 resamples bound the CI well enough
	sample := make([]float64, len(xs))
	for b := range stats {
		for i := range sample {
			sample[i] = xs[rng.IntN(len(xs))]
		}
		stats[b] = Percentile(sample, p)
	}
	return Percentile(stats, 2.5), Percentile(stats, 97.5)
}

func gate(r ratio, threshold float64, direction string) Metric {
	passed := r.value >= threshold
	if direction == Lower {
		passed = r.value <= threshold
	}
	return Metric{Value: r.value, Threshold: threshold, Direction: direction, Passed: passed, CILow: r.lo, CIHigh: r.hi}
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func sum(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s
}

func safeDiv(n, d float64) float64 {
	if d == 0 {
		return 0
	}
	return n / d
}

// Percentile returns the p-th percentile (nearest rank) of xs (0 for an empty slice).
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	k := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(k, len(s)-1))]
}
