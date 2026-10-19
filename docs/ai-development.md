# AI-assisted development

This repository was built by an AI coding agent, Claude Code, working from a written specification. A human maintainer set the task, owns the repository and decides what ships. This document says how the work was organised, what the review steps caught, which rules kept the record honest, and what a human must still check.

The gateway itself calls no model. The AI was part of the build, not part of the product.

## How the build was run

The specification is `docs/specs/000-build-spec.md` (it started as `BUILD.md` at the repository root). The agent was told to read it and execute it end to end, and not to stop until every item in the Global Definition of Done was checked. The operating rules are in section 0 of the spec. The main ones:

- Decide, don't ask. An ambiguous choice is made, then recorded in `docs/DECISIONS.md`. An architectural choice also gets an ADR in `docs/adr/`.
- Progress lives in the repository. `PROGRESS.md` lists every task with its status and evidence, and has a "Needs the maintainer" section at the top.
- Work in small verified steps: a failing test first, then the code, then the checks, then a commit.
- Honesty over appearance (see "Honesty rules" below).
- Safety rails: no `terraform apply`, no cloud deploys, no creating or publishing repositories, no messages to real people, no secrets or real personal data, no force-pushes, no pushed tags.

The build environment was Claude Code on the web. The versions and limits that applied are recorded in `docs/DEVELOPMENT.md`.

## The harness

The harness is everything in the repository that steers and checks the agent. It was built first (milestone M0) and used for the rest of the work.

| Part | Where | What it does |
|---|---|---|
| Instructions | `CLAUDE.md`, `AGENTS.md` | `CLAUDE.md` has the map of the code, the real `make` targets, the Go conventions, the test-first rule, the attribution rule and the never-do list. `AGENTS.md` has the same essentials for other agents. |
| Rules | `.claude/rules/` | Short rule files. `git-commits.md` always loads. The rest load by path: `go.md`, `rego.md`, `helm.md`, `python.md`, `frontend.md`, `testing.md`, `evals.md`, `docker.md`, `security.md`, `docs.md` and `llm-code.md`. |
| Hooks | `.claude/hooks/` | Four Python scripts using the standard library only (see below). |
| Subagents | `.claude/agents/` | Seven roles (see below). |
| Skills | `.claude/skills/` | Thirteen short procedures: `tdd`, `quality-gate`, `eval-report`, `new-adr`, `add-migration`, `add-endpoint`, `add-agent-tool`, `add-eval-case`, `demo-capture`, `release`, `add-policy`, `add-upstream-server` and `audit-forensics`. |
| MCP servers | `.mcp.json` | Three development servers: this gateway on `http://localhost:8210/mcp` (token from `MCPGW_DEV_TOKEN`), `@playwright/mcp` for console inspection, and a read-only Postgres server for audit inspection. No test depends on them. |
| Settings | `docs/harness/settings.json` | The permission lists, the attribution setting and the hook registration. Not installed yet (see below). |

The files that must exist are listed in `tests/harness/test_harness_files.py`, which checks them.

### Hooks and their tests

- `session_start.py` (SessionStart): on Claude Code on the web only, sets up the Docker registry mirror, starts the Docker daemon and installs the pinned tools. It is idempotent.
- `pre_tool_guard.py` (PreToolUse): blocks the never-do actions: `terraform apply` and `destroy`, force-pushes and tag pushes, git identity changes, AI attribution lines in commits and pull requests, repository creation or visibility changes, messages to real recipients, reads of `.env` files other than `.env.example`, and edits to existing files in `evals/datasets/test/`. It fails open if its own input is malformed, so a hook bug cannot wedge a session.
- `post_edit_format.py` (PostToolUse): runs gofmt and goimports, prettier under `e2e/`, and `mcp-gateway policy fmt` for Rego. It does nothing when a formatter is missing.
- `stop_check.py` (Stop): runs `make check-fast` before the agent stops, and blocks once with a summary if it fails.

