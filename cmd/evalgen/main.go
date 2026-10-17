// Command evalgen runs the offline eval suites and writes evalgate.report.v1 reports plus
// evals/REPORT.md:
//
//	evalgen run [--datasets evals/datasets] [--out evals/reports] [--baseline evals/baseline] [--report evals/REPORT.md]
//
// It calls no model (mode offline, models []). The chaos suite is written by cmd/loadgen.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/evals"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		_, _ = fmt.Fprintln(stderr, "usage: evalgen run [flags]")
		return 2
	}
	fs := flag.NewFlagSet("evalgen run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	datasets := fs.String("datasets", "evals/datasets", "datasets directory")
	out := fs.String("out", "evals/reports", "reports directory")
	baseline := fs.String("baseline", "evals/baseline", "baseline reports directory")
	reportMD := fs.String("report", "evals/REPORT.md", "Markdown report path (empty: skip)")
	policyDir := fs.String("policies", "policies", "policy bundle directory")
	live := fs.Bool("live", false, "rerun against a running stack (not implemented: see docs/evaluation.md)")
	_ = fs.String("gateway", "", "gateway URL (--live)")
	_ = fs.String("issuer", "", "dev issuer URL (--live)")
	_ = fs.String("admin", "", "admin URL (--live)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *live {
		_, _ = fmt.Fprintln(stdout, "estimated cost USD 0.00: no model is called; the live suites reuse the offline graders")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	engine, err := policy.New(ctx, policy.Options{Dir: *policyDir})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "policy:", err)
		return 1
	}
	hashes := map[string]string{"policy_bundle": engine.BundleHash()}
	for name, path := range map[string]string{"injection_detector": "internal/injection/injection.go", "redaction_detector": "internal/redact/redact.go"} {
		h, err := evals.FileSHA256(path)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		hashes[name] = h
	}
	r := &evals.Runner{Policy: engine, Scorer: injection.New(injection.Options{}), Redactor: redact.New(),
		InjectionThreshold: cfg.InjectionThreshold, GitSHA: evals.GitSHA(ctx), Now: time.Now(), PromptHashes: hashes}
	failed := false
	for _, split := range []string{"dev", "test"} {
		ds, err := evals.LoadDatasets(*datasets, split)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		lat, err := r.RunLatency(ctx, split, ds.Policy)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		for _, rep := range []evals.Report{r.RunPolicy(ctx, split, ds.Policy), r.RunInjection(split, ds.Injection), r.RunRedaction(split, ds.Redaction), lat} {
			if err := evals.WriteReport(*out, rep, split); err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 1
			}
			for name, m := range rep.Metrics {
				_, _ = fmt.Fprintf(stdout, "%-5s %-13s %-20s %.4f (threshold %s %.4f) passed=%v\n", split, rep.Suite, name, m.Value, m.Direction, m.Threshold, m.Passed)
				if split == "test" && !m.Passed {
					failed = true
				}
			}
		}
	}
	if *reportMD != "" {
		md, err := evals.Markdown(*out, *baseline)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.WriteFile(*reportMD, []byte(md), 0o600); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if failed {
		_, _ = fmt.Fprintln(stderr, "evalgen: a test-partition threshold failed")
		return 1
	}
	return 0
}
