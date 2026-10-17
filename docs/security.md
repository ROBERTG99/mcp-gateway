# Security and threat model

mcp-gateway sits between LLM agents and upstream MCP servers. It calls no model; its job is to decide which tool calls may run, run each write at most once, keep personal data out of logs and audit records, and leave a tamper-evident record of every call. This document covers the threats (STRIDE per component), the OWASP Top 10 for LLM applications mapped to controls, the controls themselves with the tests that hold them in place, and the accepted findings of `make security`.

## Trust boundaries

| Boundary | Inside | Outside | Crossing |
|---|---|---|---|
| Agent to gateway | gateway (8210) | agents, their prompts and the content they read | JWT (RS256) per request, MCP JSON-RPC body (≤ 1 MiB) |
| Gateway to upstream | gateway | upstream MCP servers and their data | tenant and agent in `_meta.mcpgw`, idempotency key, W3C trace context |
| Upstream output to agent | gateway | upstream results (possibly attacker-written text) | untrusted marking, injection score, size limit (4 MiB) |
| Operators to gateway | admin API and console (8200) | people with tokens or console sessions | JWT scopes, console session cookie and CSRF token |
| Gateway to stores | gateway | Postgres, Redis or DynamoDB | least-privilege database role, append-only audit table |

## STRIDE per component

| Component | Spoofing | Tampering | Repudiation | Information disclosure | Denial of service | Elevation of privilege |
|---|---|---|---|---|---|---|
| MCP endpoint (`internal/proxy`) | RS256 JWT with `kid`, issuer and audience checked; sessions bound to the principal (`TestSessionBoundToPrincipal`) | arguments parsed as I-JSON, schema-validated, depth ≤ 32 (`TestLimits`) | every outcome audited, `-32048` when the audit buffer is full (`TestEveryOutcomeAuditedOnce`) | tool list filtered by policy per principal (`TestToolsListFilteredByPolicy`, `TestVisibilityFollowsPolicyReload`) | body ≤ 1 MiB (`TestBodyLimit`), per-tenant and per-agent token buckets, daily budgets (`TestRateLimitStage`) | tenant taken from the token only, never from the body or `_meta` |
| Authentication (`internal/authn`) | JWKS fetched from the configured issuer; unknown `kid` refreshes once; HS256 and `none` rejected | — | — | tokens never logged (`TestNoSecretsInLogs`) | JWKS refresh is single-flight and never blocks verification (`TestJWKSHungIssuerDoesNotBlock`) | scopes come from the token; no implicit scopes |
| Policy (`internal/policy`) | — | bundle compiled at load; a broken bundle keeps the previous one (`mcpgw_policy_reload_failures_total`) | decisions recorded in the audit event | — | prepared queries; p95 0.19 ms (`evals/REPORT.md`, latency.v1) | default deny; deny beats hold; an approval lifts only a hold (`policies/*_test.rego`) |
| Idempotency (`internal/idempotency`) | keys scoped by tenant | the stored hash binds key, tool and canonical arguments (`TestIdempotencyKeyBoundToTool`) | replays flagged in `_meta.mcpgw.idempotent_replay` and audited as `replayed` | stored results ≤ 256 KiB | store down means writes are refused (`TestIdempotencyStoreDownFailsClosed`) | — |
| Approvals (`internal/approvals`) | approvers need `mcpgw:approve` and the approval's tenant | approval bound to tenant, agent, tool, args hash and key; single use (`TestApprovalBoundToArgsHash`) | decisions audited as `approval.approve` / `approval.reject` with the approver as agent | webhook payload redacted and HMAC-signed (`TestApprovalWebhookContract`) | one pending approval per call (unique index), outbox claimed with a lease | the security reviewer sees no buttons and gets 403 (`TestConsoleApprovalsRBAC`, `e2e/tests/approvals.spec.ts`) |
| Audit log (`internal/audit`) | — | SHA-256 hash chain; append-only trigger; `audit verify` finds the first bad seq (`TestAuditVerifyDetectsTamper`, `TestAuditVerifyDetectsValueTampers`) | one event per outcome, including denials and rejections | arguments redacted before storage | bounded buffer; unstorable events never stall the writer (`TestAuditWriterIsolatesUnstorableEvent`) | gateway role has INSERT and SELECT only |
| Admin API and console (`internal/admin`, `internal/console`) | bearer tokens (API); server-side sessions from dev personas (console, development only) | CSRF token on every POST; `SameSite=Strict`, `HttpOnly` cookies (`TestCSRFRejected`) | approvals and policy reloads audited or logged | tenant scoping of audit and approvals (`TestAdminTenantScoping`, `TestConsoleRequiresScope`); chain verify only for cross-tenant viewers | Schemathesis contract run without server errors (`scripts/schemathesis.sh`) | scopes per route mirror the API (`TestConsoleRequiresScope`); strict CSP, no inline code (`TestTemplatesHaveNoInlineCode`) |
| Upstreams (`pkg/mcpserver`, examples) | tenant required in `_meta.mcpgw` | upstream-side dedup on `(tenant, idempotency_key)` (`TestWriteToolDedup`) | — | tenant isolation in every query (`TestUpstreamTenantIsolation`) | results ≤ 4 MiB at the gateway | upstream role limited to its schemas |
| Telemetry | — | — | step record per call | logs redacted; no arguments in spans or step records | — | — |

## OWASP Top 10 for LLM applications (2025) mapped to controls

The gateway is the enforcement point for the tools an LLM agent may use, so most items map to controls on tool calls rather than on the model.

