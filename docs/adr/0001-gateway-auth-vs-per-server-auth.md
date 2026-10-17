# 0001. Authenticate at the gateway, not at each upstream server

- Status: Accepted (2026-10-17)

## Context

Agents reach several upstream MCP servers (ledger, CRM, orders). Every call needs to know who is calling, for which tenant and with which permissions. The policy, the rate limits, the approvals and the audit log all depend on that answer. If each upstream authenticated agents on its own, each would need its own token format, key handling and tenant mapping, and the gateway could not make a decision before forwarding.

## Decision

The gateway is the only place that authenticates agents.

- `internal/authn/verifier.go` verifies a bearer JWT. It accepts RS256 only, requires a `kid`, and checks issuer, audience and expiry. Lifetime is at most one hour and clock skew is 30 seconds.
- Keys come from a JWKS URL and are cached. Known keys keep working while the issuer is unreachable, until the stale limit. Keys under 2048 bits are ignored.
- The tenant and the agent come from the verified claims (`tenant`, `sub`), never from the request body or `_meta` (`internal/authn/verifier.go`, `.claude/rules/security.md`). A token without them is rejected.
- Scopes come from the `scopes` claim. The policy checks `<server>:read` or `<server>:write` (`policies/authz.rego`). Admin routes check `mcpgw:admin`, `mcpgw:approve` and `mcpgw:audit:read` (`internal/admin/admin.go`).
- The gateway tells each upstream who is calling by adding `_meta.mcpgw` with tenant, agent and request id (`internal/pipeline/pipeline.go`, `forward`). Tools built with `pkg/mcpserver` refuse a call without a tenant (`ReadTool`, `WriteTool`), and the example upstreams scope every query by it (`TestUpstreamTenantIsolation` in `internal/upstreams/upstreams_test.go`).

## Alternatives considered

- **Each upstream authenticates agents itself.** Rejected: every upstream would repeat JWT and JWKS handling, tenants and scopes could drift between servers, and the gateway could not hide tools or apply limits before forwarding. Revoking an agent would mean touching every server.
- **Pass the agent's token through to the upstream.** Rejected: the token would reach servers that do not need it, and upstream output could leak it. The gateway also logs and audits calls, so it must hold the identity anyway.
- **Mutual TLS between agent and gateway only.** Rejected: it identifies a client machine, not a tenant, agent and scope set.

## Consequences

- Upstreams trust the gateway. They must not be reachable by agents directly. The code does not enforce that separation; it is a deployment rule.
- The example upstreams do not check a token. `_meta.mcpgw` is a statement from a trusted caller, not proof.
- Production identity federation is out of scope for v0.1.0. The dev issuer (`mcp-gateway dev-issuer`) stands in for it.
- The gateway has one verification path to test and one set of keys to rotate.
