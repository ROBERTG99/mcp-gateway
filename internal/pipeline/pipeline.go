// Package pipeline executes tools/call through the governance stages, in order:
// limits -> rate limit and budget -> policy -> approval -> idempotency acquire -> upstream call
// -> output marking and scoring -> idempotency complete -> redaction -> audit enqueue.
// Every outcome, including rejections, produces exactly one audit event.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/pkg/canonicaljson"
)

// Upstream forwards a call (catalog.Catalog in production).
type Upstream interface {
	Call(ctx context.Context, t catalog.Tool, args json.RawMessage, meta mcp.Meta) (*mcp.CallToolResult, error)
}

// Policy evaluates authorization decisions (policy.Engine in production).
type Policy interface {
	Evaluate(ctx context.Context, in policy.Input) (policy.Decision, error)
}

// Deps are the pipeline's collaborators. Upstream and Policy are required; the others are
// optional in tests (a nil stage is skipped) and always set by the server.
type Deps struct {
	Upstream    Upstream
	Policy      Policy
	Idempotency idempotency.Store
	Scorer      Scorer
	Findings    FindingSink
	Clock       clock.Clock
	Logger      *slog.Logger
}

// Options are the pipeline's tunables.
type Options struct {
	IdempotencyTTL     time.Duration // completed record lifetime (default 24 h)
	IdempotencyLockTTL time.Duration // in-progress lock lifetime (default 30 s)
	MaxStoredResult    int           // bytes; larger results are not stored for replay (default 256 KiB)
	InjectionThreshold float64       // score at or above which output is flagged (default 0.5)
	InjectionMode      string        // "mark" (default) or "block"
}

// Pipeline executes governed tool calls. It implements proxy.Caller.
type Pipeline struct {
	deps Deps
	opts Options
}

var _ proxy.Caller = (*Pipeline)(nil)

// New builds a pipeline.
func New(deps Deps, opts Options) *Pipeline {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.IdempotencyTTL == 0 {
		opts.IdempotencyTTL = 24 * time.Hour
	}
	if opts.IdempotencyLockTTL == 0 {
		opts.IdempotencyLockTTL = 30 * time.Second
	}
	if opts.MaxStoredResult == 0 {
		opts.MaxStoredResult = 256 << 10
	}
	if opts.InjectionThreshold == 0 {
		opts.InjectionThreshold = 0.5
	}
	if opts.InjectionMode == "" {
		opts.InjectionMode = "mark"
	}
	return &Pipeline{deps: deps, opts: opts}
}

// call is the per-request state threaded through the stages; it also becomes the audit event.
type call struct {
	principal authn.Principal
	tool      catalog.Tool
	req       *mcp.CallToolRequest
	start     time.Time
	requestID string
	runID     string

	args     json.RawMessage // raw arguments ("{}" when absent)
	argsHash string          // SHA-256 of RFC 8785 canonical args
	argsVal  any             // decoded arguments for policy

	auditSeq int64 // allocated when the audit slot is reserved

	idemKey  string
	acquired bool
	replayed bool
	decision string // allow | require_approval | deny | none
	status   string // ok | replayed | held | denied | rejected | error
}

// CallTool runs one tools/call through every stage.
func (p *Pipeline) CallTool(ctx context.Context, pc proxy.Call) (*mcp.CallToolResult, error) {
	c := &call{principal: pc.Principal, tool: pc.Tool, req: pc.Request, start: p.deps.Clock.Now(),
		requestID: newRequestID(), decision: "none", status: "error"}
	res, err := p.run(ctx, c)
	if err != nil {
		var je *jsonrpc.Error
		if !errors.As(err, &je) {
			// Unexpected internal failure: never leak details to the agent.
			p.deps.Logger.Error("tool call failed", "request_id", c.requestID, "tool", c.tool.FullName(), "error", err)
			err = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
		}
	}
	return res, err
}

