package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
)

// approvalStatuses are the list filters; "pending" is the default queue.
var approvalStatuses = []string{"pending", "approved", "rejected", "expired", "consumed", "all"}

type approvalsView struct {
	Status    string
	Statuses  []string
	Approvals []approvalRow
}

type approvalRow struct {
	approvals.Approval
	ExpiresIn string
}

type approvalView struct {
	approvals.Approval
	ExpiresIn    string
	CanDecide    bool
	ApproveNote  string // what was typed in each form, kept when the page is shown again
	RejectReason string
}

// mayDecide mirrors approvals.Service.Decide: mcpgw:approve and the approval's own tenant.
func mayDecide(p authn.Principal, a approvals.Approval) bool {
	return p.HasScope(admin.ScopeApprove) && p.Tenant != "*" && p.Tenant == a.Tenant
}

// canDecide reports whether the decision forms are shown.
func canDecide(p authn.Principal, a approvals.Approval) bool {
	return mayDecide(p, a) && a.Status == approvals.StatusPending
}

func (c *Console) expiresIn(a approvals.Approval) string {
	if a.Status != approvals.StatusPending {
		return "—"
	}
	d := a.ExpiresAt.Sub(c.opts.Clock.Now())
	if d <= 0 {
		return "expired"
	}
	if d < time.Minute {
		return "in under a minute"
	}
	return fmt.Sprintf("in %d min", int(d.Round(time.Minute)/time.Minute))
}

func (c *Console) approvalsPage(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	st := r.URL.Query().Get("status")
	if st == "" {
		st = "pending"
	}
	v := view{Title: "Approvals", Nav: "approvals", RetryURL: "/console/approvals"}
	data := approvalsView{Status: st, Statuses: approvalStatuses}
	if !slices.Contains(approvalStatuses, st) {
		v.Error = "Unknown status filter."
		v.Data = data
		c.render(w, r, http.StatusBadRequest, "approvals", v)
		return
	}
	filter := st
	if st == "all" {
		filter = ""
	}
	list, err := c.deps.Approvals.List(r.Context(), s.principal, filter, 200)
	if err != nil {
		c.opts.Logger.Error("console approvals list", "error", err)
		v.Error = "The approval store is unavailable."
		v.Data = data
		c.render(w, r, http.StatusOK, "approvals", v)
		return
	}
	// Soonest expiry first: that is the order an approver must work in.
	slices.SortStableFunc(list, func(a, b approvals.Approval) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	for _, a := range list {
		// The store still reports a pending approval past its expiry as pending until the sweep;
		// the service marks it expired, so it leaves the pending queue here.
		if st != "all" && a.Status != st {
			continue
		}
		data.Approvals = append(data.Approvals, approvalRow{Approval: a, ExpiresIn: c.expiresIn(a)})
	}
	v.Data = data
	c.render(w, r, http.StatusOK, "approvals", v)
}

func (c *Console) approvalPage(w http.ResponseWriter, r *http.Request) {
	v := view{Nav: "approvals"}
	switch r.URL.Query().Get("done") {
	case "approved":
		v.Flash = "Approved. The agent's retry with the same arguments and idempotency key now executes once."
	case "rejected":
		v.Flash = "Rejected. The agent's retry receives approval_rejected."
	}
	c.showApproval(w, r, http.StatusOK, v, approvalView{})
}

// showApproval renders the detail page for the {id} in the path, or a not-found page.
func (c *Console) showApproval(w http.ResponseWriter, r *http.Request, status int, v view, typed approvalView) {
	s := current(r)
	id := chi.URLParam(r, "id")
	v.Title = "Approval"
	a, err := c.deps.Approvals.Get(r.Context(), id, s.principal)
	if errors.Is(err, approvals.ErrNotFound) {
		v.Error, v.ErrorTitle, v.RetryURL, v.RetryLabel, v.Flash = "It does not exist or belongs to another tenant.", "No such approval.",
			"/console/approvals", "Back to approvals", ""
		c.render(w, r, http.StatusNotFound, "approval", v)
		return
	}
	if err != nil {
		c.opts.Logger.Error("console approval get", "error", err)
		v.Error, v.RetryURL, v.Flash = "The approval store is unavailable.", "/console/approvals/"+url.PathEscape(id), ""
		c.render(w, r, http.StatusOK, "approval", v)
		return
	}
	v.Title = "Approval " + a.Server + "." + a.Tool
	typed.Approval, typed.ExpiresIn, typed.CanDecide = a, c.expiresIn(a), canDecide(s.principal, a)
	v.Data = typed
	c.render(w, r, status, "approval", v)
}

// decide handles the approve and reject forms (CSRF is checked by requireSession).
func (c *Console) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := current(r)
		id := chi.URLParam(r, "id")
		field := "note"
		if !approve {
			field = "reason"
		}
		note := r.PostForm.Get(field)
		typed := approvalView{ApproveNote: note}
		if !approve {
			typed = approvalView{RejectReason: note}
		}
		cur, err := c.deps.Approvals.Get(r.Context(), id, s.principal)
		if errors.Is(err, approvals.ErrNotFound) {
			c.showApproval(w, r, http.StatusNotFound, view{Nav: "approvals"}, approvalView{})
			return
		}
		// Permission first (as the admin API), then the input.
		if err == nil && !mayDecide(s.principal, cur) {
			c.showApproval(w, r, http.StatusForbidden, view{Nav: "approvals", Error: decisionMessage(approvals.ErrForbidden)}, approvalView{})
			return
		}
		if len(note) > 2000 {
			c.showApproval(w, r, http.StatusBadRequest, view{Nav: "approvals", Error: "The note or reason must be at most 2,000 characters."}, typed)
			return
		}
		a, err := c.deps.Approvals.Decide(r.Context(), id, s.principal, approve, note)
		if err != nil {
			code, _ := admin.DecisionStatus(err)
			c.showApproval(w, r, code, view{Nav: "approvals", Error: decisionMessage(err)}, typed)
			return
		}
		done := "approved"
		if !approve {
			done = "rejected"
		}
		// The target is built from the stored approval's uuid, never from request text.
		if !approvals.ValidID(a.ID) {
			http.Error(w, "invalid approval id", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/console/approvals/"+a.ID+"?done="+done, http.StatusSeeOther)
	}
}

func decisionMessage(err error) string {
	switch {
	case errors.Is(err, approvals.ErrForbidden):
		return "Only an approver of this approval's tenant can decide it."
	case errors.Is(err, approvals.ErrNotPending):
		return "This approval has already been decided."
	case errors.Is(err, approvals.ErrExpired):
		return "This approval has expired; the agent must call again to request a new one."
	case errors.Is(err, approvals.ErrReasonRequired):
		return "A rejection needs a reason."
	default:
		return "The decision could not be recorded: the approval store or the audit log is unavailable."
	}
}
