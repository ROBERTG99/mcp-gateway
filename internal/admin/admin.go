// Package admin serves the admin REST API (api/openapi.yaml) on :8200: audit search and
// verification, approvals, policy evaluation and reload, and upstream status. Requests carry a
// bearer JWT; every route checks scopes, and tenant-scoped principals only see their tenant.
// Errors are RFC 9457 problem documents.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

// Admin scopes.
const (
	ScopeAdmin     = "mcpgw:admin"
	ScopeApprove   = "mcpgw:approve"
	ScopeAuditRead = "mcpgw:audit:read"
)

// CatalogStatus reports upstream health.
type CatalogStatus interface {
	Status() []catalog.UpstreamStatus
}

// Deps are the API's collaborators.
type Deps struct {
	Verifier  *authn.Verifier
	Audit     audit.Reader
	Approvals *approvals.Service
	Policy    *policy.Engine
	Catalog   CatalogStatus
	// Flush persists buffered audit events (before verify); optional.
	Flush func(ctx context.Context) error
}

// API holds the handlers (also used by the console).
type API struct{ deps Deps }

// New returns the API.
func New(deps Deps) *API { return &API{deps: deps} }

// NewRouter returns the /admin routes behind bearer authentication.
func NewRouter(deps Deps) http.Handler {
	a := New(deps)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { return deps.Verifier.Middleware(next) })
	a.Mount(r)
	return r
}

// Mount registers the routes on r (authentication must already have run).
func (a *API) Mount(r chi.Router) {
	r.Use(strictQuery)
	r.With(require(ScopeAuditRead, ScopeAdmin)).Get("/admin/audit", a.listAudit)
	r.With(require(ScopeAuditRead, ScopeAdmin)).Get("/admin/audit/verify", a.verifyAudit)
	r.With(require(ScopeAuditRead, ScopeAdmin)).Get("/admin/audit/{seq}", a.getAudit)
	r.With(require(ScopeApprove, ScopeAuditRead, ScopeAdmin)).Get("/admin/approvals", a.listApprovals)
	r.With(require(ScopeApprove, ScopeAuditRead, ScopeAdmin)).Get("/admin/approvals/{ref}", a.getApproval)
	r.Post("/admin/approvals/{ref}", a.decide)
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Allow") == "" {
			w.Header().Set("Allow", allowedMethods(r.URL.Path))
		}
		Problem(w, http.StatusMethodNotAllowed, "method not allowed")
	})
	r.With(require(ScopeAdmin, ScopeAuditRead)).Post("/admin/policy/evaluate", a.evaluate)
	r.With(require(ScopeAdmin)).Post("/admin/policy/reload", a.reload)
	r.With(require(ScopeAdmin, ScopeAuditRead)).Get("/admin/upstreams", a.upstreams)
}

// allowedMethods lists the methods of an /admin path (RFC 9110 requires Allow on 405).
func allowedMethods(path string) string {
	switch {
	case strings.Contains(path, ":approve"), strings.Contains(path, ":reject"),
		path == "/admin/policy/evaluate", path == "/admin/policy/reload":
		return "POST"
	default:
		return "GET"
	}
}

// strictQuery rejects empty or repeated query parameters on /admin routes: every parameter is
// typed, and an empty or ambiguous value is a client error rather than "no filter".
func strictQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			for k, v := range r.URL.Query() {
				if len(v) != 1 || v[0] == "" {
					Problem(w, http.StatusBadRequest, "query parameter "+k+" must appear once with a value")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func require(anyOf ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := authn.FromContext(r.Context())
			if !ok {
				Problem(w, http.StatusUnauthorized, "authentication required")
				return
			}
			for _, s := range anyOf {
				if p.HasScope(s) {
					next.ServeHTTP(w, r)
					return
				}
			}
			Problem(w, http.StatusForbidden, "missing scope: one of "+strings.Join(anyOf, ", "))
		})
	}
}

// Problem writes an RFC 9457 problem document.
func Problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
}

// JSON writes a JSON response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func principal(r *http.Request) authn.Principal {
	p, _ := authn.FromContext(r.Context())
	return p
}

