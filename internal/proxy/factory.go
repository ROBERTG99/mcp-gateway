package proxy

import (
	"context"

	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

// PolicyVisibility lists a tool when the policy, evaluated with args = null (argument rules do
// not fire), allows it or requires approval for it. Any evaluation error hides the tool.
func PolicyVisibility(engine *policy.Engine) Visibility {
	return func(ctx context.Context, p authn.Principal, t catalog.Tool) bool {
		d, err := engine.Evaluate(ctx, ListInput(p, t))
		return err == nil && (d.Allow || d.RequireApproval)
	}
}

// ListInput is the policy input for listing a tool.
func ListInput(p authn.Principal, t catalog.Tool) policy.Input {
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return policy.Input{
		Tenant: p.Tenant, Agent: p.Agent, Scopes: scopes, Server: t.Server, Tool: t.Name,
		Args: nil, Annotations: t.Annotations(),
	}
}
