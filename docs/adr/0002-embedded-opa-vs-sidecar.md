# 0002. Embedded OPA instead of an OPA sidecar

- Status: Accepted (2026-10-16)

## Context

Every `tools/call` and every `tools/list` entry needs an authorization decision (allow, require
approval, deny) with reasons. The decision sits on the hot path: the overhead budget for the whole
gateway is p95 < 5 ms (NFR-1), and policy evaluation alone must stay under 1 ms p95 (`latency.v1`).
Platform engineers must be able to test policies offline (`make policy-test`) and reload them
without restarting the gateway.

## Decision

Embed OPA as a library (`github.com/open-policy-agent/opa/v1/rego`). The bundle in `policies/`
(Rego modules plus `policies/data/*.json` under `data.mcpgw`) is compiled into one prepared
query, `data.mcpgw.authz.decision`, held behind an atomic pointer (`internal/policy`).

- Default deny: no compiled bundle means every call is denied and `/readyz` returns 503.
- Hot reload: `MCPGW_POLICY_RELOAD_INTERVAL` polls the bundle's content hash; `POST
  /admin/policy/reload` forces it. A successful reload swaps the prepared query atomically and
  resynchronises every cached per-principal tool list (agents receive `tools/list_changed`).
- A failed reload keeps the last good bundle serving (see `docs/DECISIONS.md`). `/readyz` stays 200 but reports
  `policy: stale: last reload failed: <error>`, and the error is logged. The counter
  `mcpgw_policy_reload_failures_total` counts every failed load or reload, the first one included
  (`internal/telemetry/metrics.go`, `internal/policy/policy.go`). The gauge `mcpgw_policy_stale` is
  1 while the last reload failed and the previous bundle is still serving. The `PolicyLoadFailure`
  alert in `deploy/prometheus/alerts.yaml` fires on a recent failure, on the stale gauge or when
  `mcpgw_policy_loaded` is 0.
- Rego unit tests (`*_test.rego`) and golden decisions run with `mcp-gateway policy test`, with no
  OPA binary installed.

## Alternatives considered

- **OPA sidecar (REST, `localhost:8181`).** Rejected: an extra network hop per call (two per
  listing), another process to deploy, monitor and secure, and a second failure mode (sidecar down)
  that would also have to fail closed. Bundle distribution and decision logs are features we do
  not need for v0.1.0.
- **Policies in Go code.** Rejected: platform engineers would need a code change and a release to
  adjust limits; Rego keeps policy reviewable and testable on its own.
- **Deny everything when a reload fails.** Rejected: a typo in a bundle that is not active yet
  would take every replica out of service (see `docs/DECISIONS.md`).

## Consequences

- Policy evaluation is an in-process function call (measured by `latency.v1` in `make eval`).
- The gateway binary includes OPA, which adds to the image size budget (NFR-10).
- Rego rules must stay deterministic: no `http.send` or other non-deterministic built-ins
  (`.claude/rules/rego.md`).
