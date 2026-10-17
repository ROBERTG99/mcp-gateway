package mcpgw.authz.approval_test

import data.mcpgw.authz
import data.mcpgw.authz.approval

held := {
	"tenant": "t_alpha", "agent": "agent-support", "scopes": ["orders:write"],
	"server": "orders", "tool": "refund_order", "annotations": {"readOnlyHint": false},
	"args": {"order_id": "O-00001", "amount_cents": 50000, "reason": "r"}, "approval": null, "now": "2026-10-16T12:00:00Z",
}

test_approved_turns_hold_into_allow if {
	d := authz.decision with input as object.union(held, {"approval": {"id": "a1", "status": "approved"}})
	d.allow
	not d.require_approval
	d.reasons == ["approval_granted", "scope_granted"]
}

test_pending_or_rejected_stay_held if {
	every status in ["pending", "rejected", "expired", "consumed"] {
		d := authz.decision with input as object.union(held, {"approval": {"id": "a1", "status": status}})
		d.require_approval
		not d.allow
	}
}

test_approved_flag if {
	approval.approved with input as {"approval": {"id": "a", "status": "approved"}}
	not approval.approved with input as {"approval": null}
	not approval.approved with input as {}
}

test_approval_does_not_grant_missing_scope if {
	d := authz.decision with input as object.union(held, {"scopes": ["orders:read"], "approval": {"id": "a1", "status": "approved"}})
	not d.allow
	d.reasons == ["missing_scope"]
}

test_missing_scope_cannot_create_a_hold if {
	d := authz.decision with input as object.union(held, {"scopes": ["orders:read"]})
	not d.allow
	not d.require_approval
	d.reasons == ["missing_scope"]
}

test_approved_credit_note_allowed if {
	note := {
		"tenant": "t_alpha", "agent": "agent-support", "scopes": ["ledger:write"],
		"server": "ledger", "tool": "issue_credit_note", "annotations": {"readOnlyHint": false},
		"args": {"account_id": "A-0001", "amount_cents": 500, "reason": "r"}, "approval": {"id": "a1", "status": "approved"},
	}
	d := authz.decision with input as note
	d.allow
	d.reasons == ["approval_granted", "scope_granted"]
}
