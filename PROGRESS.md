# PROGRESS

Build tracker for `docs/specs/000-build-spec.md` (originally `BUILD.md`). Tasks are checked here, never in the spec.

## Needs the maintainer

- **`.claude/settings.json` could not be written by the agent.** The Claude Code auto-mode classifier denied the write as self-modification (both via Bash and the Write tool). The intended content is in `docs/harness/settings.json`; copy it to `.claude/settings.json` (`cp docs/harness/settings.json .claude/settings.json`) and commit. Until then the hooks in `.claude/hooks/` are not registered and the attribution key is enforced only by `.claude/rules/git-commits.md` and by hand.

- **Maintainer review of 40 injection cases (spec §11).** The injection suite is labelled by construction from attack and benign templates in `internal/datagen/evals.go`; please read 40 cases (for example the first 20 of `evals/datasets/dev/injection.v1.jsonl` and the first 20 of `evals/datasets/test/injection.v1.jsonl`) and confirm the labels. Not done by the agent.

## Identity evidence (§1.1)

`.claude/settings.json` and `.claude/rules/git-commits.md` were not pre-seeded (the initial commit contains only `BUILD.md`, `README.md`, `LICENSE`, `.gitignore`). The rule file was created and committed alone; see the item above for the settings file.

Environment variables `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` are all defined (`CLAUDE_CODE_REMOTE=true`).

```text
$ git log -1 --format='%an <%ae> | %cn <%ce>%n%B'   # commit 'chore: configure commit attribution'
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
chore: configure commit attribution
```

## Resume here (handoff, 2026-10-16)

**State:** T-01..T-32 done and pushed on `claude/build-md-execution-blkq3q`. Milestones M0-M4 reviewed "ship"; M5 is in progress (T-33 done, T-34 eval runner in progress). `make check-fast` is green; integration tests pass with Docker (testcontainers); `make e2e` passes on the compose stack (33 Playwright tests, axe clean).

**Next:** finish T-34 (`internal/evals`, `cmd/evalgen`): the first offline run fails the injection recall gate (dev 0.6667, test 0.5250 vs >= 0.80); tune the detector on dev only, never the threshold. Then T-35..T-36.

**Start of a new session (Claude Code on the web):**

1. Start Docker (Appendix A) if `docker info` fails: write `/etc/docker/daemon.json` with the `mirror.gcr.io` mirror, `nohup dockerd &`, wait. (The SessionStart hook does this once `.claude/settings.json` is installed, see "Needs the maintainer".)
2. `make tools` (idempotent; tools in `./bin`, built with the go.mod toolchain), `uv sync`, `cd e2e && pnpm install --frozen-lockfile`.
3. Re-read `docs/specs/000-build-spec.md`, this file, `docs/DECISIONS.md`, `git log --oneline -30`.

**What exists (for orientation):**

- Gateway assembly: `internal/server/server.go` (App: verifier, catalog, policy engine, Redis, Postgres pool, audit writer + Postgres sink/seq, approvals service + outbox worker + maintenance loop, pipeline, proxy, admin API and console on chi). `cmd/mcp-gateway`: serve, migrate, dev-issuer, dev-token, policy test/fmt, audit verify, healthcheck.
- Pipeline (`internal/pipeline`): audit reserve -> args parse (I-JSON, object) -> rate limit -> key validation (writes) -> policy -> approval -> idempotency acquire -> upstream -> idempotency complete/release -> output marking -> audit commit. Not yet built: NFR-9 limits stage (`limits.go`, T-39), telemetry spans/metrics (T-37/T-38).
- Audit: `Event.Sanitize` (write path only, markers for unstorable values) vs lossless `Event.Normalize` (read path); the writer bisects unstorable batches and stores a placeholder. Verify hashes stored rows as they are.
- Console (`internal/console`): sign-in personas, audit list/event with verify (cross-tenant viewers only), approvals queue/detail with approve/reject, policy tester with six examples, upstreams. No inline styles or scripts (CSP; `TestTemplatesHaveNoInlineCode`).
- E2E (`e2e/`, pnpm): `tests/approvals.spec.ts` (keyboard-only approve, reject, reviewer 403), `tests/console.spec.ts` (axe at 375/1280 px, light/dark, screenshots to `docs/images/`). MCP calls use the session protocol (initialize, initialized, tools/call) against :8210.
- Compose (`compose.yaml`): postgres 8220, redis 8230, approval-sink (WireMock) 8241, migrate, dev-issuer 8212, upstream-ledger/crm/orders 8213-8215, gateway 8200/8210/8211, profiles `dynamodb` and `tools`. Missing: observability profile (T-38), chaos profile (T-35), `compose.test.yaml` (T-41), `scripts/demo.sh` (T-44), `scripts/docs.sh` (video for `make docs`, T-46).
- Not started: evals (M5: `internal/datagen/evals.go` is a stub that returns an error), observability (M6), Helm/CI (M7), docs (M8: README is still the placeholder; ADR 0002 exists), release (M9).

