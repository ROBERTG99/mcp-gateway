// Command datagen writes the synthetic dataset (datagen.v1, seed 20260).
//
//	datagen hash                       print the dataset hash
//	datagen seed [--database-url URL]  load the dataset into the upstream schemas (idempotent)
//	datagen evals --out evals/datasets write the labelled eval datasets
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: datagen hash|seed|evals [flags]")
		return 2
	}
	fs := flag.NewFlagSet("datagen "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Uint64("seed", datagen.DefaultSeed, "generator seed")
	dbURL := fs.String("database-url", os.Getenv("MCPGW_UPSTREAM_DATABASE_URL"), "Postgres URL (seed)")
	out := fs.String("out", "evals/datasets", "output directory (evals)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	d := datagen.Generate(*seed)
	switch args[0] {
	case "hash":
		_, _ = fmt.Fprintf(stdout, "%s seed=%d sha256=%s\n", d.Schema, d.Seed, d.Hash())
	case "seed":
		pool, err := store.Open(ctx, *dbURL)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		defer pool.Close()
		if err := datagen.Seed(ctx, pool, d); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		c, err := datagen.CountRows(ctx, pool)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "seeded %s sha256=%s accounts=%d customers=%d orders=%d notes=%d\n",
			d.Schema, d.Hash(), c.Accounts, c.Customers, c.Orders, c.Notes)
	case "evals":
		n, err := datagen.WriteEvalDatasets(*out, *seed)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "wrote %d eval files to %s\n", n, *out)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
	return 0
}
