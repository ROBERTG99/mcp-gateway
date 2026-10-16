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
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
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
	scenario := fs.Bool("scenario", false, "run the scripted demo scenario (PASS/FAIL per step)")
	admin := fs.String("admin", envOr("MCPGW_ADMIN_URL", "http://localhost:8200"), "admin API base URL (--scenario)")
	issuer := fs.String("issuer", envOr("MCPGW_DEV_ISSUER_URL", "http://localhost:8212"), "dev issuer URL (--scenario)")
	webhook := fs.String("webhook-admin", "", "WireMock base URL to check the approval webhook (--scenario)")
	stall := fs.String("stall-prefix", "demo-stall-", "idempotency key prefix the orders upstream stalls on (--scenario)")
	onTimeout := fs.String("on-timeout-cmd", "", "shell command run after step 4's first call fails (--scenario)")
	tamperCmd := fs.String("tamper-cmd", "", "shell command that tampers with one audit row and prints its seq (--scenario step 7)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *scenario {
		cfg := demo.Config{GatewayURL: *gateway, AdminURL: *admin, IssuerURL: *issuer, WebhookAdminURL: *webhook,
			StallPrefix: *stall, Out: stdout}
		if *onTimeout != "" {
			cfg.OnTimeout = func(ctx context.Context) error { _, err := shell(ctx, *onTimeout, stderr); return err }
		}
		if *tamperCmd != "" {
			cfg.Tamper = func(ctx context.Context) (int64, error) {
				out, err := shell(ctx, *tamperCmd, stderr)
				if err != nil {
					return 0, err
				}
				return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
			}
		}
		if _, err := demo.Run(ctx, cfg); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
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

// shell runs an operator-supplied command line (demo hooks) and returns its stdout.
func shell(ctx context.Context, cmdline string, stderr io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline) //nolint:gosec // G204: the operator's own demo hook
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmdline, err)
	}
	return string(out), nil
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