The hooks are tested without Claude Code, by `tests/harness/test_hooks.py`, run by `make hooks-test`. At task T-04 that run reported 46 passed, with `ruff` and `mypy --strict` clean (`PROGRESS.md`, T-04 row). `make hooks-test` now runs every Python test in the repository.

### Subagents

The seven roles are defined in `.claude/agents/`, each with a fixed tool list:

- `reviewer`: read-only, `model: opus`. It returns "ship" or "fix first" with blockers listed first.
- `security-auditor`: read-only, `model: opus`. It reports findings by severity.
- `policy-reviewer`: read-only, `model: opus`. It checks least privilege and default deny in Rego.
- `test-designer`: writes only test files, never production code.
- `eval-analyst`: analyses eval results, works from the dev partition and never reads or edits the test partition.
- `ux-reviewer`: read-only review of the console for usability and WCAG 2.2 AA.
- `docs-editor`: edits documents and checks that numbers cite their source.

### What is not active yet

`.claude/settings.json` is not installed. The agent could not write it: Claude Code's auto-mode classifier denied the write as self-modification, through both Bash and the Write tool. The intended content is in `docs/harness/settings.json`. Until the maintainer copies it into place (`PROGRESS.md`, "Needs the maintainer"):

- the hooks in `.claude/hooks/` are not registered, so the guard, the formatter and the stop check did not run during the build;
- the permission allow and deny lists are not in force;
- the attribution setting that blanks Claude's commit trailers is not set, so the rule in `.claude/rules/git-commits.md` and manual checks carry that duty.

The hooks are tested and ready. They just were not wired in. The session-start setup is one of them, so the Docker steps in `PROGRESS.md` ("Start of a new session") are the manual fallback.

## The per-task loop

Each task in `PROGRESS.md` follows `.claude/skills/tdd/SKILL.md`:

1. Read the task and its definition in the spec.
2. Write the named test first and watch it fail for the right reason.
3. Write the smallest change that passes. Keep the injectable clock and `%w` error wrapping.
4. Run the checks for the area: `make check-fast` always, plus `make policy-test`, `make hooks-test` or `make integration` when touched.
5. Record the evidence in `PROGRESS.md`: status and the command with a one-line result.
6. Commit with a Conventional Commit message. The commit goes out under the maintainer's name only, with no attribution trailers.

The branch is not left red between tasks. The recorded evidence is the command and what it printed, so a reader can run it again.

## The per-milestone review

At the end of each milestone, the `reviewer` subagent reviewed the milestone's diff. It checks, in order: the contracts, fail-closed behaviour, whether the named tests really assert the behaviour, security, honesty of the documents and attribution. A "fix first" verdict meant the blockers were fixed and the review ran again. The milestone moved on only on "ship". For M4 a separate UX review also ran.

The verdicts are in the "Milestone reviews" table of `PROGRESS.md`:

| Milestone | Scope | Verdict |
|---|---|---|
| M0 | Bootstrap and harness | ship, second pass |
| M1 | Domain and data | ship, second pass |
| M2 | Governance core | ship, third pass |
| M3 | API and services | ship, third pass |
| M4 | Console | ship, second pass (UX re-review blockers fixed after it) |
| M5 | Evals | ship, second pass |

At the time of writing, `PROGRESS.md` has no verdict yet for M6 onward: its handoff note says M6 awaits its review. Read `PROGRESS.md` for the current state.

## What the reviews caught

These are items from the "Milestone reviews" table in `PROGRESS.md`. Each was fixed with a regression test, so the fix is checked every time the suite runs.

