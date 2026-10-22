# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-10-17

First release. The public contracts (the MCP endpoint, JWT claims, idempotency, JSON-RPC error codes, policy input and decision, approvals and the
webhook, untrusted-output marking, the `mcpgw.audit.v1` event and the admin REST API) are the v0.1.0 API.

### Added

- Streamable HTTP MCP endpoint that aggregates upstream MCP servers as `<server>.<tool>`, with one MCP server
  per principal that lists only the tools policy allows.
- RS256 JWT authentication per tenant and agent, with a JWKS cache, single-flight refresh and a stale limit.
- Embedded OPA policy (`data.mcpgw.authz.decision`): default deny, scopes per server, tenant allowlists,
  refund and credit-note limits, approval holds, hot reload that keeps the last good bundle;
  `mcp-gateway policy test` and `policy fmt`.
- Human approvals: held calls, approval bound to tenant, agent, tool, arguments hash and key, single use,
  HMAC-signed webhook outbox, approve and reject in the admin API and the console, no self-approval.
- Mandatory idempotency for writes with Redis, DynamoDB and in-memory stores behind one interface
  (`storetest` conformance suite), plus upstream-side dedup in `pkg/mcpserver`.
- Untrusted-output marking with heuristic injection scoring, PII redaction before storage, webhooks and
  logs, and per-tenant and per-agent rate limits and daily budgets.
- SHA-256 hash-chained audit log in Postgres (one chain across replicas, append-only trigger), `mcp-gateway
  audit verify`, and an audit writer that survives a lost commit acknowledgement.
- Admin REST API (`api/openapi.yaml`) and a server-rendered console: sign-in personas, audit log and event,
  approvals, policy tester and upstreams, WCAG 2.2 AA checked with axe at 375 and 1280 px in both themes.
- Example upstreams `ledger`, `crm` and `orders` on Postgres, a synthetic data generator (seed 20260), a
  scripted demo (`make demo`) and a load and chaos generator.
- Offline evals (`policy.v1`, `injection.v1`, `redaction.v1`, `latency.v1`) with a regression gate, and
  the chaos suite (`make chaos`).
- OpenTelemetry spans per stage, Prometheus metrics, alerts with promtool tests, and a Grafana dashboard.
- Docker Compose stack with healthchecks, `make test-docker`, a distroless gateway image, a Helm chart with
  Redis and DynamoDB values, GitHub Actions workflows, and the Claude Code harness (rules, hooks, subagents,
  skills).

### Security

- Fails closed when policy is not loaded, the idempotency store or rate limiter is down (writes), or audit is
  saturated. NFR-9 limits on body size, result size, stored results and argument depth, and schema
  validation of arguments.
- Threat model, OWASP Top 10 for LLM applications mapping and scan results in `docs/security.md`
  (`make security`).

### Known limitations

- NFR-2 (2,000 calls/s on 8 vCPUs) is not met on the build machine, and NFR-7 was verified only at that
  machine's rate (`docs/KNOWN_LIMITATIONS.md`, `docs/perf/`).
- Console sign-in uses the development issuer. Eval data is synthetic, and injection scoring is defence in
  depth. Audit events still queued in memory are lost on a hard crash. The full list is in
  `docs/KNOWN_LIMITATIONS.md`.

[0.1.0]: https://github.com/ROBERTG99/mcp-gateway/releases/tag/v0.1.0