func (p *Pipeline) run(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	if err := p.parseArgs(c); err != nil {
		c.status = "rejected"
		return nil, err
	}
	if err := p.authorize(ctx, c); err != nil {
		return nil, err
	}
	if !c.tool.ReadOnly() {
		res, done, err := p.acquire(ctx, c)
		if done || err != nil {
			return res, err
		}
	}
	res, err := p.forward(ctx, c)
	if err != nil {
		p.release(ctx, c)
		c.status = "error"
		return nil, err
	}
	if c.acquired {
		if res.IsError {
			p.release(ctx, c) // a tool error is not a completed side effect: the key may be retried
		} else {
			p.complete(ctx, c, res)
		}
	}
	c.status = "ok"
	return p.finish(ctx, c, res), nil
}

func (p *Pipeline) parseArgs(c *call) error {
	c.args = json.RawMessage("{}")
	if c.req != nil && c.req.Params != nil && len(c.req.Params.Arguments) > 0 {
		c.args = c.req.Params.Arguments
	}
	h, err := canonicaljson.SHA256Hex(c.args)
	if err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments must be I-JSON: " + err.Error()}
	}
	c.argsHash = h
	if err := json.Unmarshal(c.args, &c.argsVal); err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments are not valid JSON"}
	}
	if m := c.meta(); m != nil {
		c.runID, _ = m["run_id"].(string)
	}
	return nil
}

func (c *call) meta() mcp.Meta {
	if c.req == nil || c.req.Params == nil {
		return nil
	}
	return c.req.Params.Meta
}

// authorize is the policy stage: deny -> -32044 with the reasons.
func (p *Pipeline) authorize(ctx context.Context, c *call) error {
	scopes := c.principal.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	d, err := p.deps.Policy.Evaluate(ctx, policy.Input{
		Tenant: c.principal.Tenant, Agent: c.principal.Agent, Scopes: scopes,
		Server: c.tool.Server, Tool: c.tool.Name, Args: c.argsVal, Annotations: c.tool.Annotations(),
		Now: p.deps.Clock.Now().UTC(),
	})
	if err != nil {
		p.deps.Logger.Error("policy evaluation failed; denying", "request_id", c.requestID, "error", err)
	}
	switch {
	case err == nil && d.Allow:
		c.decision = "allow"
		return nil
	default:
		c.decision, c.status = "deny", "denied"
		return mcperr.New(mcperr.CodePolicyDenied, map[string]any{"reasons": d.Reasons})
	}
}

// forward calls the upstream with the gateway context in _meta.
func (p *Pipeline) forward(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	meta := mcp.Meta{"mcpgw": map[string]any{"tenant": c.principal.Tenant, "agent": c.principal.Agent, "request_id": c.requestID}}
	if c.idemKey != "" {
		meta["idempotency_key"] = c.idemKey
	}
	if c.runID != "" {
		meta["run_id"] = c.runID
	}
	res, err := p.deps.Upstream.Call(ctx, c.tool, c.args, meta)
	if err != nil {
		if errors.Is(err, catalog.ErrUpstreamUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			return nil, mcperr.New(mcperr.CodeUpstreamUnavailable, map[string]any{"server": c.tool.Server})
		}
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			return nil, je // the upstream's own protocol error (e.g. invalid params)
		}
		return nil, fmt.Errorf("upstream %s: %w", c.tool.FullName(), err)
	}
	return res, nil
}

// finish marks the output as untrusted, scores it and adds the gateway's _meta.
func (p *Pipeline) finish(ctx context.Context, c *call, res *mcp.CallToolResult) *mcp.CallToolResult {
	out, score := p.mark(ctx, c, res)
	meta := mcp.Meta{}
	for k, v := range res.Meta {
		meta[k] = v
	}
	meta["mcpgw"] = map[string]any{
		"idempotent_replay": c.replayed,
		"request_id":        c.requestID,
		"injection":         map[string]any{"score": score.Score, "signals": score.Signals},
	}
	out.Meta = meta
	return out
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}
