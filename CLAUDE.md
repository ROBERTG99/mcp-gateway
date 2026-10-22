# mcp-gateway: instructions for Claude Code

Go gateway and library between LLM agents and upstream MCP servers: one Streamable HTTP endpoint,
JWT per tenant and agent, policy-filtered tools, embedded OPA per call (allow, deny, hold for
approval), mandatory idempotency on writes, PII redaction, per-tenant rate limits, untrusted-output
marking with injection scoring, and a SHA-256 hash-chained audit log. The gateway calls no model.

## Where things live

| Path | What |
|---|---|
| `cmd/mcp-gateway/` | `serve`, `dev-issuer`, `migrate`, `audit verify`, `policy test/fmt`, `healthcheck` |
| `internal/authn` | JWT (RS256) verification, JWKS cache |
| `internal/catalog` | upstream MCP clients, `<server>.<tool>` namespacing, refresh, list_changed |
| `internal/proxy` | Streamable HTTP handler, one `*mcp.Server` per principal (visible tools only) |
| `internal/pipeline` | `tools/call` stages: limits, rate limit, policy, approval, idempotency, upstream, output marking, redaction, audit |
| `internal/policy` | embedded OPA, `data.mcpgw.authz.decision`, atomic hot reload |
| `internal/{idempotency,approvals,audit,redact,injection,ratelimit,console,admin,telemetry}` | one concern each |
| `pkg/mcpserver` | public helpers for upstreams (`ReadTool`, `WriteTool`, tenant context, dedup) |
| `cmd/upstream-{ledger,crm,orders}` | example upstreams on Postgres |
| `policies/` `config/` `migrations/` `api/openapi.yaml` | Rego, YAML config, goose SQL, admin API contract |
| `evals/` `e2e/` `deploy/` `tests/` | eval datasets and reports, Playwright, Helm/Grafana/Prometheus, integration/contract/harness tests |

## Commands (real make targets)

- `make check-fast`: gofmt, vet, golangci-lint and unit tests (< 60 s). Run before every commit.
- `make test`: unit, integration (Docker), contract, policy and hook tests.
- `make policy-test`, `make hooks-test`, `make helm-check`, `make promtool`, `make eval`, `make bench`.
- `make up && make seed && make demo`: the local stack and scripted scenario.
- `make lint`, `make typecheck`, `make security`, `make verify` (the single gate), `make verify-clean`.

## Go conventions

- Errors wrapped with `%w`; `context.Context` first; no package-level mutable state; `internal/clock` for time.
- `go test -race` clean; no sleeps in tests; table-driven; one named test per behaviour.
- Never import a model provider SDK (depguard).
- Log with `slog` only after `internal/redact`; never log tokens.

## Working rules

- **Test first**: write the named failing test, implement, run `make check-fast`, commit (Conventional Commits). Never leave the branch red.
- Never skip, disable or weaken a test or threshold. Record gaps in `docs/KNOWN_LIMITATIONS.md`.
- Never edit `evals/datasets/test/`. Never invent numbers; every README/doc number cites its command or report.
- Record non-obvious choices in `docs/DECISIONS.md`; architectural ones also get an ADR in `docs/adr/`.

## Commit attribution

Do not add `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" to commits or pull
requests. Do not modify
`git config user.*` or the `GIT_AUTHOR_*` / `GIT_COMMITTER_*` variables.

## Never do

- `terraform apply` or `terraform destroy`; deploy to any cloud account.
- Create, publish or change the visibility of a GitHub repository; push tags; force-push.
- Send messages to real recipients (email, SMS, Slack, voice).
- Commit secrets or real personal data (synthetic data only, seed 20260).
- Read `.env` files other than `.env.example`.

## Project records

- Decisions: `docs/DECISIONS.md`; ADRs: `docs/adr/`; limitations: `docs/KNOWN_LIMITATIONS.md`.
- Harness: `.claude/rules/`, `.claude/agents/` (run `reviewer` before opening a pull request), `.claude/skills/`, `.claude/hooks/`.
