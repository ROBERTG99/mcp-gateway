package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
)

type auditView struct {
	Filters   map[string]string
	CrossTen  bool
	Page      audit.Page
	OlderURL  string
	Statuses  []string
	Verified  *audit.VerifyResult
	CanVerify bool
	Truncated bool
}

// auditQuery turns the console form (empty fields allowed, datetime-local times) into the
// admin API's query values.
func auditQuery(form url.Values) (url.Values, map[string]string, error) {
	q, filters := url.Values{}, map[string]string{}
	for _, k := range []string{"tenant", "agent", "tool", "status", "approval_id", "cursor", "limit", "from", "to"} {
		v := form.Get(k)
		if v == "" {
			continue
		}
		filters[k] = v
		if k == "from" || k == "to" {
			t, err := parseLocal(v)
			if err != nil {
				return nil, filters, fmt.Errorf("%s must be a date and time", k)
			}
			v = t.Format(time.RFC3339)
		}
		q.Set(k, v)
	}
	return q, filters, nil
}

func parseLocal(v string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid time")
}

func (c *Console) auditPage(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	v := view{Title: "Audit log", Nav: "audit", RetryURL: "/console/audit"}
	data := auditView{CrossTen: s.principal.Tenant == "*", Statuses: []string{"ok", "replayed", "held", "denied", "rejected", "error"}}
	q, filters, err := auditQuery(r.URL.Query())
	data.Filters = filters
	if err == nil {
		var f audit.Filter
		f, err = admin.AuditFilterFromQuery(q, s.principal)
		if err == nil {
			data.Page, err = c.deps.Audit.List(r.Context(), f)
			if errors.Is(err, audit.ErrBadCursor) {
				err = errors.New("the page link is no longer valid")
			} else if err != nil {
				c.opts.Logger.Error("console audit list", "error", err)
				err = errors.New("the audit store is unavailable")
			}
		}
	}
	if err != nil {
		v.Error = err.Error() + "."
		v.Data = data
		c.render(w, r, http.StatusOK, "audit", v)
		return
	}
	if data.Page.NextCursor != "" {
		older := url.Values{}
		for k, val := range filters {
			if k != "cursor" {
				older.Set(k, val)
			}
		}
		older.Set("cursor", data.Page.NextCursor)
		data.OlderURL = "/console/audit?" + older.Encode()
	}
	// The chain spans every tenant, so only cross-tenant viewers get the button and the check.
	data.CanVerify = admin.CanVerify(s.principal)
	if r.URL.Query().Get("verify") == "1" && data.CanVerify {
		res, err := c.api.Verify(r.Context(), 0)
		switch {
		case err != nil:
			v.Error = "Verification could not run: the audit store is unavailable."
		case res.OK:
			v.Flash = fmt.Sprintf("Chain verified: %d events, head hash %s.", res.Checked, short(res.HeadHash))
		default:
			v.Error = fmt.Sprintf("Chain broken at seq %d: %s (%d events verified before it).", res.FirstBadSeq, res.Reason, res.Checked)
			v.RetryURL = "/console/audit?verify=1"
		}
		data.Verified = &res
	}
	v.Data = data
	c.render(w, r, http.StatusOK, "audit", v)
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16] + "…"
	}
	return s
}

func (c *Console) auditEventPage(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 64)
	if err != nil || seq < 1 {
		c.render(w, r, http.StatusNotFound, "audit_event", view{Title: "Audit event", Nav: "audit", Error: "No such audit event.", RetryURL: "/console/audit"})
		return
	}
	e, err := c.deps.Audit.Get(r.Context(), seq)
	if errors.Is(err, audit.ErrEventNotFound) || (err == nil && s.principal.Tenant != "*" && e.Tenant != s.principal.Tenant) {
		c.render(w, r, http.StatusNotFound, "audit_event", view{Title: "Audit event", Nav: "audit", Error: "No such audit event.", RetryURL: "/console/audit"})
		return
	}
	if err != nil {
		c.render(w, r, http.StatusOK, "audit_event", view{Title: "Audit event", Nav: "audit",
			Error: "The audit store is unavailable.", RetryURL: r.URL.Path})
		return
	}
	c.render(w, r, http.StatusOK, "audit_event", view{Title: fmt.Sprintf("Audit event %d", seq), Nav: "audit", Data: e})
}
