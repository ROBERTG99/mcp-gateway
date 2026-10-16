package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
)

// cmdAudit implements `audit verify [--from-seq N]`: exit 0 when the chain verifies, 1 naming the
// first bad seq otherwise.
func cmdAudit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		_, _ = fmt.Fprintln(stderr, "usage: mcp-gateway audit verify [--from-seq N]")
		return 2
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.Int64("from-seq", 0, "start verification at the event with this seq")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer pool.Close()
	res, err := audit.VerifyPostgres(ctx, pool, *from)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if !res.OK {
		_, _ = fmt.Fprintf(stdout, "FAIL: audit chain broken at seq %d (%s) after %d valid events\n", res.FirstBadSeq, res.Reason, res.Checked)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "OK: verified %d events, head hash %s\n", res.Checked, res.HeadHash)
	return 0
}
