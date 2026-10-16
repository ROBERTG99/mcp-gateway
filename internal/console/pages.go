package console

import (
	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
)

// mountPages registers the session-protected pages with the scopes the matching admin API routes
// require (internal/admin.Mount); the approvals POSTs are further checked by Service.Decide.
func (c *Console) mountPages(r chi.Router) {
	inspect := c.requireScope(admin.ScopeAuditRead, admin.ScopeAdmin)
	r.With(inspect).Get("/audit", c.auditPage)
	r.With(inspect).Get("/audit/{seq}", c.auditEventPage)
	approvalsRead := c.requireScope(admin.ScopeApprove, admin.ScopeAuditRead, admin.ScopeAdmin)
	r.With(approvalsRead).Get("/approvals", c.approvalsPage)
	r.With(approvalsRead).Get("/approvals/{id}", c.approvalPage)
	r.With(approvalsRead).Post("/approvals/{id}/approve", c.decide(true))
	r.With(approvalsRead).Post("/approvals/{id}/reject", c.decide(false))
	r.With(inspect).Get("/policy", c.policyPage)
	r.With(inspect).Post("/policy", c.evaluatePolicy)
	r.With(inspect).Get("/upstreams", c.upstreamsPage)
}

func canSeeApprovals(p authn.Principal) bool {
	return p.HasScope(admin.ScopeApprove) || p.HasScope(admin.ScopeAuditRead) || p.HasScope(admin.ScopeAdmin)
}