// AuditFilterFromQuery parses /admin/audit query parameters; the viewer's tenant is enforced.
func AuditFilterFromQuery(q map[string][]string, viewer authn.Principal) (audit.Filter, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	for k, maxLen := range map[string]int{"tenant": 64, "agent": 128, "tool": 128, "approval_id": 64, "cursor": 18} {
		v := get(k)
		if len(v) > maxLen || !utf8.ValidString(v) {
			return audit.Filter{}, fmt.Errorf("%s must be valid UTF-8, at most %d bytes", k, maxLen)
		}
	}
	f := audit.Filter{Tenant: get("tenant"), Agent: get("agent"), Tool: get("tool"), Status: get("status"),
		ApprovalID: get("approval_id"), Cursor: get("cursor")}
	if viewer.Tenant != "*" {
		f.Tenant = viewer.Tenant
	}
	if s := get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			return f, errors.New("limit must be an integer between 1 and 200")
		}
		f.Limit = n
	}
	for k, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if _, present := q[k]; present {
			s := get(k)
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return f, errors.New(k + " must be an RFC 3339 timestamp")
			}
			*dst = t
		}
	}
	if _, present := q["status"]; present && !validStatus(f.Status) {
		return f, errors.New("status must be one of ok, replayed, held, denied, rejected, error")
	}
	return f, nil
}

func validStatus(s string) bool {
	switch s {
	case "ok", "replayed", "held", "denied", "rejected", "error":
		return true
	}
	return false
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	f, err := AuditFilterFromQuery(r.URL.Query(), principal(r))
	if err != nil {
		Problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.ContainsRune(f.Tenant+f.Agent+f.Tool+f.Status+f.ApprovalID, 0) {
		JSON(w, http.StatusOK, audit.Page{Events: []audit.Event{}}) // stored values never contain NUL
		return
	}
	page, err := a.deps.Audit.List(r.Context(), f)
	if errors.Is(err, audit.ErrBadCursor) {
		Problem(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	if err != nil {
		Problem(w, http.StatusServiceUnavailable, "audit store unavailable")
		return
	}
	JSON(w, http.StatusOK, page)
}

func (a *API) getAudit(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 64)
	if err != nil || seq < 1 {
		Problem(w, http.StatusBadRequest, "seq must be a positive integer")
		return
	}
	e, err := a.deps.Audit.Get(r.Context(), seq)
	viewer := principal(r)
	if errors.Is(err, audit.ErrEventNotFound) || (err == nil && viewer.Tenant != "*" && e.Tenant != viewer.Tenant) {
		Problem(w, http.StatusNotFound, "audit event not found")
		return
	}
	if err != nil {
		Problem(w, http.StatusServiceUnavailable, "audit store unavailable")
		return
	}
	JSON(w, http.StatusOK, e)
}

func (a *API) verifyAudit(w http.ResponseWriter, r *http.Request) {
	if p, _ := authn.FromContext(r.Context()); !CanVerify(p) {
		Problem(w, http.StatusForbidden, "chain verification covers every tenant: it needs a cross-tenant viewer or mcpgw:admin")
		return
	}
	var from int64
	if s := r.URL.Query().Get("from_seq"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			Problem(w, http.StatusBadRequest, "from_seq must be a positive integer")
			return
		}
		from = n
	}
	res, err := a.Verify(r.Context(), from)
	if errors.Is(err, audit.ErrEventNotFound) {
		Problem(w, http.StatusNotFound, "from_seq not found")
		return
	}
	if err != nil {
		Problem(w, http.StatusServiceUnavailable, "audit store unavailable")
		return
	}
	JSON(w, http.StatusOK, res)
}

// CanVerify reports whether p may verify the audit chain, which spans every tenant (its result
// names other tenants' sequence numbers and the head hash, and each run forces a flush).
func CanVerify(p authn.Principal) bool { return p.Tenant == "*" || p.HasScope(ScopeAdmin) }

// Verify flushes buffered events and verifies the chain.
func (a *API) Verify(ctx context.Context, from int64) (audit.VerifyResult, error) {
	if a.deps.Flush != nil {
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = a.deps.Flush(fctx)
		cancel()
	}
	return a.deps.Audit.Verify(ctx, from) //nolint:wrapcheck // passthrough
}

func (a *API) listApprovals(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if r.URL.Query().Has("status") && status == "" {
		Problem(w, http.StatusBadRequest, "status must not be empty")
		return
	}
	switch status {
	case "", approvals.StatusPending, approvals.StatusApproved, approvals.StatusRejected, approvals.StatusExpired, approvals.StatusConsumed:
	default:
		Problem(w, http.StatusBadRequest, "status must be pending, approved, rejected, expired or consumed")
		return
	}
	list, err := a.deps.Approvals.List(r.Context(), principal(r), status, 200)
	if err != nil {
		Problem(w, http.StatusServiceUnavailable, "approval store unavailable")
		return
	}
	if list == nil {
		list = []approvals.Approval{}
	}
	JSON(w, http.StatusOK, map[string]any{"approvals": list})
}

func (a *API) getApproval(w http.ResponseWriter, r *http.Request) {
	ap, err := a.deps.Approvals.Get(r.Context(), chi.URLParam(r, "ref"), principal(r))
	if errors.Is(err, approvals.ErrNotFound) {
		Problem(w, http.StatusNotFound, "approval not found")
		return
	}
	if err != nil {
		Problem(w, http.StatusServiceUnavailable, "approval store unavailable")
		return
	}
	JSON(w, http.StatusOK, ap)
}

// DecisionStatus maps a decision error to its HTTP status (contract: 403, 404, 409, 410).
func DecisionStatus(err error) (int, string) {
	switch {
	case errors.Is(err, approvals.ErrForbidden):
		return http.StatusForbidden, "not allowed to decide this approval"
	case errors.Is(err, approvals.ErrNotFound):
		return http.StatusNotFound, "approval not found"
	case errors.Is(err, approvals.ErrNotPending):
		return http.StatusConflict, "approval is not pending"
	case errors.Is(err, approvals.ErrExpired):
		return http.StatusGone, "approval expired"
	case errors.Is(err, approvals.ErrReasonRequired):
		return http.StatusBadRequest, "a rejection needs a reason"
	default:
		return http.StatusServiceUnavailable, "approval store or audit unavailable"
	}
}

func (a *API) decide(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(chi.URLParam(r, "ref"), ":")
	if !ok || (action != "approve" && action != "reject") {
		Problem(w, http.StatusNotFound, "unknown approval action")
		return
	}
	p, authed := authn.FromContext(r.Context())
	if !authed {
		Problem(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !p.HasScope(ScopeApprove) {
		Problem(w, http.StatusForbidden, "missing scope: "+ScopeApprove)
		return
	}
	var body struct {
		Note   string `json:"note"`
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			Problem(w, http.StatusBadRequest, "body must be a JSON object")
			return
		}
	}
	note := body.Note
	if action == "reject" {
		note = body.Reason
	}
	if len(note) > 2000 {
		Problem(w, http.StatusBadRequest, "note or reason must be at most 2000 characters")
		return
	}
	ap, err := a.deps.Approvals.Decide(r.Context(), id, p, action == "approve", note)
	if err != nil {
		status, detail := DecisionStatus(err)
		Problem(w, status, detail)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"id": ap.ID, "status": ap.Status, "decided_by": ap.DecidedBy, "decided_at": ap.DecidedAt})
}

func (a *API) evaluate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	var in policy.Input
	if err == nil {
		in, err = ParsePolicyInput(raw)
	}
	if err != nil {
		Problem(w, http.StatusBadRequest, "body must be a policy input object: "+err.Error())
		return
	}
	d, err := a.deps.Policy.Evaluate(r.Context(), in)
	if err != nil && !errors.Is(err, policy.ErrNotLoaded) {
		Problem(w, http.StatusUnprocessableEntity, "policy evaluation failed")
		return
	}
	JSON(w, http.StatusOK, map[string]any{"decision": d, "matched_rules": d.Reasons, "bundle_hash": a.deps.Policy.BundleHash()})
}

