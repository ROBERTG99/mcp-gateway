package pipeline

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

// Approvals is the approval service (approvals.Service).
type Approvals interface {
	Hold(ctx context.Context, r approvals.HoldRequest) (approvals.Approval, error)
	Consume(ctx context.Context, id string, b approvals.Binding) (approvals.Approval, string, error)
}

func (c *call) binding() approvals.Binding {
	return approvals.Binding{Tenant: c.principal.Tenant, Agent: c.principal.Agent, Server: c.tool.Server,
		Tool: c.tool.Name, ArgsSHA256: c.argsHash, IdempotencyKey: c.idemKey}
}

// approve is the approval stage for a require_approval decision. It returns a held result
// (done=true), or nil to continue (the approval was valid and consumed), or a -32045 error.
func (p *Pipeline) approve(ctx context.Context, c *call, in policy.Input, d policy.Decision) (*mcp.CallToolResult, bool, error) {
	c.decision = "require_approval"
	if p.deps.Approvals == nil {
		c.status = "denied"
		return nil, true, mcperr.New(mcperr.CodePolicyDenied, map[string]any{"reasons": append(d.Reasons, "approvals_unavailable")})
	}
	id, _ := c.meta()["approval_id"].(string)
	if id == "" {
		a, err := p.deps.Approvals.Hold(ctx, approvals.HoldRequest{Binding: c.binding(), Args: c.args, Reasons: d.Reasons})
		if err != nil {
			p.deps.Logger.Error("approval store unavailable", "request_id", c.requestID, "error", err)
			c.status = "error"
			return nil, true, mcperr.New(mcperr.CodeDependencyUnavailable, map[string]any{"dependency": "approvals"})
		}
		c.approvalID, c.status = a.ID, "held"
		return p.held(c, a.ID, d.Reasons), true, nil
	}
	a, reason, err := p.deps.Approvals.Consume(ctx, id, c.binding())
	if err != nil {
		p.deps.Logger.Error("approval store unavailable", "request_id", c.requestID, "error", err)
		c.status = "error"
		return nil, true, mcperr.New(mcperr.CodeDependencyUnavailable, map[string]any{"dependency": "approvals"})
	}
	if a.ID != "" && a.Tenant == c.principal.Tenant {
		c.approvalID = a.ID
	}
	switch {
	case reason == approvals.ReasonPending:
		c.status = "held"
		return p.held(c, a.ID, a.Reasons), true, nil
	case reason == "",
		// A consumed approval authorises its own call only: the same-key retry continues to the
		// idempotency stage, which replays (or, after a failed attempt, re-executes under the
		// same key, where upstream dedup still guarantees one write).
		reason == approvals.ReasonConsumed && a.Matches(c.binding()):
		in.Approval = &policy.Approval{ID: a.ID, Status: approvals.StatusApproved}
		d2, err := p.deps.Policy.Evaluate(ctx, in)
		if err != nil || !d2.Allow {
			c.status = "denied"
			return nil, true, mcperr.New(mcperr.CodePolicyDenied, map[string]any{"reasons": d2.Reasons})
		}
		c.decision = "allow"
		return nil, false, nil
	default:
		c.status = "rejected"
		return nil, true, mcperr.New(mcperr.CodeApprovalInvalid, map[string]any{"reason": reason})
	}
}

// held is the approval_required result (contract v0.1.0), returned again while pending.
func (p *Pipeline) held(c *call, id string, reasons []string) *mcp.CallToolResult {
	if reasons == nil {
		reasons = []string{}
	}
	body := map[string]any{"status": "approval_required", "approval_id": id, "reasons": reasons}
	text, _ := json.Marshal(body)
	return &mcp.CallToolResult{
		IsError:           true,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
		StructuredContent: body,
		Meta: mcp.Meta{"mcpgw": map[string]any{"audit_seq": c.auditSeq, "request_id": c.requestID,
			"idempotent_replay": false}},
	}
}
