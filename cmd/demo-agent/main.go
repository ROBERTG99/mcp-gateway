// Command demo-agent is a scripted MCP client (no LLM) that drives the gateway.
package main

import (
	"context"
	"encoding/json"
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
	tool := fs.String("call", "", "call this tool once and print the result as JSON")
	argsJSON := fs.String("args", "{}", "tool arguments (JSON object) for --call")
	key := fs.String("key", "", "idempotency key for --call")
	approvalID := fs.String("approval-id", "", "approval id for --call (retry of a held call)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tool != "" {
		return callOnce(ctx, *gateway, *token, *tool, *argsJSON, *key, *approvalID, stdout, stderr)
	}
	if !*list {
		_, _ = fmt.Fprintln(stderr, "nothing to do (use --list or --call)")
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

func callOnce(ctx context.Context, gateway, token, tool, argsJSON, key, approvalID string, stdout, stderr io.Writer) int {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		_, _ = fmt.Fprintf(stderr, "--args: %v\n", err)
		return 2
	}
	session, err := connect(ctx, gateway, token)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "connect: %v\n", err)
		return 1
	}
	defer func() { _ = session.Close() }()
	meta := mcp.Meta{}
	if key != "" {
		meta["idempotency_key"] = key
	}
	if approvalID != "" {
		meta["approval_id"] = approvalID
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: meta})
	if err != nil {
		out, _ := json.Marshal(map[string]any{"error": err.Error()})
		_, _ = fmt.Fprintln(stdout, string(out))
		return 1
	}
	out, _ := json.Marshal(res)
	_, _ = fmt.Fprintln(stdout, string(out))
	return 0
}
