package console

import (
	"github.com/go-chi/chi/v5"
)

// mountPages registers the session-protected pages.
func (c *Console) mountPages(r chi.Router) {
	r.Get("/audit", c.auditPage)
	r.Get("/audit/{seq}", c.auditEventPage)
	r.Get("/approvals", c.approvalsPage)
	r.Get("/approvals/{id}", c.approvalPage)
	r.Post("/approvals/{id}/approve", c.decide(true))
	r.Post("/approvals/{id}/reject", c.decide(false))
	r.Get("/policy", c.policyPage)
	r.Post("/policy", c.evaluatePolicy)
	r.Get("/upstreams", c.upstreamsPage)
}
