package console

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mountPages registers the session-protected pages.
func (c *Console) mountPages(r chi.Router) {
	r.Get("/audit", c.auditPage)
	r.Get("/approvals", c.approvalsPage)
	r.Get("/policy", c.policyPage)
	r.Get("/upstreams", c.upstreamsPage)
}

func (c *Console) auditPage(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "audit", view{Title: "Audit log", Nav: "audit"})
}

func (c *Console) approvalsPage(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "approvals", view{Title: "Approvals", Nav: "approvals"})
}

func (c *Console) policyPage(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "policy", view{Title: "Policy tester", Nav: "policy"})
}

func (c *Console) upstreamsPage(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "upstreams", view{Title: "Upstreams", Nav: "upstreams"})
}