func (a *API) reload(w http.ResponseWriter, r *http.Request) {
	if err := a.deps.Policy.Reload(r.Context()); err != nil {
		Problem(w, http.StatusUnprocessableEntity, "reload failed; the previous bundle stays active: "+err.Error())
		return
	}
	JSON(w, http.StatusOK, map[string]any{"bundle_hash": a.deps.Policy.BundleHash(), "reloaded": true})
}

func (a *API) upstreams(w http.ResponseWriter, _ *http.Request) {
	JSON(w, http.StatusOK, map[string]any{"upstreams": a.deps.Catalog.Status()})
}

// ParsePolicyInput decodes a policy tester input (the PolicyInput schema of api/openapi.yaml).
func ParsePolicyInput(raw []byte) (policy.Input, error) {
	var in policy.Input
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return in, errors.New("body must be a JSON object")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, err //nolint:wrapcheck // reported to the client as is
	}
	if err := validApprovalField(raw); err != nil {
		return in, err
	}
	if in.Scopes == nil {
		in.Scopes = []string{}
	}
	return in, nil
}

// validApprovalField enforces the PolicyInput schema for approval: null or {id: string, status: string}.
func validApprovalField(raw []byte) error {
	var probe struct {
		Approval *struct {
			ID     *string `json:"id"`
			Status *string `json:"status"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	if probe.Approval != nil && (probe.Approval.ID == nil || probe.Approval.Status == nil) {
		return errors.New("approval needs string id and status")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, k := range []string{"tenant", "agent", "server", "tool", "scopes", "annotations"} {
		if v, ok := fields[k]; ok && string(v) == "null" {
			return fmt.Errorf("%s must not be null", k)
		}
	}
	if v, ok := fields["scopes"]; ok {
		var scopes []*string
		if err := json.Unmarshal(v, &scopes); err != nil || slices.Contains(scopes, nil) {
			return errors.New("scopes must be an array of strings")
		}
	}
	return nil
}
