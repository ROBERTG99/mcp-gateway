package mcpgw.authz.limits_test

import data.mcpgw.authz.limits

refund(amount) := {"server": "orders", "tool": "refund_order", "args": {"order_id": "O-1", "amount_cents": amount, "reason": "r"}}

test_refund_boundaries if {
	count(limits.hold) == 0 with input as refund(10000)
	count(limits.deny) == 0 with input as refund(10000)
	limits.hold == {"refund_requires_approval"} with input as refund(10001)
	limits.hold == {"refund_requires_approval"} with input as refund(100000)
	limits.deny == {"refund_over_limit"} with input as refund(100001)
	count(limits.hold) == 0 with input as refund(100001)
}

test_refund_invalid_amount_denied if {
	limits.deny == {"invalid_amount"} with input as refund("lots")
	limits.deny == {"invalid_amount"} with input as refund(0)
	limits.deny == {"invalid_amount"} with input as refund(-5)
	limits.deny == {"invalid_amount"} with input as {"server": "orders", "tool": "refund_order", "args": {"order_id": "O-1"}}
}

test_credit_note_always_held if {
	limits.hold == {"credit_note_requires_approval"} with input as {"server": "ledger", "tool": "issue_credit_note", "args": {"customer_id": "C-1", "amount_cents": 1, "reason": "x"}}
}

test_no_argument_rules_when_listing if {
	count(limits.hold) == 0 with input as {"server": "ledger", "tool": "issue_credit_note", "args": null}
	count(limits.deny) == 0 with input as {"server": "orders", "tool": "refund_order", "args": null}
}

test_other_tools_unaffected if {
	count(limits.hold) == 0 with input as {"server": "crm", "tool": "add_note", "args": {"customer_id": "C-1", "text": "x"}}
	count(limits.deny) == 0 with input as {"server": "crm", "tool": "add_note", "args": {"customer_id": "C-1", "text": "x"}}
}