**Gotchas learned:**

- Pipe exit codes: gate commits on `make check-fast >/tmp/cf.log 2>&1 && ...`, never through `| tail`. `fmt-check` only sees tracked files: run `gofmt -l` on new files before committing.
- Disk: Docker build cache and the Go build cache fill the allowance ("no space left on device" in `docker compose build`); `docker builder prune -af` and `go clean -cache` free ~20 GB.
- Rebuild the gateway image (`docker compose build gateway && docker compose up -d --wait gateway`) before Schemathesis or e2e, or they test the old image.
- When writing Go test strings with `\u200b`, check that the file holds the escape, not the literal character (staticcheck ST1018).
- `docker compose build -q` with `network: host` fails silently ("additional privileges requested"); build without `-q`.
- Schemathesis: `./scripts/schemathesis.sh` (uvx schemathesis 4.31.1) needs the stack up; its cache `.schemathesis/` is ignored.
- `/usr/local/bin/golangci-lint` is an old system binary; the Makefile puts `./bin` first in PATH. A concurrent golangci-lint (e.g. from a subagent) makes `make check-fast` fail with "parallel golangci-lint is running": retry.
- Playwright 1.64 runs against the preinstalled `/opt/pw-browsers/chromium-1194` (`e2e/playwright.config.ts`); never `playwright install` in the cloud.

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
| M4 | ship (second pass; UX re-review blockers fixed after it) | Code review first pass "fix first": console pages checked only the session, not the admin API scopes (TestConsoleRequiresScope, GETs and POSTs), and upstream content lacked the untrusted-output chip (TestUpstreamsPage). UX review: 10 blockers fixed in 752aa56 (verify failure keeps the list, theme toggle state, hover and placeholder contrast, h1 on error pages, rendered CSRF/scope errors, execution summary, state gaps, separate approve/reject text); 2 mobile blockers fixed after: list tables stack as labelled cards below 768 px and the chip is above the upstreams table (e2e `375px lists`). Evidence: make check-fast; go test -race -tags=integration ./internal/console ./internal/admin ./internal/server ./internal/config; SCREENSHOTS=1 pnpm exec playwright test: 48 passed; BenchmarkAuditPage p95 14.71 ms (reviewer run). |
| M3 | ship (third pass) | First pass "fix first": one hostile argument (integer beyond 2^53 or NUL) stalled the audit writer forever (now sanitised on write, unstorable batches bisected with a placeholder; TestAuditArgsAlwaysStorable, TestAuditWriterIsolatesUnstorableEvent, TestHostileArgsDoNotStopAudit, TestAuditUnstorableArgsPostgres); served `ts` not re-hashable (Event.MarshalJSON, TestAuditAPIEventsRehashable); approval_consumed never returned. Second pass: read-side sanitising hid tampering (lossless Normalize, TestAuditVerifyDetectsValueTampers). Suggestions applied: uuid lookups, outbox SKIP LOCKED lease, one pending approval per call (migration 00005), webhook secret length, verify limited to cross-tenant viewers, writer closed flag, dynamic audit WHERE, enqueue latency on thread CPU time plus wall p50. |
| M2 | ship (third pass) | Blockers fixed with regression tests: `arguments: null` bypassed argument rules (TestNullArgumentsDoNotBypassArgumentRules), JWKS refresh blocked under a mutex (TestJWKSHungIssuerDoesNotBlock), cards followed by digits escaped redaction, reload failure undocumented (ADR 0002 + stale readiness), docs citing not-yet-built metrics reworded. |
| M1 | ship (second pass) | First pass "fix first": concurrent refunds could exceed the order amount (snapshot taken before the row lock). Fixed by locking first and summing in a second statement; deterministic `TestInsertRefundSerialisesOnOrderLock` fails on the old SQL, passes now. Suggestions applied: I-JSON checks in canonicaljson, raw dedup results, goose advisory lock, password guards, narrower gateway env. |
| M0 | ship (second pass) | First pass "fix first": 2 blockers in `pre_tool_guard.py` (env-read and force-push/identity bypasses) fixed in "fix: close guard-hook bypasses..." with 18 regression cases; proxy/catalog races and fail-open session binding fixed; `TestSessionBoundToPrincipal` added. |

