// Command demo-agent is a scripted MCP client (no LLM) that drives the gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gateway := fs.String("gateway", envOr("MCPGW_GATEWAY_URL", "http://localhost:8210/mcp"), "gateway MCP endpoint")
	token := fs.String("token", os.Getenv("MCPGW_DEV_TOKEN"), "bearer token")
	list := fs.Bool("list", false, "list the tools visible to the token and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*list {
		_, _ = fmt.Fprintln(stderr, "nothing to do (use --list)")
		return 2
	}
	session, err := connect(ctx, *gateway, *token)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "connect: %v\n", err)
		return 1
	}
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "list tools: %v\n", err)
		return 1
	}
	for _, t := range res.Tools {
		_, _ = fmt.Fprintln(stdout, t.Name)
	}
	return 0
}

func connect(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-agent", Version: "v0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", url, err)
	}
	return session, nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
