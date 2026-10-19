# Contributing

Thank you for helping with mcp-gateway. This file tells you how to set up, how changes are made here, and how to add the common things: an upstream, a policy, a migration and an eval case.

Read `CLAUDE.md` for the map of the repository and `AGENTS.md` for the short version. Report security problems privately, not through issues (see `SECURITY.md`).

## Set up

You need Go, Docker, `uv`, Node with `pnpm`, and `make`. `go.mod` pins the Go toolchain, and `GOTOOLCHAIN=auto` downloads it. The environment used to build this repository is recorded in `docs/DEVELOPMENT.md`.

```bash
make bootstrap
```

`make bootstrap` installs the pinned developer tools into `./bin`, downloads Go modules, runs `uv sync --frozen`, runs `pnpm install --frozen-lockfile` in `e2e/`, and copies `.env.example` to `.env` if there is no `.env`. Every `MCPGW_` variable is documented in `.env.example`, which is generated from `internal/config/config.go`.

To run the full stack with synthetic data:

```bash
make up && make seed
make demo
```

`make help` lists every target. The main ones:

| Target | What it does |
|---|---|
| `make check-fast` | gofmt check, vet, golangci-lint and unit tests |
| `make test` | unit, integration (needs Docker), contract, policy and hook tests |
| `make lint`, `make typecheck` | all linters (Go, Rego, Python, TypeScript, Markdown, Actions), and type checks |
| `make policy-test` | Rego tests and golden decisions |
| `make eval` | offline evals with thresholds; writes `evals/REPORT.md` |
| `make verify` | the single gate; run it before a release |

## The test-first loop

Every change starts with a failing test. The steps are in `.claude/skills/tdd/SKILL.md`:

1. Read the task and its definition in `docs/specs/000-build-spec.md`.
2. Write the named test first and run it, for example `go test -race -run '^TestName$' ./path/...`. Check that it fails because the behaviour is missing, not because of a typo.
3. Write the smallest change that makes it pass.
4. Run the area checks: `make check-fast` always, plus `make policy-test`, `make hooks-test` or `make integration` when you touched those areas.
5. Record the evidence in `PROGRESS.md`: the command and a one-line result.
6. Commit.

Never skip, disable, mark as expected-failure or weaken a test or a threshold to get green. Fix the code. If something cannot be fixed, write the gap in `docs/KNOWN_LIMITATIONS.md`.

Go conventions (`.claude/rules/go.md`):

- wrap errors with `%w`;
- `context.Context` is the first parameter of anything that does I/O;
- no package-level mutable state; take time from `internal/clock`;
- tests are table-driven, pass with `-race` and do not sleep;
- never import a model provider SDK (the linter enforces it; the gateway calls no model);
- log through `internal/redact` first, and never log tokens.

## Before every commit

Run `make check-fast`. It must pass. Run it with its exit code visible: do not pipe it through `tail`, because a pipe hides a failure.

Before you open a pull request, also run `make lint` and `make typecheck`, and `make test` if you have Docker. Markdown files must pass `e2e/node_modules/.bin/markdownlint-cli2 <files>` (part of `make lint`). A pre-commit configuration is provided in `.pre-commit-config.yaml`.

## Commits

Use Conventional Commits: `type(scope): summary`. The history uses `feat`, `fix`, `test`, `perf`, `docs` and `chore`, for example `feat(pipeline): ...` or `fix(evals): ...`. Keep each commit green.

Commits and pull requests go out under the author's own name only. Do not add `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" lines to a commit message or a pull request description. Do not change `git config user.*` or the `GIT_AUTHOR_*` and `GIT_COMMITTER_*` variables. The rule is in `.claude/rules/git-commits.md`, and the Claude Code guard hook in `.claude/hooks/pre_tool_guard.py` blocks these actions for agents.

Never force-push or push tags.

## Add an upstream server

Connecting a new MCP server needs configuration and a policy entry, not Go code in the gateway. The steps are in `.claude/skills/add-upstream-server/SKILL.md`:

1. Add an entry to `config/upstreams.yaml`: `name` (matches `^[a-z][a-z0-9_]{1,31}$`) and `url` (a Streamable HTTP endpoint). Tools appear as `<name>.<tool>`.
2. Allow the server for the tenants that may use it in `policies/data/tenants.json`. Add limits to `config/tenants.yaml` if they differ.
3. Scopes `<name>:read` and `<name>:write` work without more policy. Add special limits in `policies/limits.rego` with tests.
4. The catalog picks the server up within `MCPGW_CATALOG_REFRESH`. `GET /admin/upstreams` shows its health and tools.

