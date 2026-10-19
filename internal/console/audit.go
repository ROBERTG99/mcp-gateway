package console

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
)

type auditView struct {
	Filters    map[string]string
	CrossTen   bool
	Page       audit.Page
	OlderURL   string
	Statuses   []string
	Verified   *audit.VerifyResult
	BadSeq     int64 // first bad seq when verification failed (linked from the banner)
	CanVerify  bool
	VerifyURL  string // the current filters plus verify=1
	ListFailed bool   // the list query failed: no table, error banner instead
	Truncated  bool
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
				return nil, filters, fmt.Errorf("“%s” must be a date and time", map[string]string{"from": "From", "to": "To"}[k])
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
	if err == nil && slices.ContainsFunc(slices.Collect(maps.Values(filters)), func(v string) bool { return strings.ContainsRune(v, 0) }) {
		// Stored values never contain NUL (as in the admin API): nothing can match.
		data.Page = audit.Page{Events: []audit.Event{}}
		v.Data = data
		c.render(w, r, http.StatusOK, "audit", v)
		return
	}
	if err == nil {
		var f audit.Filter
		f, err = admin.AuditFilterFromQuery(q, s.principal)
		if err == nil {
			data.Page, err = c.deps.Audit.List(r.Context(), f)
			if errors.Is(err, audit.ErrBadCursor) {
				err = errors.New("the page link is no longer valid")
			} else if err != nil {
				c.opts.Logger.Error("console audit list", "error", err)
				err = errStoreUnavailable
			}
		}
	}
	if err != nil {
		v.Error, v.RetryURL, v.RetryLabel = err.Error()+".", "/console/audit", "Reset filters"
		data.ListFailed = true
		v.Data = data
		status := http.StatusBadRequest
		if errors.Is(err, errStoreUnavailable) {
			status, v.RetryURL, v.RetryLabel = http.StatusOK, r.URL.String(), ""
		}
		c.render(w, r, status, "audit", v)
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
	vq := url.Values{}
	for k, val := range filters {
		vq.Set(k, val)
	}
	vq.Set("verify", "1")
	data.VerifyURL = "/console/audit?" + vq.Encode()
	if r.URL.Query().Get("verify") == "1" && data.CanVerify {
		// A failed verification is reported above the list, which stays visible.
		res, err := c.api.Verify(r.Context(), 0)
		switch {
		case err != nil:
			v.ErrorTitle, v.Error = "Verification could not run.", "The audit store is unavailable."
			v.RetryURL, v.RetryLabel = data.VerifyURL, "Verify again"
		case res.OK:
			v.Flash = fmt.Sprintf("Chain verified: %d events, head hash %s.", res.Checked, short(res.HeadHash))
		default:
			v.ErrorTitle = "Audit chain verification failed."
			v.Error = fmt.Sprintf("The chain breaks at seq %d: %s (%d events verified before it). Treat the log as tampered and follow the runbook.",
				res.FirstBadSeq, res.Reason, res.Checked)
			v.RetryURL, v.RetryLabel = fmt.Sprintf("/console/audit/%d", res.FirstBadSeq), fmt.Sprintf("Open event %d", res.FirstBadSeq)
			data.BadSeq = res.FirstBadSeq
		}
		data.Verified = &res
	}
	v.Data = data
	c.render(w, r, http.StatusOK, "audit", v)
}

var errStoreUnavailable = errors.New("the audit store is unavailable")

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
		c.render(w, r, http.StatusNotFound, "audit_event", view{Title: "Audit event", Nav: "audit", Error: "No such audit event.", RetryURL: "/console/audit", RetryLabel: "Back to the audit log"})
		return
	}
	e, err := c.deps.Audit.Get(r.Context(), seq)
	if errors.Is(err, audit.ErrEventNotFound) || (err == nil && s.principal.Tenant != "*" && e.Tenant != s.principal.Tenant) {
		c.render(w, r, http.StatusNotFound, "audit_event", view{Title: "Audit event", Nav: "audit", Error: "No such audit event.", RetryURL: "/console/audit", RetryLabel: "Back to the audit log"})
		return
	}
	if err != nil {
		c.render(w, r, http.StatusOK, "audit_event", view{Title: "Audit event", Nav: "audit",
			Error: "The audit store is unavailable.", RetryURL: r.URL.Path})
		return
	}
	c.render(w, r, http.StatusOK, "audit_event", view{Title: fmt.Sprintf("Audit event %d", seq), Nav: "audit", Data: e})
}
