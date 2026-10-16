// Command dev-issuer runs the development JWT issuer (same as `mcp-gateway dev-issuer`).
// It refuses to start unless MCPGW_DEV_ISSUER_ENABLED=true.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	srv, err := devissuer.New(devissuer.Options{Enabled: cfg.DevIssuerEnabled, KeyFile: cfg.DevIssuerKeyFile})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := devissuer.Serve(ctx, cfg.DevIssuerAddr, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