`tests/integration/add_upstream_test.go` shows the pattern end to end.

If you are writing the upstream itself in Go, use `pkg/mcpserver`: `ReadTool` and `WriteTool` register tools, and `WriteTool` adds upstream-side dedup on `(tenant, idempotency_key)`. The example upstreams are in `internal/upstreams/` (`crm.go`, `ledger.go`, `orders.go`) with their commands in `cmd/upstream-*`. To add a tool to one of them, follow `.claude/skills/add-agent-tool/SKILL.md`: required input fields, integer cents for amounts, a policy limit if the tool moves money, a policy eval case and an upstream test.

## Add or change a policy

The steps are in `.claude/skills/add-policy/SKILL.md`:

1. Write the test first in the matching `policies/*_test.rego`: an allow case, a hold case and a deny case, plus `args == null`, which is what a tool listing sends.
2. Implement the rule in `policies/*.rego`. Keep default deny, use stable snake_case reasons and do not call `http.send` (`.claude/rules/rego.md`).
3. Run `make policy-test`. If the contract changes, update `internal/policy/testdata/golden.json`.
4. Format with `go run ./cmd/mcp-gateway policy fmt policies` (`make fmt` does it).
5. Reload a running gateway with `POST /admin/policy/reload` and check `/readyz`.
6. Ask the `policy-reviewer` subagent for a least-privilege review if you use Claude Code.

## Add a migration

The steps are in `.claude/skills/add-migration/SKILL.md`:

1. Create `migrations/NNNNN_description.sql` with the next number. It needs `-- +goose Up` and `-- +goose Down` sections. Wrap functions and triggers in `-- +goose StatementBegin` and `-- +goose StatementEnd`.
2. Keep grants least-privilege. The gateway role `mcpgw_app` has `INSERT` and `SELECT` only on `audit_events`.
3. Add or extend a test in `internal/store` (build tag `integration`) that migrates up, checks the change and migrates down.
4. Run `make integration`, then `make migrate` against the stack.

Add a new migration instead of editing one that has been applied. Do not run `migrate down` against data you care about: the down step of migration 2 drops the audit table.

## Add an eval case

Eval datasets are generated, labelled by construction, with seed 20260. The steps are in `.claude/skills/add-eval-case/SKILL.md`:

1. Extend the generator in `internal/datagen/evals.go`. Do not edit the JSONL files by hand.
2. Regenerate with `go run ./cmd/datagen evals --out evals/datasets`.
3. Run `make eval`. Update `evals/baseline/` only deliberately and with a recorded reason (`.claude/skills/eval-report/SKILL.md`).
4. Never edit `evals/REPORT.md` by hand. It is rendered from `evals/reports/*.json`.

## Never edit `evals/datasets/test/`

The test partition is frozen. Heuristics and thresholds are tuned on `evals/datasets/dev/` only. The guard hook blocks edits to existing files in `evals/datasets/test/`, and `WriteEvalDatasets` refuses to change an existing test file (`ErrTestPartitionChanged` in `internal/datagen/evals.go`). A generator change that would alter the test files therefore fails. Do not work around it. Propose a new dataset version in your pull request and say why.

A regression of more than 1.0 percentage point against `evals/baseline/` fails the gate (`.claude/rules/evals.md`).

## Record decisions

- When something is ambiguous, choose, and add a row to `docs/DECISIONS.md` with the date, the decision, the alternatives and the reason.
- An architectural decision also gets an ADR in `docs/adr/NNNN-title.md`. Use `.claude/skills/new-adr/SKILL.md`. Every ADR names the alternative it rejected.
- Gaps and deviations go in `docs/KNOWN_LIMITATIONS.md`.
- Every number in a document cites the command or report that produced it (`.claude/rules/docs.md`). Do not invent numbers. If something was not run, say so.

## Pull requests

- Keep a pull request to one change, with its tests.
- Say which commands you ran and what they printed.
- Docs: plain English and short sentences. No marketing claims.
- `CODEOWNERS` lists the maintainer as reviewer for every path.

## License

By contributing you agree that your contribution is licensed under the MIT license in `LICENSE`.
