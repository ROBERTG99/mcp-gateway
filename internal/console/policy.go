package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

// policyExample is a ready-made policy input (synthetic tenants and agents).
type policyExample struct {
	ID, Title, Expect string
	Input             map[string]any
}

func refundInput(amount int, approval map[string]any) map[string]any {
	in := map[string]any{"tenant": "t_alpha", "agent": "agent-support", "scopes": []string{"orders:read", "orders:write"},
		"server": "orders", "tool": "refund_order", "annotations": map[string]any{"readOnlyHint": false},
		"args": map[string]any{"order_id": "O-00042", "amount_cents": amount, "reason": "damaged"}}
	if approval != nil {
		in["approval"] = approval
	}
	return in
}

var policyExamples = []policyExample{
	{ID: "refund-small", Title: "Small refund (50.00)", Expect: "allow", Input: refundInput(5000, nil)},
	{ID: "refund-large", Title: "Large refund (500.00)", Expect: "hold", Input: refundInput(50000, nil)},
	{ID: "refund-large-approved", Title: "Large refund with an approved approval", Expect: "allow",
		Input: refundInput(50000, map[string]any{"id": "00000000-0000-4000-8000-000000000042", "status": "approved"})},
	{ID: "refund-over-limit", Title: "Refund over the limit (5,000.00)", Expect: "deny", Input: refundInput(500000, nil)},
	{ID: "read-without-scope", Title: "CRM read without crm:read", Expect: "deny", Input: map[string]any{"tenant": "t_alpha",
		"agent": "agent-support", "scopes": []string{"orders:read"}, "server": "crm", "tool": "get_customer",
		"annotations": map[string]any{"readOnlyHint": true}, "args": map[string]any{"customer_id": "C-0042"}}},
	{ID: "tenant-without-upstream", Title: "Tenant without the ledger upstream", Expect: "deny", Input: map[string]any{"tenant": "t_gamma",
		"agent": "agent-collections", "scopes": []string{"ledger:read"}, "server": "ledger", "tool": "list_invoices",
		"annotations": map[string]any{"readOnlyHint": true}, "args": map[string]any{"customer_id": "C-0042"}}},
}

type policyView struct {
	Examples   []policyExample
	Selected   string
	Input      string
	Result     *policy.Decision
	Label      string // allow, hold or deny
	BundleHash string
}

func canInspect(p authn.Principal) bool {
	return p.HasScope(admin.ScopeAdmin) || p.HasScope(admin.ScopeAuditRead)
}

func indent(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func (c *Console) policyPage(w http.ResponseWriter, r *http.Request) {
	v := view{Title: "Policy tester", Nav: "policy"}
	if !canInspect(current(r).principal) {
		v.Error = "The policy tester needs mcpgw:admin or mcpgw:audit:read."
		c.render(w, r, http.StatusForbidden, "policy", v)
		return
	}
	data := policyView{Examples: policyExamples, Selected: policyExamples[0].ID, Input: indent(policyExamples[0].Input),
		BundleHash: c.deps.Policy.BundleHash()}
	if id := r.URL.Query().Get("example"); id != "" {
		i := slices.IndexFunc(policyExamples, func(e policyExample) bool { return e.ID == id })
		if i < 0 {
			v.Error, v.RetryURL, v.Data = "No such example.", "/console/policy", data
			c.render(w, r, http.StatusNotFound, "policy", v)
			return
		}
		data.Selected, data.Input = id, indent(policyExamples[i].Input)
	}
	v.Data = data
	c.render(w, r, http.StatusOK, "policy", v)
}

func (c *Console) evaluatePolicy(w http.ResponseWriter, r *http.Request) {
	v := view{Title: "Policy tester", Nav: "policy"}
	if !canInspect(current(r).principal) {
		v.Error = "The policy tester needs mcpgw:admin or mcpgw:audit:read."
		c.render(w, r, http.StatusForbidden, "policy", v)
		return
	}
	raw := r.PostForm.Get("input")
	data := policyView{Examples: policyExamples, Input: raw, BundleHash: c.deps.Policy.BundleHash()}
	v.Data = &data
	if len(raw) > 256<<10 {
		v.Error = "The input must be at most 256 KiB."
		c.render(w, r, http.StatusBadRequest, "policy", v)
		return
	}
	in, err := admin.ParsePolicyInput([]byte(raw))
	if err != nil {
		v.Error = "The input is not a valid policy input: " + err.Error() + "."
		c.render(w, r, http.StatusBadRequest, "policy", v)
		return
	}
	d, err := c.deps.Policy.Evaluate(r.Context(), in)
	switch {
	case errors.Is(err, policy.ErrNotLoaded):
		v.Error = "No policy bundle is loaded, so every call is denied (fail closed)."
	case err != nil:
		v.Error = "Policy evaluation failed; the gateway denies such calls."
	}
	data.Result, data.Label = &d, "deny"
	switch {
	case d.Allow:
		data.Label = "allow"
	case d.RequireApproval:
		data.Label = "hold"
	}
	if v.Error == "" {
		v.Flash = "Evaluated against bundle " + short(data.BundleHash) + "."
	}
	c.render(w, r, http.StatusOK, "policy", v)
}

func (c *Console) upstreamsPage(w http.ResponseWriter, r *http.Request) {
	v := view{Title: "Upstreams", Nav: "upstreams", RetryURL: "/console/upstreams"}
	if !canInspect(current(r).principal) {
		v.Error = "The upstreams page needs mcpgw:admin or mcpgw:audit:read."
		c.render(w, r, http.StatusForbidden, "upstreams", v)
		return
	}
	list := slices.Clone(c.deps.Catalog.Status())
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	v.Data = list
	c.render(w, r, http.StatusOK, "upstreams", v)
}
