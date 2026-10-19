# The gateway sets input.approval only after binding the approval to this exact call (tenant,
# agent, tool, args_sha256, unexpired, unconsumed); here an approved approval lifts a hold.
package mcpgw.authz.approval

approved if input.approval.status == "approved"
