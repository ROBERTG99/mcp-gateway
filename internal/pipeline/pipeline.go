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
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/proxy"
	"github.com/ROBERTG99/mcp-gateway/internal/ratelimit"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
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
	// Limiter enforces rate limits and budgets (Redis); LocalLimiter is the per-process fallback
	// for reads when Limiter fails. Writes fail closed (-32049).
	Limiter      ratelimit.Limiter
	LocalLimiter ratelimit.Limiter
	// Audit reserves a slot before any side effect and records one event per outcome.
	Audit Auditor
	// Redactor redacts arguments before they reach the audit log.
	Redactor *redact.Redactor
	// Approvals holds calls that need a human decision (nil: such calls are denied).
	Approvals Approvals
	// Metrics records Prometheus metrics (nil: none).
	Metrics *telemetry.Metrics
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Auditor reserves audit slots (audit.Writer).
type Auditor interface {
	Reserve(ctx context.Context) (*audit.Ticket, error)
}

// Options are the pipeline's tunables.
type Options struct {
	IdempotencyTTL     time.Duration // completed record lifetime (default 24 h)
	IdempotencyLockTTL time.Duration // in-progress lock lifetime (default 30 s)
	MaxStoredResult    int           // bytes; larger results are not stored for replay (default 256 KiB)
	InjectionThreshold float64       // score at or above which output is flagged (default 0.5)
	InjectionMode      string        // "mark" (default) or "block"
	Node               string        // app.node on spans and step records (default: hostname)
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
	if opts.Node == "" {
		opts.Node, _ = os.Hostname()
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

	auditSeq   int64 // allocated when the audit slot is reserved
	approvalID string

	idemKey  string
	acquired bool
	replayed bool
	upstream time.Duration // time spent in the upstream call (excluded from overhead)
	decision string        // allow | require_approval | deny | none
	status   string        // ok | replayed | held | denied | rejected | error
}

// CallTool runs one tools/call through every stage.
func (p *Pipeline) CallTool(ctx context.Context, pc proxy.Call) (*mcp.CallToolResult, error) {
	c := &call{principal: pc.Principal, tool: pc.Tool, req: pc.Request, start: p.deps.Clock.Now(),
		requestID: newRequestID(), decision: "none", status: "error"}
	// Audit first: without a slot nothing may happen (-32048, fail closed).
	var ticket *audit.Ticket
	if p.deps.Audit != nil {
		t, err := p.deps.Audit.Reserve(ctx)
		if err != nil {
			p.deps.Logger.Error("audit saturated; rejecting call", "request_id", c.requestID, "error", err)
			p.deps.Metrics.AuditRejected()
			return nil, mcperr.New(mcperr.CodeAuditBackpressure, nil)
		}
		ticket, c.auditSeq = t, t.Seq()
	}
	res, err := p.run(ctx, c)
	if ticket != nil {
		_, span := p.stage(ctx, c, telemetry.SpanAudit, attribute.Int64("mcpgw.audit_seq", c.auditSeq),
			attribute.String("mcpgw.status", c.status))
		ticket.Commit(p.auditEvent(c))
		span.End()
	}
	p.logStep(ctx, c, err)
	p.deps.Metrics.ToolCall(c.principal.Tenant, c.tool.Server, c.tool.Name, c.status)
	p.deps.Metrics.Overhead(p.deps.Clock.Now().Sub(c.start) - c.upstream)
	if c.replayed {
		p.deps.Metrics.Replay()
	}
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
	if err := p.rateLimit(ctx, c); err != nil {
		return nil, err
	}
	if !c.tool.ReadOnly() {
		key, err := c.idempotencyKey()
		if err != nil {
			c.status = "rejected"
			return nil, err
		}
		c.idemKey = key
	}
	pctx, span := p.stage(ctx, c, telemetry.SpanPolicy)
	res, done, err := p.authorize(pctx, c)
	span.SetAttributes(attribute.String("mcpgw.decision", c.decision))
	span.End()
	if c.decision != "none" {
		p.deps.Metrics.PolicyDecision(c.decision)
	}
	if done || err != nil {
		return res, err
	}
	if !c.tool.ReadOnly() {
		ictx, span := p.stage(ctx, c, telemetry.SpanIdempotency, attribute.String("mcpgw.op", "acquire"))
		res, done, err := p.acquire(ictx, c)
		span.SetAttributes(attribute.Bool("mcpgw.replayed", c.replayed))
		span.End()
		if done || err != nil {
			return res, err
		}
	}
	uctx, span := p.stage(ctx, c, telemetry.SpanUpstream, attribute.String("mcpgw.server", c.tool.Server),
		attribute.String("mcpgw.upstream_tool", c.tool.Name))
	upStart := p.deps.Clock.Now()
	res, err = p.forward(uctx, c)
	c.upstream = p.deps.Clock.Now().Sub(upStart)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upstream call failed")
	}
	span.End()
	if err != nil {
		p.release(ctx, c)
		c.status = "error"
		return nil, err
	}
	if c.acquired {
		ictx, span := p.stage(ctx, c, telemetry.SpanIdempotency, attribute.String("mcpgw.op", "complete"))
		if res.IsError {
			p.release(ictx, c) // a tool error is not a completed side effect: the key may be retried
		} else {
			p.complete(ictx, c, res)
		}
		span.End()
	}
	c.status = "ok"
	return p.finish(ctx, c, res), nil
}

