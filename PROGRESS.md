# PROGRESS

Build tracker for `docs/specs/000-build-spec.md` (originally `BUILD.md`). Tasks are checked here, never in the spec.

## Needs the maintainer

- **`.claude/settings.json` could not be written by the agent.** The Claude Code auto-mode classifier denied the write as self-modification (both via Bash and the Write tool). The intended content is in `docs/harness/settings.json`; copy it to `.claude/settings.json` (`cp docs/harness/settings.json .claude/settings.json`) and commit. Until then the hooks in `.claude/hooks/` are not registered and the attribution key is enforced only by `.claude/rules/git-commits.md` and by hand.

## Identity evidence (§1.1)

`.claude/settings.json` and `.claude/rules/git-commits.md` were not pre-seeded (the initial commit contains only `BUILD.md`, `README.md`, `LICENSE`, `.gitignore`). The rule file was created and committed alone; see the item above for the settings file.

Environment variables `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` are all defined (`CLAUDE_CODE_REMOTE=true`).

```text
$ git log -1 --format='%an <%ae> | %cn <%ce>%n%B'   # commit 'chore: configure commit attribution'
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
chore: configure commit attribution
```

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
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
| T-15 | Policy-filtered discovery | M2 | todo | |
| T-16 | Idempotency store: memory and Redis | M2 | todo | |
| T-17 | Idempotency store: DynamoDB | M2 | todo | |
| T-18 | Idempotency stage | M2 | todo | |
| T-19 | PII redaction | M2 | todo | |
| T-20 | Untrusted marking and injection scoring | M2 | todo | |
| T-21 | Rate limit and daily budget | M2 | todo | |
| T-22 | Audit writer | M3 | todo | |
| T-23 | Audit verify | M3 | todo | |
| T-24 | Approvals and webhook outbox | M3 | todo | |
| T-25 | Approval retry path | M3 | todo | |
| T-26 | Admin API and OpenAPI | M3 | todo | |
| T-27 | Scripted demo agent | M3 | todo | |
| T-28 | Console shell, tokens, sign-in | M4 | todo | |
| T-29 | Audit pages | M4 | todo | |
| T-30 | Approvals pages | M4 | todo | |
| T-31 | Policy tester and upstreams pages | M4 | todo | |
| T-32 | Playwright, axe, screenshots | M4 | todo | |
| T-33 | Eval datasets | M5 | todo | |
| T-34 | Eval runner and reports | M5 | todo | |
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
