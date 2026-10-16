// Package demo is the scripted (no LLM) demo scenario of spec §16, run by `demo-agent --scenario`
// against a running gateway: only allowed tools are listed; a USD 50 refund executes; a USD 500
// refund is held, approved through the admin API and the retry executes; an orders call fails
// mid-flight and the same-key retry leaves one refund; an injected CRM note is flagged; the audit
// chain verifies. Step 7 (tampering) is driven by scripts/demo.sh through Config.Tamper.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
)

// Config locates the stack and customises the scenario.
type Config struct {
	GatewayURL      string // .../mcp
	AdminURL        string
	IssuerURL       string
	WebhookAdminURL string // WireMock base URL (optional): checks the approval webhook arrived
	StallPrefix     string // orders keys with this prefix stall mid-call (upstream fault hook)
	RunID           string // makes idempotency keys unique per run
	Out             io.Writer
	// OnTimeout runs after step 4's first call failed (scripts/demo.sh restarts the upstream).
	OnTimeout func(ctx context.Context) error
	// Tamper modifies one audit row and returns its seq (step 7); nil skips the step.
	Tamper func(ctx context.Context) (int64, error)
	HTTP   *http.Client
}

// Result is the outcome of one step.
type Result struct {
	Step   int    `json:"step"`
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// AgentScopes are the demo agent's scopes: CRM read, orders read and write (no ledger).
func AgentScopes() []string { return []string{"crm:read", "orders:read", "orders:write"} }

type runner struct {
	cfg      Config
	agent    *mcp.ClientSession
	approver string
	reviewer string
	order    string
}

// Run executes the scenario and prints PASS/FAIL per step. It returns an error if a step failed.
func Run(ctx context.Context, cfg Config) ([]Result, error) {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.RunID == "" {
		cfg.RunID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	r := &runner{cfg: cfg}
	agentTok, err := devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, datagen.DemoTenant, "agent-support", AgentScopes())
	if err != nil {
		return nil, fmt.Errorf("agent token: %w", err)
	}
	if r.approver, err = devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, datagen.DemoTenant, "jo.approver",
		[]string{"mcpgw:approve", "mcpgw:audit:read"}); err != nil {
		return nil, fmt.Errorf("approver token: %w", err)
	}
	if r.reviewer, err = devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, "*", "sam.security",
		[]string{"mcpgw:audit:read"}); err != nil {
		return nil, fmt.Errorf("reviewer token: %w", err)
	}
	if r.agent, err = Connect(ctx, cfg.GatewayURL, agentTok); err != nil {
		return nil, err
	}
	defer func() { _ = r.agent.Close() }()

	steps := []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{"only allowed tools are listed", r.listTools},
		{"USD 50 refund executes", r.smallRefund},
		{"USD 500 refund is held, approved and the retry executes once", r.heldRefund},
		{"orders call fails mid-flight; the same-key retry leaves one refund", r.failedThenRetried},
		{"injected CRM note is flagged as untrusted", r.injection},
		{"audit chain verifies", r.verify},
	}
	if cfg.Tamper != nil {
		steps = append(steps, struct {
			name string
			fn   func(context.Context) (string, error)
		}{"tampered audit row is detected at its seq", r.tamper})
	}
	var results []Result
	failed := false
	for i, s := range steps {
		detail, err := s.fn(ctx)
		res := Result{Step: i + 1, Name: s.name, Pass: err == nil, Detail: detail}
		if err != nil {
			res.Detail, failed = err.Error(), true
			_, _ = fmt.Fprintf(cfg.Out, "FAIL step %d: %s: %s\n", res.Step, s.name, res.Detail)
		} else {
			_, _ = fmt.Fprintf(cfg.Out, "PASS step %d: %s (%s)\n", res.Step, s.name, detail)
		}
		results = append(results, res)
		if failed {
			break
		}
	}
	if failed {
		return results, errors.New("demo scenario failed")
	}
	return results, nil
}

// Connect opens an MCP session with a bearer token.
func Connect(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "demo-agent", Version: "v0.1.0"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", url, err)
	}
	return s, nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func (r *runner) call(ctx context.Context, tool string, args map[string]any, meta mcp.Meta) (map[string]any, *mcp.CallToolResult, int64, error) {
	res, err := r.agent.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: meta})
	if err != nil {
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			return nil, nil, je.Code, nil
		}
		return nil, nil, 0, fmt.Errorf("%s: %w", tool, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var sc map[string]any
	_ = json.Unmarshal(raw, &sc)
	return sc, res, 0, nil
}

