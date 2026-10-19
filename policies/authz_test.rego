package mcpgw.authz_test

import data.mcpgw.authz

base := {
	"tenant": "t_alpha", "agent": "agent-support", "scopes": ["crm:read", "orders:read", "orders:write", "ledger:write"],
	"server": "crm", "tool": "get_customer", "args": {"customer_id": "C-0001"},
	"annotations": {"readOnlyHint": true}, "approval": null, "now": "2026-10-16T12:00:00Z",
}

refund(amount) := object.union(base, {
	"server": "orders", "tool": "refund_order", "annotations": {"readOnlyHint": false},
	"args": {"order_id": "O-00001", "amount_cents": amount, "reason": "r"},
})

test_read_with_scope_allowed if {
	d := authz.decision with input as base
	d.allow
	not d.require_approval
	d.reasons == ["scope_granted"]
}

test_read_without_scope_denied if {
	d := authz.decision with input as object.union(base, {"scopes": ["orders:read"]})
	not d.allow
	d.reasons == ["missing_scope"]
}

test_read_scope_does_not_grant_write if {
	d := authz.decision with input as object.union(refund(5000), {"scopes": ["orders:read"]})
	not d.allow
	"missing_scope" in d.reasons
}

test_write_scope_does_not_grant_read if {
	d := authz.decision with input as object.union(base, {"scopes": ["crm:write"]})
	not d.allow
}

test_tenant_not_allowlisted_denied if {
	d := authz.decision with input as object.union(base, {"tenant": "t_unknown"})
	not d.allow
	"tenant_not_allowed" in d.reasons
}

test_tenant_without_upstream_denied if {
	d := authz.decision with input as object.union(base, {"tenant": "t_gamma", "server": "ledger", "tool": "get_balance", "scopes": ["ledger:read"]})
	not d.allow
	d.reasons == ["tenant_not_allowed"]
}

test_missing_annotations_treated_as_write if {
	# object.union merges objects recursively, so remove the annotations first.
	d := authz.decision with input as object.union(object.remove(base, ["annotations"]), {"annotations": {}, "scopes": ["crm:read"]})
	not d.allow
	d.reasons == ["missing_scope"]
}

test_empty_input_denied if {
	d := authz.decision with input as {}
	not d.allow
	not d.require_approval
	d.reasons == ["missing_scope", "tenant_not_allowed"]
}

test_refund_small_allowed if {
	d := authz.decision with input as refund(5000)
	d.allow
	not d.require_approval
}

test_refund_medium_held if {
	d := authz.decision with input as refund(50000)
	not d.allow
	d.require_approval
	d.reasons == ["refund_requires_approval"]
}

test_refund_large_denied if {
	d := authz.decision with input as refund(150000)
	not d.allow
	not d.require_approval
	d.reasons == ["refund_over_limit"]
}

test_deny_beats_hold_and_approval if {
	d := authz.decision with input as object.union(refund(150000), {"approval": {"id": "a", "status": "approved"}})
	not d.allow
	d.reasons == ["refund_over_limit"]
}

test_listing_ignores_argument_rules if {
	d := authz.decision with input as object.union(refund(150000), {"args": null})
	d.allow
	d.reasons == ["scope_granted"]
}

test_listing_credit_note_is_visible if {
	d := authz.decision with input as object.union(base, {"server": "ledger", "tool": "issue_credit_note", "annotations": {"readOnlyHint": false}, "args": null})
	d.allow
}

test_decision_has_contract_shape if {
	d := authz.decision with input as refund(50000)
	is_boolean(d.allow)
	is_boolean(d.require_approval)
	is_array(d.reasons)
	count(object.keys(d)) == 3
}