// stage starts a child span of the request for one pipeline stage.
func (p *Pipeline) stage(ctx context.Context, c *call, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	attrs = append(attrs, telemetry.AttrTenant.String(c.principal.Tenant), telemetry.AttrNode.String(p.opts.Node))
	if c.runID != "" {
		attrs = append(attrs, telemetry.AttrRunID.String(c.runID))
	}
	return telemetry.Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// logStep logs the shared step record for the call (kind "tool", no model, zero tokens and cost).
func (p *Pipeline) logStep(ctx context.Context, c *call, err error) {
	rec := telemetry.StepRecord{RunID: c.runID, TaskID: c.requestID, Node: p.opts.Node, Kind: "tool", Status: c.status,
		LatencyMS: float64(p.deps.Clock.Now().Sub(c.start).Microseconds()) / 1000, StartedAt: c.start.UTC()}
	if err != nil {
		msg := err.Error()
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			msg = fmt.Sprintf("%d %s", je.Code, je.Message)
		}
		rec.Error = &msg
	}
	telemetry.LogStep(ctx, p.deps.Logger, rec)
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
	// tools/call always evaluates the policy with an arguments object: args = null is reserved
	// for tools/list, where argument rules must not fire.
	switch c.argsVal.(type) {
	case nil:
		c.args, c.argsVal = json.RawMessage("{}"), map[string]any{}
		c.argsHash, _ = canonicaljson.SHA256Hex(c.args)
	case map[string]any:
	default:
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments must be a JSON object"}
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

// auditEvent builds the event for the call's outcome, with redacted arguments.
func (p *Pipeline) auditEvent(c *call) audit.Event {
	args := json.RawMessage(`"[REDACTED:NO_REDACTOR]"`) // never store unredacted arguments
	if p.deps.Redactor != nil {
		args = p.deps.Redactor.JSON(c.args)
	}
	e := audit.Event{
		TS: c.start, Tenant: c.principal.Tenant, Agent: c.principal.Agent, Server: c.tool.Server, Tool: c.tool.Name,
		ArgsRedacted: args, ArgsSHA256: c.argsHash, Decision: c.decision, Status: c.status,
		LatencyMS: float64(p.deps.Clock.Now().Sub(c.start).Microseconds()) / 1000,
	}
	if e.ArgsSHA256 == "" {
		e.ArgsSHA256 = strings.Repeat("0", 64) // arguments were not valid JSON
	}
	if c.idemKey != "" {
		k := c.idemKey
		e.IdempotencyKey = &k
	}
	if c.approvalID != "" {
		id := c.approvalID
		e.ApprovalID = &id
	}
	return e
}

// rateLimit is the rate limit and budget stage.
func (p *Pipeline) rateLimit(ctx context.Context, c *call) error {
	if p.deps.Limiter == nil {
		return nil
	}
	v, err := p.deps.Limiter.Allow(ctx, c.principal.Tenant, c.principal.Agent)
	if err != nil {
		if !c.tool.ReadOnly() || p.deps.LocalLimiter == nil {
			p.deps.Logger.Error("rate limiter unavailable; refusing write", "request_id", c.requestID, "error", err)
			c.status = "error"
			return mcperr.New(mcperr.CodeDependencyUnavailable, map[string]any{"dependency": "rate_limiter"})
		}
		p.deps.Logger.Warn("rate limiter unavailable; reads use the local bucket", "request_id", c.requestID, "error", err)
		if v, err = p.deps.LocalLimiter.Allow(ctx, c.principal.Tenant, c.principal.Agent); err != nil {
			c.status = "error"
			return mcperr.New(mcperr.CodeDependencyUnavailable, map[string]any{"dependency": "rate_limiter"})
		}
	}
	switch {
	case v.Allowed:
		return nil
	case v.Reason == ratelimit.BudgetExhausted:
		c.status = "rejected"
		return mcperr.New(mcperr.CodeBudgetExhausted, map[string]any{"resets_at": v.ResetsAt.UTC().Format(time.RFC3339)})
	default:
		c.status = "rejected"
		return mcperr.New(mcperr.CodeRateLimited, map[string]any{"retry_after_ms": v.RetryAfter.Milliseconds()})
	}
}

// authorize is the policy and approval stage: deny -> -32044 with the reasons; require_approval
// -> the approval stage (held result, consumed approval, or -32045).
func (p *Pipeline) authorize(ctx context.Context, c *call) (*mcp.CallToolResult, bool, error) {
	scopes := c.principal.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	in := policy.Input{
		Tenant: c.principal.Tenant, Agent: c.principal.Agent, Scopes: scopes,
		Server: c.tool.Server, Tool: c.tool.Name, Args: c.argsVal, Annotations: c.tool.Annotations(),
		Now: p.deps.Clock.Now().UTC(),
	}
	d, err := p.deps.Policy.Evaluate(ctx, in)
	if err != nil {
		p.deps.Logger.Error("policy evaluation failed; denying", "request_id", c.requestID, "error", err)
	}
	switch {
	case err == nil && d.Allow:
		c.decision = "allow"
		return nil, false, nil
	case err == nil && d.RequireApproval:
		return p.approve(ctx, c, in, d)
	default:
		c.decision, c.status = "deny", "denied"
		return nil, true, mcperr.New(mcperr.CodePolicyDenied, map[string]any{"reasons": d.Reasons})
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
	telemetry.InjectMeta(ctx, meta) // W3C trace context for the upstream
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
		"audit_seq":         c.auditSeq,
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