func (r *runner) refunded(ctx context.Context) (int64, int64, error) {
	sc, _, code, err := r.call(ctx, "orders.get_order", map[string]any{"order_id": r.order}, nil)
	if err != nil || code != 0 {
		return 0, 0, fmt.Errorf("get_order %s: code %d %w", r.order, code, err)
	}
	amount, _ := sc["amount_cents"].(float64)
	refunded, _ := sc["refunded_cents"].(float64)
	return int64(amount), int64(refunded), nil
}

func (r *runner) listTools(ctx context.Context) (string, error) {
	res, err := r.agent.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list tools: %w", err)
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	want := []string{"crm.get_customer", "orders.get_order", "orders.refund_order"}
	if !slices.Equal(names, want) {
		return "", fmt.Errorf("listed %v, want %v", names, want)
	}
	// Pick an order of the demo tenant with enough unrefunded amount for this run.
	for i := 1; i <= 200; i++ {
		r.order = fmt.Sprintf("O-%05d", i)
		amount, refunded, err := r.refunded(ctx)
		if err == nil && amount-refunded >= 60000 {
			return strings.Join(names, ", "), nil
		}
	}
	return "", errors.New("no order with 600 USD left to refund")
}

func (r *runner) refund(ctx context.Context, amount int64, key, approvalID string) (map[string]any, *mcp.CallToolResult, int64, error) {
	meta := mcp.Meta{"idempotency_key": key}
	if approvalID != "" {
		meta["approval_id"] = approvalID
	}
	return r.call(ctx, "orders.refund_order", map[string]any{"order_id": r.order, "amount_cents": amount, "reason": "demo: damaged item"}, meta)
}

func (r *runner) smallRefund(ctx context.Context) (string, error) {
	_, before, err := r.refunded(ctx)
	if err != nil {
		return "", err
	}
	sc, res, code, err := r.refund(ctx, 5000, "demo-small-"+r.cfg.RunID, "")
	if err != nil || code != 0 || res.IsError {
		return "", fmt.Errorf("refund 5000: code %d err %w result %v", code, err, sc)
	}
	_, after, err := r.refunded(ctx)
	if err != nil || after-before != 5000 {
		return "", fmt.Errorf("refunded delta = %d, want 5000", after-before)
	}
	return fmt.Sprintf("%s refund_id %v", r.order, sc["refund_id"]), nil
}

func (r *runner) heldRefund(ctx context.Context) (string, error) {
	_, before, err := r.refunded(ctx)
	if err != nil {
		return "", err
	}
	key := "demo-held-" + r.cfg.RunID
	sc, res, code, err := r.refund(ctx, 50000, key, "")
	if err != nil || code != 0 || !res.IsError || sc["status"] != "approval_required" {
		return "", fmt.Errorf("expected approval_required, got code %d err %w result %v", code, err, sc)
	}
	id, _ := sc["approval_id"].(string)
	if r.cfg.WebhookAdminURL != "" {
		if err := r.waitWebhook(ctx, id); err != nil {
			return "", err
		}
	}
	if err := r.admin(ctx, http.MethodPost, "/admin/approvals/"+id+":approve", r.approver, map[string]any{"note": "demo approval"}, nil); err != nil {
		return "", fmt.Errorf("approve: %w", err)
	}
	sc, res, code, err = r.refund(ctx, 50000, key, id)
	if err != nil || code != 0 || res.IsError {
		return "", fmt.Errorf("approved retry: code %d err %w result %v", code, err, sc)
	}
	// A further retry is a replay, never a second refund.
	if _, res2, code, err := r.refund(ctx, 50000, key, id); err != nil || code != 0 || res2.IsError {
		return "", fmt.Errorf("second retry: code %d err %w", code, err)
	}
	_, after, err := r.refunded(ctx)
	if err != nil || after-before != 50000 {
		return "", fmt.Errorf("refunded delta = %d, want 50000", after-before)
	}
	return "approval " + id, nil
}