- **M0.** Two blockers in the guard hook: a bypass that read `.env` files, and a bypass for force-push and identity changes. Both were closed with 18 regression cases. The review also found races in the proxy and the catalog and a session binding that failed open. `TestSessionBoundToPrincipal` was added.
- **M1, the refund race.** Concurrent refunds could exceed the order amount. The code took a snapshot before it locked the order row, so two refunds each saw room. The fix locks the row first and sums existing refunds in a second statement. `TestInsertRefundSerialisesOnOrderLock` fails on the old SQL and passes now (`docs/DECISIONS.md`, 2026-10-16 refund row).
- **M2.** A tool call with `arguments: null` skipped the argument rules (`TestNullArgumentsDoNotBypassArgumentRules`). Refreshing the token issuer's keys blocked every verification behind a lock (`TestJWKSHungIssuerDoesNotBlock`). A card number followed by digits escaped redaction. A policy reload failure was not documented (now `docs/adr/0002-embedded-opa-vs-sidecar.md` and a stale readiness state).
- **M3, the audit stall.** One hostile argument (an integer beyond 2^53, or a NUL character) stalled the audit writer forever. The buffer then filled and every later call failed with `-32048`. Arguments are now made storable on write, and a batch that cannot be stored is split until the bad event is alone and gets a placeholder (`TestAuditArgsAlwaysStorable`, `TestAuditWriterIsolatesUnstorableEvent`, `TestHostileArgsDoNotStopAudit`). The second pass found that cleaning values when reading them back could hide tampering, so reads are now lossless (`TestAuditVerifyDetectsValueTampers`). Served timestamps could not be re-hashed, and `approval_consumed` was never returned. Both were fixed.
- **M4.** Console pages checked the session but not the admin API scopes, so a persona without audit scopes could read the audit log (`TestConsoleRequiresScope`). Upstream content lacked the "untrusted output" chip (`TestUpstreamsPage`). The UX review raised 10 blockers (for example hover and placeholder contrast, a theme toggle state, and error pages without a heading), fixed in commit `752aa56`. Two mobile blockers were fixed after.
- **M5.** A compiled load generator binary was committed by accident. It was untracked and ignored, but it is still in history (see below). The eval gate accepted regressions whose confidence interval included zero, so it now fails any drop of more than 1.0 percentage point. The injection dev and test sets share templates, which is now disclosed in `evals/REPORT.md` and `docs/KNOWN_LIMITATIONS.md`. Three over-broad detector rules were narrowed.

Running the suite also found problems that no review did. For example, `make test-docker` exposed an audit writer that retried an already-committed batch until its drain gave up (`docs/DECISIONS.md`, 2026-10-17 row on the lost commit acknowledgement).

A review is not independent proof. The reviewer is itself a model. The reviews found real defects, but they cannot show that none are left. That is why a human check is listed below.

## Honesty rules

The rules are in section 0 of the spec and in `.claude/rules/docs.md`, `.claude/rules/testing.md` and `.claude/rules/evals.md`.

- **Numbers cite their source.** Every number in the README and the documents points to the command or generated report that produced it. A test checks the README (`tests/harness/test_docs.py`). Numbers are never invented. If something was not run, the document says so.
- **No weakened tests.** Never skip, disable, mark as expected-failure or loosen a test or a threshold to get green. Fix the code, or record the gap in `docs/KNOWN_LIMITATIONS.md`.
- **Evals are not tuned on the test partition.** Detectors are tuned on `evals/datasets/dev/`. Existing files in `evals/datasets/test/` cannot be edited (the guard hook blocks it and the generator refuses). `evals/REPORT.md` is generated by `make eval`, never edited by hand.
- **Gaps are written down.** Examples already in the repository:
  - `govulncheck` could not run in the build environment, and `docs/security.md` says "not run";
  - the throughput target NFR-2 was not met on the 4-vCPU build machine (`docs/KNOWN_LIMITATIONS.md` and `docs/perf/README.md`);
  - injection recall on the test partition sits exactly at its gate, and `docs/KNOWN_LIMITATIONS.md` explains why it is not a held-out estimate;
  - a DynamoDB emulator test failed once under load. It was neither retried nor weakened, and the case is recorded in `docs/KNOWN_LIMITATIONS.md`;
  - no GitHub Actions run was observed from the build environment.
