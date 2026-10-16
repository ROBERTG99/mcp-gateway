package pipeline

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

// idempotencyKey reads the key from _meta.idempotency_key or the Idempotency-Key header; both
// present must match. Missing or malformed keys are -32041.
func (c *call) idempotencyKey() (string, error) {
	var fromMeta, fromHeader string
	if m := c.meta(); m != nil {
		fromMeta, _ = m["idempotency_key"].(string)
	}
	if c.req != nil && c.req.Extra != nil && c.req.Extra.Header != nil {
		fromHeader = c.req.Extra.Header.Get("Idempotency-Key")
	}
	key := fromMeta
	if key == "" {
		key = fromHeader
	}
	switch {
	case key == "":
		return "", mcperr.New(mcperr.CodeIdempotencyKeyRequired, map[string]any{"reason": "missing"})
	case fromMeta != "" && fromHeader != "" && fromMeta != fromHeader:
		return "", mcperr.New(mcperr.CodeIdempotencyKeyRequired, map[string]any{"reason": "header_meta_mismatch"})
	case !mcpserver.ValidKey(key):
		return "", mcperr.New(mcperr.CodeIdempotencyKeyRequired, map[string]any{"reason": "invalid_format"})
	}
	return key, nil
}

// storedResult is what the gateway keeps for replay. Results larger than MaxStoredResult are
// recorded as completed without a body; a replay then forwards to the upstream, whose own dedup
// returns the stored result (second idempotency layer).
type storedResult struct {
	Result    *mcp.CallToolResult `json:"result,omitempty"`
	Oversized bool                `json:"oversized,omitempty"`
}

// acquire claims the key. done=true means the call is finished (replayed) without the upstream.
func (p *Pipeline) acquire(ctx context.Context, c *call) (*mcp.CallToolResult, bool, error) {
	key, err := c.idempotencyKey()
	if err != nil {
		c.status = "rejected"
		return nil, true, err
	}
	c.idemKey = key
	if p.deps.Idempotency == nil {
		return nil, false, nil
	}
	a, err := p.deps.Idempotency.Acquire(ctx, c.principal.Tenant, key, c.argsHash, p.opts.IdempotencyLockTTL)
	if err != nil {
		p.deps.Logger.Error("idempotency store unavailable; refusing write", "request_id", c.requestID, "error", err)
		c.status = "error"
		return nil, true, mcperr.New(mcperr.CodeDependencyUnavailable, map[string]any{"dependency": "idempotency_store"})
	}
	switch a.Outcome {
	case idempotency.Acquired:
		c.acquired = true
		return nil, false, nil
	case idempotency.Replay:
		var sr storedResult
		if err := json.Unmarshal(a.Result, &sr); err != nil || sr.Oversized || sr.Result == nil {
			return nil, false, nil // forward: the upstream replays its own stored result
		}
		c.replayed, c.status = true, "replayed"
		return p.finish(ctx, c, sr.Result), true, nil
	case idempotency.InProgress:
		c.status = "rejected"
		return nil, true, mcperr.New(mcperr.CodeIdempotencyInProgress, nil)
	default:
		c.status = "rejected"
		return nil, true, mcperr.New(mcperr.CodeIdempotencyConflict, nil)
	}
}

func (p *Pipeline) complete(ctx context.Context, c *call, res *mcp.CallToolResult) {
	if p.deps.Idempotency == nil {
		return
	}
	stored := storedResult{Result: res}
	raw, err := json.Marshal(stored)
	if err != nil || len(raw) > p.opts.MaxStoredResult {
		raw, _ = json.Marshal(storedResult{Oversized: true})
	}
	if err := p.deps.Idempotency.Complete(context.WithoutCancel(ctx), c.principal.Tenant, c.idemKey, c.argsHash, raw, p.opts.IdempotencyTTL); err != nil {
		// The upstream committed; its own dedup still protects a retry.
		p.deps.Logger.Warn("idempotency complete failed", "request_id", c.requestID, "error", err)
	}
}

func (p *Pipeline) release(ctx context.Context, c *call) {
	if !c.acquired || p.deps.Idempotency == nil {
		return
	}
	if err := p.deps.Idempotency.Release(context.WithoutCancel(ctx), c.principal.Tenant, c.idemKey, c.argsHash); err != nil {
		p.deps.Logger.Warn("idempotency release failed", "request_id", c.requestID, "error", err)
	}
	c.acquired = false
}