func (r *runner) waitWebhook(ctx context.Context, approvalID string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var out struct {
			Requests []struct {
				Request struct {
					Body string `json:"body"`
				} `json:"request"`
			} `json:"requests"`
		}
		if err := r.get(ctx, r.cfg.WebhookAdminURL+"/__admin/requests", "", &out); err == nil {
			for _, rq := range out.Requests {
				if strings.Contains(rq.Request.Body, approvalID) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("approval webhook for %s not received", approvalID)
}

func (r *runner) failedThenRetried(ctx context.Context) (string, error) {
	_, before, err := r.refunded(ctx)
	if err != nil {
		return "", err
	}
	key := r.cfg.StallPrefix + r.cfg.RunID
	_, _, code, err := r.refund(ctx, 1000, key, "")
	if err != nil || code != mcperr.CodeUpstreamUnavailable {
		return "", fmt.Errorf("first call: code %d err %w, want -32046 (upstream unavailable)", code, err)
	}
	if r.cfg.OnTimeout != nil {
		if err := r.cfg.OnTimeout(ctx); err != nil {
			return "", fmt.Errorf("on-timeout hook: %w", err)
		}
	}
	attempts := 1
	deadline := time.Now().Add(90 * time.Second)
	for {
		attempts++
		_, res, code, err := r.refund(ctx, 1000, key, "")
		if err == nil && code == 0 && !res.IsError {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("retry never succeeded (last code %d, err %w)", code, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	_, after, err := r.refunded(ctx)
	if err != nil || after-before != 1000 {
		return "", fmt.Errorf("refunded delta = %d after %d attempts, want exactly 1000", after-before, attempts)
	}
	return fmt.Sprintf("one refund after %d attempts with key %s", attempts, key), nil
}

func (r *runner) injection(ctx context.Context) (string, error) {
	_, res, code, err := r.call(ctx, "crm.get_customer", map[string]any{"customer_id": datagen.DemoInjectedCustomer}, nil)
	if err != nil || code != 0 {
		return "", fmt.Errorf("get_customer: code %d err %w", code, err)
	}
	raw, _ := json.Marshal(res.Meta)
	var meta struct {
		MCPGW struct {
			Injection struct {
				Score   float64  `json:"score"`
				Signals []string `json:"signals"`
			} `json:"injection"`
		} `json:"mcpgw"`
	}
	_ = json.Unmarshal(raw, &meta)
	inj := meta.MCPGW.Injection
	if inj.Score < 0.5 || !slices.Contains(inj.Signals, "override_phrase") || !slices.Contains(inj.Signals, "tool_reference") {
		return "", fmt.Errorf("injection = %+v, want score >= 0.5 with override_phrase and tool_reference", inj)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "<<untrusted tool output>>") {
		return "", errors.New("output not marked untrusted")
	}
	return fmt.Sprintf("score %.2f signals %s", inj.Score, strings.Join(inj.Signals, ",")), nil
}

type verifyResult struct {
	OK          bool   `json:"ok"`
	Checked     int    `json:"checked"`
	FirstBadSeq int64  `json:"first_bad_seq"`
	Reason      string `json:"reason"`
}

func (r *runner) verify(ctx context.Context) (string, error) {
	var v verifyResult
	if err := r.get(ctx, r.cfg.AdminURL+"/admin/audit/verify", r.reviewer, &v); err != nil {
		return "", err
	}
	if !v.OK {
		return "", fmt.Errorf("chain broken at seq %d: %s", v.FirstBadSeq, v.Reason)
	}
	return fmt.Sprintf("%d events verified", v.Checked), nil
}

func (r *runner) tamper(ctx context.Context) (string, error) {
	seq, err := r.cfg.Tamper(ctx)
	if err != nil {
		return "", fmt.Errorf("tamper: %w", err)
	}
	var v verifyResult
	if err := r.get(ctx, r.cfg.AdminURL+"/admin/audit/verify", r.reviewer, &v); err != nil {
		return "", err
	}
	if v.OK || v.FirstBadSeq != seq {
		return "", fmt.Errorf("verify after tampering seq %d: ok=%v first_bad_seq=%d", seq, v.OK, v.FirstBadSeq)
	}
	return fmt.Sprintf("verify failed at seq %d as expected", seq), nil
}

func (r *runner) get(ctx context.Context, url, token string, out any) error {
	return r.do(ctx, http.MethodGet, url, token, nil, out)
}

func (r *runner) admin(ctx context.Context, method, path, token string, body, out any) error {
	return r.do(ctx, method, r.cfg.AdminURL+path, token, body, out)
}

func (r *runner) do(ctx context.Context, method, url, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s: %w", url, err)
		}
	}
	return nil
}