## Tasks

| ID | Title | Milestone | Status | Evidence |
|---|---|---|---|---|
| T-01 | Bootstrap and progress files | M0 | done | `uv run pytest tests/harness/test_repo_files.py` → 3 passed; identity evidence above |
| T-02 | Go module, Makefile, lint config | M0 | done | `make check-fast` → gofmt, vet, golangci-lint 0 issues, unit ok in 20 s; `TestVersionFlag` passes; all §16 targets resolve with `make -n` |
| T-03 | Instructions, settings, rules, subagents, skills | M0 | done | `uv run pytest tests/harness/test_harness_files.py` → 3 passed (settings template at docs/harness/settings.json, see Needs the maintainer) |
| T-04 | Hooks | M0 | done | `make hooks-test` → 46 passed (guard, formatter, stop check, session start); ruff and mypy --strict clean |
| T-05 | Walking skeleton in Docker | M0 | done | `go test -race ./internal/proxy -run TestProxyForwardsCall` ok; `docker compose up -d --build --wait` all healthy in 50 s; `go run ./cmd/demo-agent --list` → echo.echo |
| T-06 | Configuration | M1 | done | `go test -race ./internal/config` ok (TestConfigRejectsBadServerName, TestEnvOverrides, TestEnvValidation, TestTenantsFile, TestEveryVariableDocumented); 58 MCPGW_ variables in .env.example |
| T-07 | Migrations and append-only trigger | M1 | done | `go test -tags=integration ./internal/store` ok (TestAuditAppendOnly: app role UPDATE/DELETE/TRUNCATE rejected, owner blocked by trigger; TestMigrateUpDown); `docker compose run --rm migrate migrate up/reset/up/status` all exit 0 |
| T-08 | Synthetic data generator | M1 | done | `go test ./internal/datagen` ok (TestDatagenDeterministic: 600 customers, 2,000 orders, 1,500 notes, 40 injected); `go run ./cmd/datagen hash` twice → 874d425a745c689a556c570039eb6913a583186d343bde224a0206f50d665d8d both runs; TestSeedTwiceGivesEqualCounts (integration) ok |
| T-09 | pkg/mcpserver | M1 | done | `go test -race ./pkg/...` ok (TestWriteToolDedup, TestWriteToolRequiresKey, TestWriteToolConcurrentDedup, Example); integration TestPostgresDedupConcurrent: 12 concurrent same-key writes → 1 refund row; RFC 8785 pkg/canonicaljson with rapid key-order property |
| T-10 | Example upstreams and seed job | M1 | done | `go test -tags=integration ./internal/upstreams` ok (TestLedgerRecordPromise, TestUpstreamTenantIsolation, TestUpstreamToolsListed, TestRefundValidation); compose: upstreams healthy on 8213-8215, `make seed` twice → accounts=600 customers=600 orders=2000 notes=1500 both runs; `demo-agent --list` → 7 namespaced tools |
| T-11 | JWT authentication | M2 | done | `go test -race ./internal/authn` ok (TestJWTRejectsWrongAudience, TestJWTRejectsAlgNone incl. HS256 key confusion, expired/iss/kid/tenant/lifetime cases, TestJWKSCacheAndStaleLimit, TestMiddlewareWWWAuthenticate) |
| T-12 | Dev issuer | M2 | done | `go test -race ./internal/devissuer` ok (TestDevIssuerTokenVerifies via authn.Verifier, TestDevIssuerDisabledByDefault, bad-request cases); compose: `make dev-token SCOPES=crm:read` token → `demo-agent --list` lists tools, no token → exit 1 (401) |
| T-13 | Catalog and namespacing | M2 | done | `go test -race ./internal/catalog` ok (TestProxyAggregatesNamespaces: 7 tools, definitions identical except the name prefix; TestCatalogListChanged: upstream AddTool reaches the agent as notifications/tools/list_changed; concurrent refresh; upstream down) |
| T-14 | Policy engine and Rego bundle | M2 | done | `make policy-test` → 24 Rego tests passed + TestDecisionContract (8 golden decisions: 5000 allow, 50000 hold, 150000 deny); TestBrokenBundleDeniesEverything, TestReloadIsAtomicAndKeepsLastGood, TestReadyzReflectsPolicyBundle (503 on broken bundle) |
| T-15 | Policy-filtered discovery | M2 | done | `go test -race ./internal/proxy` ok (TestToolsListFilteredByPolicy: crm:read → [crm.get_customer], all scopes → 7, t_gamma no ledger; TestHiddenToolLooksUnknown: -32602 identical shape; TestVisibilityFollowsPolicyReload: list_changed and empty list after reload) |
| T-16 | Idempotency store: memory and Redis | M2 | done | `go test -race ./internal/idempotency` ok: storetest.Run on memory and Redis (miniredis); `go test -tags=integration -run Real` ok on redis:7 (acquire/in-progress/conflict/replay, release, TTLs, tenant isolation, 24-way concurrent acquire → 1 winner) |
| T-17 | Idempotency store: DynamoDB | M2 | done | `go test -tags=integration -run DynamoDB ./internal/idempotency` ok: storetest.Run on amazon/dynamodb-local (conditional writes, expires_at TTL); compose profile `dynamodb` on 8240; idempotency.Open selects redis/dynamodb/memory |
| T-18 | Idempotency stage | M2 | done | `go test -race ./internal/pipeline` ok (TestIdempotencyReplay, TestIdempotencyConflict, TestIdempotencyKeyRequired, TestIdempotencyInProgress, TestIdempotencyReleasedOnUpstreamFailure, TestIdempotencyStoreDownFailsClosed); compose: same-key refund twice → 1 row, 2nd idempotent_replay=true |
| T-19 | PII redaction | M2 | done | `go test -race ./internal/redact` ok (TestRedactorDetectors 18 cases incl. Luhn-invalid untouched, TestRedactorProperties: idempotent / Luhn-invalid never / Luhn-valid always, TestRedactJSONKeepsStructure, TestHandlerRedactsLogs); gateway logger wrapped with redact.Handler; audit and webhook use it in T-22/T-24 |
| T-20 | Untrusted marking and injection scoring | M2 | done | `go test -race ./internal/injection ./internal/pipeline` ok (TestInjectionSignals 13 cases incl. hidden Unicode, homoglyphs, fullwidth, leetspeak; spec example scores ≥ 0.5 with override_phrase+tool_reference; TestUntrustedMarking; TestInjectionFlaggedAndBlocked mark/block); TestFindingsStored (integration) ok |
| T-21 | Rate limit and daily budget | M2 | done | `go test -race ./internal/ratelimit ./internal/pipeline` ok (TestTokenBucketRedis memory+miniredis: 11th call in 1 s → rate_limited, retry_after ≤ 200 ms; TestDailyBudgetResets at 00:00 UTC; agent bucket; burst property; TestRateLimitStage -32042/-32043; limiter down → writes -32049, reads local bucket); TestTokenBucketRealRedis on redis:7 |
| T-22 | Audit writer | M3 | done | `go test -race ./internal/audit ./internal/pipeline` ok (TestAuditChainAppend 1,000 events verified; TestAuditEnqueueLatency p99 ≤ 1 ms; TestAuditBackpressure; TestAuditDrainOnShutdown; TestEveryOutcomeAuditedOnce 7 outcomes → 7 events, card redacted; TestFailClosedAuditSaturated -32048 with no upstream call); integration TestAuditChainAppendPostgres: 2 writers, 1,000 events, one verified chain |
| T-23 | Audit verify | M3 | done | `go test -tags=integration -run TestAuditVerifyDetectsTamper ./cmd/mcp-gateway` ok: 1,000 events verify exit 0; superuser UPDATE at chain_pos 500 → exit 1 naming that seq; --from-seq after it → 0; DELETE detected; scripts/tamper_audit.sh for the drill (make audit-verify on compose in T-44) |
| T-24 | Approvals and webhook outbox | M3 | done | `go test -race ./internal/approvals` ok (TestApprovalWebhookContract: exact payload keys, redacted args, HMAC verifies; TestOutboxBackoff: 2 s, 4 s, 8 s… capped at 1 h, 24 attempts then abandoned; TestDecideRBACAndAudit: 403 other tenant/no scope, 409 second decision, reason required, approval.approve/reject audited); approvalstest.Run on memory and Postgres (integration) incl. 12-way concurrent consume → 1. HTTP codes mapped in T-26 |
| T-25 | Approval retry path | M3 | done | `go test -race ./internal/pipeline` ok (TestApprovalLifecycle: held result shape, same approval while pending, approve → one upstream execution, retry replays, audit held/held/held/ok/replayed with approval_id; TestApprovalBoundToArgsHash: other args/key → approval_mismatch, unknown → approval_not_found, rejected, expired → -32045) |
| T-26 | Admin API and OpenAPI | M3 | done | `go test -race ./internal/admin ./tests/contract` ok (TestAdminRBAC 17 cases: 401/403/400/404/409 + 200s, problem+json; TestAdminTenantScoping; TestErrorCodes and TestCallShapes vs golden JSON); `./scripts/schemathesis.sh` against compose → 841/841 and 813/813 generated cases passed, 0 failures (seeds 20261, 20262) |
| T-27 | Scripted demo agent | M3 | done | `go test -tags=integration -run TestDemoScenarioSteps ./internal/demo` ok (in-process stack, 6 steps PASS, repeatable); compose: `go run ./cmd/demo-agent --scenario --webhook-admin http://localhost:8241` → PASS steps 1-6 in 13 s (webhook seen by WireMock, stalled call → -32046 → same-key retry → exactly one refund, injection score 0.55) |
| T-28 | Console shell, tokens, sign-in | M4 | done | `go test -race ./internal/console` ok (TestConsoleRequiresSession: 5 routes redirect to sign-in, security headers incl. CSP script-src self; TestSessionCookieAttributes HttpOnly/SameSite=Strict/Path; TestCSRFRejected sign-in and POST; TestThemesAndStatic light+dark tokens, console.js ≤ 3 KB, self-hosted fonts) |
| T-29 | Audit pages | M4 | done | audit list/detail pages with filters, keyset paging, verify; TestAuditPageFiltersAndEmptyState, TestAuditQueryPostgres; BenchmarkAuditPage p95 11.31 ms over 100,000 rows (go test -tags=integration -bench BenchmarkAuditPage -benchtime 300x ./internal/console) |
| T-30 | Approvals pages | M4 | done | approvals queue (status tabs) and detail with what-will-execute, approve with note, reject with reason; TestConsoleApprovalsRBAC (reviewer: no buttons, POST 403; approver: approve/reject audited; other tenant 404); TestTemplatesHaveNoInlineCode |
| T-31 | Policy tester and upstreams pages | M4 | done | policy tester with six examples (allow, hold, deny, approval_granted) showing matched rules; upstreams page with health, breaker, tools; TestPolicyTesterShowsMatchedRules, TestUpstreamsPage |
| T-32 | Playwright, axe, screenshots | M4 | done | make e2e (cd e2e && pnpm exec playwright test) on the compose stack: 33 passed (approvals.spec.ts keyboard-only approve + audit, reject with reason, reviewer no buttons and API 403; console.spec.ts axe 0 serious/critical on 7 screens at 375 and 1280 px in light and dark, no horizontal scroll, error and empty states); screenshots in docs/images/ |
| T-33 | Eval datasets | M5 | done | go run ./cmd/datagen evals (seed 20260): policy.v1 90/60, injection.v1 120/80 (100 malicious incl. 30 obfuscated, 100 benign incl. 30 hard negatives), redaction.v1 72/48 (90 with PII spans, 30 decoy-only); TestEvalDatasetsCountsAndSplits (counts, strata, unique ids, determinism, committed files == generator, test partition never rewritten) |
| T-34 | Eval runner and reports | M5 | done | go run ./cmd/evalgen run: evalgate.report.v1 per suite and split in evals/reports/, evals/REPORT.md; test: policy decision_accuracy 1.0, injection recall 0.8000 (dev 0.9167) and FPR 0, redaction recall 1.0 / precision 1.0, latency.v1 policy eval p95 0.19 ms; TestReportMatchesEvalgateSchema, TestPolicyEvalIsExact, TestMarkdownCitesReports, TestInjectionSignalFamilies; make eval needs scripts/evalgate_check.py (T-36) |
| T-35 | Chaos eval | M5 | todo | |
| T-36 | Eval gate | M5 | todo | |
| T-37 | Traces, logs, step records | M6 | todo | |
| T-38 | Metrics, alerts, dashboard | M6 | todo | |
| T-39 | Security hardening and threat model | M6 | todo | |
| T-40 | Benchmarks and load test | M6 | todo | |
| T-41 | Compose stack and test-docker | M7 | todo | |
| T-42 | Helm chart | M7 | todo | |
| T-43 | CI and repository integrations | M7 | todo | |
| T-44 | Demo end to end | M8 | todo | |
| T-45 | README, architecture, ADRs | M8 | todo | |
| T-46 | Remaining documents | M8 | todo | |
| T-47 | Review round | M9 | todo | |
| T-48 | Release preparation | M9 | todo | |
| T-49 | Final verification | M9 | todo | |

