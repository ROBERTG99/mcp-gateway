package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
)

// cmdDevIssuer serves the development token issuer (refuses unless MCPGW_DEV_ISSUER_ENABLED=true).
func cmdDevIssuer(ctx context.Context, _ []string, _, stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	srv, err := devissuer.New(devissuer.Options{Enabled: cfg.DevIssuerEnabled, KeyFile: cfg.DevIssuerKeyFile})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	logger := newLogger(stderr, cfg.LogLevel)
	logger.Warn("dev issuer running: development only, never expose it", "addr", cfg.DevIssuerAddr)
	if err := devissuer.Serve(ctx, cfg.DevIssuerAddr, srv); err != nil {
		logger.Error("dev issuer stopped", "error", err)
		return 1
	}
	return 0
}

// cmdDevToken mints a token from a running dev issuer and prints it.
func cmdDevToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	issuer := fs.String("issuer", "http://localhost:8212", "dev issuer base URL")
	tenant := fs.String("tenant", "t_alpha", "tenant")
	agent := fs.String("agent", "agent-support", "agent id (sub)")
	scopes := fs.String("scopes", "", "comma-separated scopes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var list []string
	for s := range strings.SplitSeq(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	tok, err := devissuer.RequestToken(ctx, &http.Client{Timeout: 10 * time.Second}, *issuer, *tenant, *agent, list)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, tok)
	return 0
}
