# Verification

Final verification of v0.1.0 against `docs/specs/000-build-spec.md` §19. This file records what was run, on
what, and with which result. Steps that were not run say so.

## Environment

- Date: 2026-10-17 (the `make verify-clean` run finished at 22:07 UTC).
- Commit verified: `86f7e6cdda74bb9128acc28507cea236144803f8`. Later commits change only documentation
  (this file, `PROGRESS.md`), the regenerated `evals/` reports, and the spec's move to `docs/specs/`.
- Machine: Claude Code on the web, Linux 6.18, 4 vCPUs (Intel Xeon @ 2.10 GHz).
- Toolchains: Docker 29.8.2, Go 1.27.2, Python 3.13 with uv 0.11.32, Node 22 with pnpm 10.28.0 (versions in
  `docs/DEVELOPMENT.md`).
- No `.env` file in the clone before `make bootstrap`, and no model API keys (`scripts/verify_clean.sh`
  unsets `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `CLAUDE_API_KEY` and `GEMINI_API_KEY`). The gateway calls no
  model.

## §19 steps 1-2: `make verify-clean`

`make verify-clean` cloned the commit into a fresh `mktemp -d` folder, then ran `make bootstrap` and
`make verify`. It reused the pinned tools from `./bin`; `VERIFY_COLD=1` would download them again
(`docs/DECISIONS.md`). Result: **exit 0 in 646 s.** `make verify` ran the targets below in this order (its
`up … down` sequence covers §19 step 2). Per-target timings were not recorded; the total is the measured
duration of the whole run.

| Target | Result |
|---|---|
| `make lint` | gofmt, golangci-lint 0 issues, Rego fmt, ruff, eslint, prettier, markdownlint, actionlint: clean |
| `make typecheck` | go vet, tsc, mypy --strict: clean |
| `make security` | gitleaks, gosec, trivy fs, trivy image, hadolint, actionlint, pnpm audit, go-licenses: ok. govulncheck **not run**: vuln.go.dev is unreachable from this environment (it fails the step under `CI=true`; `docs/security.md`) |
| `make unit` | `go test -race -short ./...`: all packages ok |
| `make integration` | `go test -race -tags=integration` (testcontainers): all packages ok |
| `make policy-test` | 28 Rego tests passed, Go policy tests ok |
| `make hooks-test` | 107 Python tests passed |
| `make coverage` | core packages 86.6-95.5% (budget 85%), overall 84.2% of 5,740 statements (budget 70%) |
| `make helm-check` | lint and kubeconform clean for `values.yaml` and `values-dynamodb.yaml`, render tests ok |
| `make promtool` | 6 rules; alert tests pass |
| `make eval` | `evalgate_check: 5 reports, 0 failures` |
| `make bench` | overhead p95 0.508 ms (NFR-1 < 5 ms), audit page p95 13.15 ms (NFR-8 < 300 ms) |
| `make up` | every service healthy |
| `make image-size` | 17.6 MB compressed (budget < 40 MB), 57.6 MB unpacked |
| `make seed` | synthetic dataset loaded (seed 20260) |
| `make contract` | golden contract tests ok; Schemathesis: 833 generated, 833 passed, 12 skipped |
| `make demo` | PASS steps 1-7 and the restore (transcript below) |
| `make e2e` | 50 Playwright tests passed, axe included |
| `make test-docker` | in containers: 31 Go packages ok, 0 failed (`go test -race -tags=integration ./...`), 107 Python tests passed |
| `make down` | stack removed |

The four runs before this one failed. Each failure was fixed at its cause:

1. A DynamoDB test clock fixed in the past let dynamodb-local's TTL sweeper delete live locks.
2. A `.env` created from `.env.example` changed the stack's tenants file.
3. A line too long in a new test.
4. A row link whose nested screen-reader span intercepted a click.

Each is described in its commit message; the first two are also in `docs/DECISIONS.md`.

### Demo transcript (`make demo`, inside `make verify-clean`)

```text
PASS step 1: only allowed tools are listed (crm.get_customer, orders.get_order, orders.refund_order)
PASS step 2: USD 50 refund executes (O-00001 refund_id 1)
PASS step 3: USD 500 refund is held, approved and the retry executes once (approval 5c4e7f24-711d-41f1-ae6b-e861e02a0cee)
PASS step 4: orders call fails mid-flight; the same-key retry leaves one refund (one refund after 2 attempts with key demo-stall-1792274499513311837)
PASS step 5: injected CRM note is flagged as untrusted (score 0.55 signals override_phrase,tool_reference)
PASS step 6: audit chain verifies (15 events verified)
PASS step 7: tampered audit row is detected at its seq (verify failed at seq 8 as expected)
PASS restore: seq 8 restored; mcp-gateway audit verify passes again
```

## §19 step 3: reviews

- `reviewer`: the pass after this run found no code blockers, only overstated boxes in `PROGRESS.md` and
  wording in this file, which were fixed; a further pass gave the verdict recorded in the `PROGRESS.md`
  milestone table ("Final (T-49)"), with what every review caught.
- `security-auditor`: no critical or high findings.
- `security-review` skill (whole branch against `origin/main`): one medium finding, fixed. A consumed
  approval could re-authorise a held write after the idempotency TTL;
  `TestConsumedApprovalBoundedByIdempotencyTTL` covers it.
- `code-review` skill: 10 findings, fixed.
- `simplify` skill: applied, with the skipped items and their reasons in `docs/DECISIONS.md`.
- `policy-reviewer` and `ux-reviewer`: no policy blockers; the one UX blocker was fixed.

Steps 1 and 2 were repeated after the fixes; this file records that final run.

## §19 step 4: live evals

Not run. The gateway calls no model, so there is no provider key and no cost; `evals/REPORT.md` says live
evals were not run.

## §19 step 5: authorship

```text
$ git log --format='%an <%ae> | %cn <%ce>' | sort -u
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
$ git log --format=%B | grep -E 'Co-Authored-By|Claude-Session|Generated with Claude Code'
(no output; grep exit status 1; last run on d8db090, 83 commits)
```

## NFR budgets

| NFR | Budget | Result | Source |
|---|---|---|---|
| NFR-1 overhead p95 | < 5 ms | 0.398 ms (`docs/perf/bench.txt`); 0.508 ms in the verify run | `make bench` |
| NFR-2 throughput | ≥ 2,000 calls/s on 8 vCPUs (CI ≥ 500) | 630.0 calls/s on 4 vCPUs: **not met** (above the CI budget of 500; measured locally, not in CI) | `make loadtest-profile`, `docs/perf/loadtest.json` |
| NFR-5 startup | < 2 s | 22.2 ms | `go test -tags=integration -run TestStartupUnder2s -v ./internal/server`, `docs/perf/startup.txt` |
| NFR-7 RSS | < 128 MB at 2,000 calls/s | 93.9 MB at 630 calls/s: **not verified** at 2,000 calls/s | `make loadtest-profile`, `docs/perf/loadtest.json` |
| NFR-8 console | < 300 ms p95 at 100,000 rows | 11.79 ms (`docs/perf/bench.txt`); 13.15 ms in the verify run | `make bench` |
| NFR-10 image and start | < 40 MB; `make up` < 5 min | 17.6 MB compressed (57.6 MB unpacked); `make up` 119 s from an empty build cache (T-41 run, `PROGRESS.md`) | `make image-size`, `make up` |

Why NFR-2 and NFR-7 fall short, and what it would take, is in `docs/KNOWN_LIMITATIONS.md`.
`make loadtest` is not part of `make verify`.

## Not run

- `make kind-smoke`: kind is not installed in this environment.
- govulncheck: vuln.go.dev is unreachable here.
- GitHub Actions: no run was observed from this environment.
- Live evals: no model is called.
