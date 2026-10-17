# Argument-based limits. Rules only fire when arguments are present: for tools/list the gateway
# sends args = null, and listing must not depend on arguments.
package mcpgw.authz.limits

args_present if {
	input.args != null
	is_object(input.args)
}

refund if {
	input.server == "orders"
	input.tool == "refund_order"
	args_present
}

# Whole cents only: 0.5 is not an amount.
valid_amount if {
	is_number(input.args.amount_cents)
	input.args.amount_cents > 0
	input.args.amount_cents == round(input.args.amount_cents)
}

# Arguments that are present but not an object never reach the argument rules, so they are denied
# here (the pipeline also rejects them; this protects /admin/policy/evaluate and other callers).
deny contains "invalid_args" if {
	input.args != null
	not is_object(input.args)
}

# orders.refund_order: <= 10,000 cents allow, <= 100,000 hold for approval, above deny.
deny contains "invalid_amount" if {
	refund
	not valid_amount
}

deny contains "refund_over_limit" if {
	refund
	valid_amount
	input.args.amount_cents > 100000
}

hold contains "refund_requires_approval" if {
	refund
	valid_amount
	input.args.amount_cents > 10000
	input.args.amount_cents <= 100000
}

# ledger.issue_credit_note always requires a human.
hold contains "credit_note_requires_approval" if {
	input.server == "ledger"
	input.tool == "issue_credit_note"
	args_present
}
