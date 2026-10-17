// Command loadgen drives the gateway over MCP.
//
//	loadgen --mode chaos --gateway URL --issuer URL --admin URL --calls 10000 [--out evals/reports]
//
// Chaos mode (spec §11, chaos.v1) sends refunds with idempotency keys while the orders upstream
// stalls a fraction of them past the gateway timeout (MCPGW_FAULT_TIMEOUT_RATE on upstream-orders),
// retries like an agent, re-sends 20% of keys on purpose, and checks that every key executed once
// and every call was audited. It writes <out>/chaos.v1.json (evalgate.report.v1).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/evals"
	"github.com/ROBERTG99/mcp-gateway/internal/loadgen"
)

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "chaos", "chaos")
	gateway := fs.String("gateway", "http://localhost:8210/mcp", "gateway MCP URL")
	issuer := fs.String("issuer", "http://localhost:8212", "dev issuer URL")
	admin := fs.String("admin", "http://localhost:8200", "admin API URL")
	calls := fs.Int("calls", 10000, "logical calls")
	concurrency := fs.Int("concurrency", 64, "parallel workers")
	out := fs.String("out", "evals/reports", "report directory (empty: no report)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "chaos" {
		_, _ = fmt.Fprintf(stderr, "unknown mode %q\n", *mode)
		return 2
	}
	res, err := loadgen.Chaos(ctx, loadgen.ChaosConfig{GatewayURL: strings.TrimSuffix(*gateway, "/"), IssuerURL: *issuer,
		AdminURL: *admin, Calls: *calls, Concurrency: *concurrency, Out: stdout})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos:", err)
		return 1
	}
	if *out != "" {
		rep := chaosReport(ctx, res)
		if err := evals.WriteReport(*out, rep, "test"); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if res.Duplicates != 0 || res.Lost != 0 || res.Unanswered != 0 || res.AuditedRatio != 1 || !res.ChainOK {
		_, _ = fmt.Fprintln(stderr, "chaos: gate failed (want 0 duplicates, 0 lost, 0 unanswered, audited ratio 1.0, chain ok)")
		return 1
	}
	return 0
}

// chaosReport turns a run into an evalgate.report.v1 document; each case is one order (its refund
// increase against the keys sent to it).
func chaosReport(ctx context.Context, r loadgen.ChaosResult) evals.Report {
	cases := make([]evals.Case, 0, len(r.PerOrder))
	for _, o := range r.PerOrder {
		cases = append(cases, evals.Case{ID: o.OrderID, Split: "test", Passed: o.Duplicates == 0 && o.Increase == o.Keys,
			Scores: map[string]float64{"keys": float64(o.Keys), "increase_cents": float64(o.Increase), "duplicates": float64(o.Duplicates)}})
	}
	now := time.Now().UTC()
	return evals.Report{Schema: evals.ReportSchema, Suite: "chaos.v1", RunID: "chaos-" + r.RunID, CreatedAt: now.Format(time.RFC3339),
		GitSHA: evals.GitSHA(ctx), Mode: "live", Models: []string{}, PromptHashes: map[string]string{}, NCases: len(cases),
		Metrics: map[string]evals.Metric{
			"duplicates":    {Value: float64(r.Duplicates), Threshold: 0, Direction: evals.Lower, Passed: r.Duplicates == 0, CILow: float64(r.Duplicates), CIHigh: float64(r.Duplicates)},
			"audited_ratio": {Value: r.AuditedRatio, Threshold: 1, Direction: evals.Higher, Passed: r.AuditedRatio == 1, CILow: r.AuditedRatio, CIHigh: r.AuditedRatio},
		},
		LatencyP50MS: r.LatencyP50MS, LatencyP95MS: r.LatencyP95MS, Cases: cases,
		Extensions: map[string]any{"calls": r.Calls, "attempts": r.Attempts, "retries": r.Retries, "deliberate_duplicates": r.DeliberateDups,
			"timeouts": r.Timeouts, "breaker_rejects": r.FastFails, "in_progress": r.InProgress, "other_errors": r.OtherErrors,
			"lost": r.Lost, "unanswered": r.Unanswered, "audit_events": r.AuditEvents, "chain_ok": r.ChainOK, "duration_s": r.Duration.Seconds(),
			"fault_timeout_rate": os.Getenv("MCPGW_FAULT_TIMEOUT_RATE")},
	}
}
