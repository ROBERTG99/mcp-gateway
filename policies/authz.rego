# Entry point of the gateway policy bundle: data.mcpgw.authz.decision.
# Default deny. Read tools need "<server>:read", writes "<server>:write"; the tenant must have the
# upstream in its allowlist (data.mcpgw.tenants). Argument limits live in limits.rego and the
# approval override in approval.rego. Deny reasons beat holds; an approved approval lifts a hold.
package mcpgw.authz

import data.mcpgw.authz.approval
import data.mcpgw.authz.limits

default decision := {"allow": false, "require_approval": false, "reasons": ["default_deny"]}

decision := {"allow": false, "require_approval": false, "reasons": sort(deny_reasons)} if {
	count(deny_reasons) > 0
} else := {"allow": false, "require_approval": true, "reasons": sort(limits.hold)} if {
	count(limits.hold) > 0
	not approval.approved
} else := {"allow": true, "require_approval": false, "reasons": sort(allow_reasons)} if {
	tenant_allowed
	scope_granted
}

deny_reasons contains "tenant_not_allowed" if not tenant_allowed

deny_reasons contains "missing_scope" if not scope_granted

deny_reasons contains reason if some reason in limits.deny

allow_reasons contains "scope_granted"

allow_reasons contains "approval_granted" if {
	count(limits.hold) > 0
	approval.approved
}

tenant_allowed if input.server in data.mcpgw.tenants[input.tenant].upstreams

read_only if input.annotations.readOnlyHint == true

required_scope := sprintf("%s:read", [input.server]) if read_only

else := sprintf("%s:write", [input.server])

scope_granted if required_scope in input.scopes