- **Decisions are recorded.** `docs/DECISIONS.md` lists what was decided, what else was considered and why.

Performance numbers are not repeated here. They live in `docs/perf/README.md`.

## What a human must still check

The "Needs the maintainer" section of `PROGRESS.md` is the list. At the time of writing it holds four items:

1. **Install `.claude/settings.json`.** Copy `docs/harness/settings.json` to `.claude/settings.json` and commit it. Until then the hooks and permission lists are not active.
2. **Review 40 injection cases.** The injection dataset is labelled by construction from templates in `internal/datagen/evals.go`. A person should read about 40 cases from the dev and test files and confirm the labels. The agent did not do this.
3. **Decide about a large file in history.** A 29.7 MB build artifact (the `loadgen` binary; `git cat-file -s 6af1610:loadgen` prints 29742161 bytes) was committed by accident in commit `6af1610` and untracked in the next commit. Removing it needs a history rewrite and a force-push, which the agent must not do. Decide before publishing further.
4. **Run the load test on an 8-vCPU machine.** The build machine had 4 vCPUs shared by the whole stack and the load generator. Re-run `make loadtest` on a larger machine, ideally with the load generator elsewhere, and update `docs/perf/`.

Beyond that list, these are open and recorded elsewhere:

- Watch the first real GitHub Actions run. The workflows were checked with `actionlint` and `tests/harness/test_workflows.py` only (`docs/KNOWN_LIMITATIONS.md`).
- Run `make kind-smoke` on a machine with kind. It was not run in the build environment.
- Run `make security` where `vuln.go.dev` is reachable, so `govulncheck` actually runs.
- Read the security-sensitive code yourself: `internal/authn`, `internal/audit`, `internal/approvals`, `internal/redact`, `policies/`, `migrations/` and `.claude/`. They are the paths `CODEOWNERS` lists separately.
- Push release tags yourself. The `release` skill never pushes a tag or publishes an image.

## If model code is ever added

The gateway calls no model, and no provider SDK may be imported. The `depguard` rule in `.golangci.yml` denies a fixed list of provider SDK packages, and `.claude/rules/llm-code.md` states the rule. Adding model code would mean editing that list on purpose, in a reviewed change, together with an ADR.

If that happens, Appendix B of `docs/specs/000-build-spec.md` applies in full. In short (read the spec for the exact text, because model APIs change):

- Load the `claude-api` skill before writing any code that calls Claude.
- Keep every model call in one gateway module per language (for example `internal/llm`). It owns provider selection, timeouts, retries, token and cost accounting, tracing and redaction. No other module imports a provider SDK.
- Take model IDs from configuration, never from logic. The spec's defaults are `claude-opus-5-5` and `claude-haiku-5-5`, with no date suffixes. On Amazon Bedrock the IDs carry the `anthropic.` prefix.
- Use the official SDKs. Use structured output with a schema, or tools with `strict: true`. Do not force tool use with `tool_choice` of type `any` or `tool`, and do not prefill the assistant turn.
- Use adaptive thinking with an explicit effort per route. Do not send `budget_tokens`.
- Check `stop_reason` and handle `refusal`, `max_tokens` and tool loops. Retry only on 408, 409, 429, 5xx and connection errors, with backoff and jitter.
- Stream long outputs. Put stable prompt prefixes first for caching.
- Keep prompts in versioned files (`prompts/<name>.v<N>.md`). Record prompt version, model, tokens, cost, latency and outcome for every call.
- Give every provider a deterministic fake driven by recorded fixtures. Tests and CI never call a live model, and live evals run only behind `make eval-live` within `LIVE_EVAL_BUDGET_USD`.
- Validate every model output against its schema before it is stored or acted on. Never run a model-proposed side effect without the permission checks and approvals of section 15 of the spec.

Today `make eval-live` prints an estimated cost of USD 0.00 and states that this repository calls no model (`Makefile`, `eval-live` target).