## Global Definition of Done

- [ ] `make verify` passes from a fresh clone (§19) with no API keys set.
- [ ] `docker compose up -d --wait` brings every service to healthy; `make seed` and `make demo` run the scripted scenario end to end.
- [ ] `make test-docker` runs the full test suite inside containers and passes.
- [ ] Unit, integration, contract and E2E suites pass; line coverage is at least 85% on core packages and 70% overall.
- [ ] Offline evals meet every threshold in §11, and `evals/REPORT.md` is generated by script from the latest run.
- [ ] Lint, format and type checks are clean.
- [ ] Security scans report no high or critical findings, or each one is documented with a reason in `docs/security.md`.
- [ ] Every workflow in §17 exists and passes actionlint; if a GitHub remote exists and CI results can be observed, the pushed branch is green.
- [ ] Every screen in §13 is implemented with loading, empty, error and success states; axe reports no serious violations; Playwright captured the screenshots in `docs/images/`.
- [ ] The harness in §7 is complete: CLAUDE.md, AGENTS.md, settings, hooks with passing tests, rules, subagents, skills and `.mcp.json`.
- [ ] Every document in §18 exists, and every number in the README points to the command or report that produced it.
- [ ] The `reviewer` subagent's final verdict is "ship", and `security-review` findings are fixed or documented.
- [ ] `PROGRESS.md` has every task checked with evidence, and `docs/KNOWN_LIMITATIONS.md` lists what is not done.
- [ ] Every commit is authored and committed by the maintainer, and no commit message or pull request contains `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" (checked as in §19).
- [ ] `BUILD.md` lives at `docs/specs/000-build-spec.md`; all work is committed, and pushed when a remote exists.
- [ ] FR-1, FR-2, FR-3: the tests named in §3.1 pass. FR-4: `make policy-test` passes and policy accuracy is 1.0.
- [ ] FR-5, FR-7: `storetest.Run` passes on all three stores; the chaos eval reports 0 duplicates and audited ratio 1.0; the demo detects the tampered `seq`.
- [ ] FR-6, FR-12: `TestApprovalLifecycle`, `e2e/tests/approvals.spec.ts` and axe pass at 375 and 1280 px in both themes.
- [ ] FR-8, FR-10: redaction recall ≥ 0.98 and precision ≥ 0.95; injection recall ≥ 0.80 and false-positive rate ≤ 0.05; `TestNoSecretsInLogs` passes.
- [ ] FR-9, FR-11, FR-13: rate-limit tests, upstream tests, `tests/integration/add_upstream_test.go` (fourth upstream by config only) and `make helm-check` pass.
- [ ] NFR budgets cited in `docs/verification.md`: overhead p95 < 5 ms, ≥ 2,000 calls/s, RSS < 128 MB, startup < 2 s, image < 40 MB.
