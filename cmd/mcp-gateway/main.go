// Command mcp-gateway is the governed MCP gateway: it serves the aggregated
// MCP endpoint and admin console, and bundles operational subcommands.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/ROBERTG99/mcp-gateway/internal/version"
)

const usage = `usage: mcp-gateway <command> [flags]

commands:
  serve                 run the gateway (MCP :8210, admin :8200, metrics :8211)
  dev-issuer            run the development JWT issuer (:8212)
  migrate [up|down|status]  apply database migrations
  audit verify [--from-seq N]  verify the audit hash chain
  policy test <dir>     run Rego unit tests
  policy fmt [--check] <dir>  format Rego files
  healthcheck [url]     probe a health endpoint (container HEALTHCHECK)
  version, --version    print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches a subcommand and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	if handler, ok := commands()[cmd]; ok {
		return handler(ctx, rest, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", cmd, usage)
	return 2
}

type commandFunc func(ctx context.Context, args []string, stdout, stderr io.Writer) int

func commands() map[string]commandFunc {
	return map[string]commandFunc{
		"serve":       cmdServe,
		"healthcheck": cmdHealthcheck,
		"version":     cmdVersion,
		"--version":   cmdVersion,
		"-version":    cmdVersion,
		"help":        cmdHelp,
		"--help":      cmdHelp,
		"-h":          cmdHelp,
	}
}

func cmdVersion(_ context.Context, _ []string, stdout, _ io.Writer) int {
	_, _ = fmt.Fprintln(stdout, version.String())
	return 0
}

func cmdHelp(_ context.Context, _ []string, stdout, _ io.Writer) int {
	_, _ = fmt.Fprint(stdout, usage)
	return 0
}