| Risk | Control in the gateway | Evidence |
|---|---|---|
| LLM01 Prompt injection | upstream output marked untrusted (`structuredContent.untrusted`, text prefixed), injection score with signals, optional `block` mode; writes need scopes, policy and, above limits, a human approval bound to the exact arguments | `TestUntrustedMarking`, `TestInjectionFlaggedAndBlocked`; injection.v1 test recall 0.8000, FPR 0 (`evals/REPORT.md`; see the caveats there) |
| LLM02 Sensitive information disclosure | redaction of cards, IBANs, national ids, emails, phones and tokens before audit, webhooks and logs; tenant isolation | `TestNoSecretsInLogs`, `TestProductionLoggerRedacts`; redaction.v1 recall and precision 1.0 on synthetic formats |
| LLM03 Supply chain | pinned dependencies, `govulncheck`, `trivy`, `go-licenses`, `pnpm audit`, `gitleaks` | `make security` (below) |
| LLM04 Data and model poisoning | not applicable: the gateway trains and calls no model; poisoned upstream content is handled as LLM01 | — |
| LLM05 Improper output handling | upstream results are size-limited, marked untrusted and escaped in the console | `TestLimits`, console XSS probes in the M4 review |
| LLM06 Excessive agency | per-tool scopes, default-deny policy, refund and credit-note holds, single-use approvals, mandatory idempotency keys on writes | `policies/*_test.rego`, `TestApprovalLifecycle`, `TestIdempotencyKeyRequired` |
| LLM07 System prompt leakage | not applicable: the gateway holds no prompts; override and "reveal your instructions" phrases raise the injection score | `TestInjectionSignals` |
| LLM08 Vector and embedding weaknesses | not applicable: no retrieval or embeddings | — |
| LLM09 Misinformation | out of scope for a tool gateway; the audit log records what tools returned | — |
| LLM10 Unbounded consumption | body, result, stored-result and depth limits; rate limits and daily budgets per tenant and agent; upstream timeout and circuit breaker | `TestLimits`, `TestBodyLimit`, `TestRateLimitStage`, `TestBreakerOpensAndRecovers` |

## Controls

- **Authentication:** RS256 only, `kid` required, issuer and audience checked.
- **Tenant isolation:** the tenant comes from the verified token, never from the request body or `_meta`. Approvers are scoped to their own tenant, and admin and console routes check scopes.
- **NFR-9 limits:**
  - request body ≤ 1 MiB (HTTP 413);
  - upstream result ≤ 4 MiB (`-32046`, `result_too_large`);
  - stored replay result ≤ 256 KiB (larger results are forwarded on retry);
  - argument nesting ≤ 32;
  - arguments validated against the tool's input schema (`-32602`).
- **Ordering of side effects:** an upstream write runs only after the audit slot, policy, approval and idempotency stages have all passed.
- **Redaction:** applied before the audit log, approval webhooks and logs. Logs go through the redacting handler, and sensitive keys are dropped.
- **Audit log:** append-only, hash-chained and verified by `mcp-gateway audit verify`.
- **Console:**
  - CSRF token per session;
  - `SameSite=Strict`, `HttpOnly` cookies;
  - strict CSP with no inline code;
  - sign-in only with a configured dev issuer.
- **Secrets:** from the environment only (`.env.example` lists names, never values). The webhook HMAC key must be at least 16 bytes.
- **Fail closed:**
  - no policy means deny;
  - idempotency store down means writes are refused;
  - audit saturation returns `-32048`;
  - an open breaker returns `-32046`.
- **SBOM:** optional, with `syft` (not run in this build).

## `make security`

`scripts/security.sh` runs the steps below; each must pass or appear under "Accepted findings".

| Step | Result on 2026-10-17 (`./scripts/security.sh`) |
|---|---|
| gitleaks (tree and history, `.gitleaks.toml`) | ok |
| gosec (through golangci-lint, so reviewed `//nolint:gosec` justifications apply) | ok |
| govulncheck | **not run**: vuln.go.dev answers 403 through this environment's proxy; CI runs it |
| trivy filesystem (vulnerabilities, secrets, misconfigurations; high and critical) | ok, after upgrading `github.com/moby/go-archive` to v0.3.0 (CVE-2026-17106, a testcontainers dependency) |
| trivy image `mcp-gateway:dev` (high and critical, fixed only) | ok |
| hadolint | ok |
| actionlint | skipped: no workflows yet (T-43) |
| pnpm audit (`e2e/`, high and critical) | ok with one accepted advisory |
| go-licenses (Apache-2.0, MIT, BSD, ISC, MPL-2.0) | ok with one manual check |

### Accepted findings

| Finding | Why it is accepted | Where |
|---|---|---|
| gitleaks `jwt` in `evals/datasets/*/redaction.v1.jsonl` and `internal/redact/*_test.go` | synthetic tokens the redaction eval and tests must detect; they sign nothing | `.gitleaks.toml` |
| GHSA-vfj7-8cjw-p6xm (`braces`, high) via `markdownlint-cli2` | dev-only lint tool in `e2e/`, runs on repository Markdown, no patched version published | `e2e/package.json` `pnpm.auditConfig.ignoreGhsas` |
| `github.com/segmentio/asm` license not classified | its `LICENSE` is the MIT license (checked by hand); dependency of the MCP SDK's JSON package | `scripts/security.sh` |
| `.claude/settings.json` not installed | the guard hooks are not active in this environment (PROGRESS.md, "Needs the maintainer") | — |

See also `docs/KNOWN_LIMITATIONS.md` (development-only console sign-in, injection scoring is defence in depth).
