---
name: tdd
description: Test-first loop for one mcp-gateway change. Use at the start of every feature or fix.
---

# TDD loop

1. State the behaviour the change adds or fixes and name the test that proves it.
2. Write the named test first. Run it: `go test -race -run '^TestName$' ./path/...` (or `uv run pytest -k name`). Confirm it fails because the behaviour is missing.
3. Implement the smallest change that makes it pass. Keep `internal/clock` injection and `%w` error wrapping.
4. Run the area checks: `make check-fast` (always), plus `make policy-test`, `make hooks-test`, `make integration` when touched.
5. Note the commands you ran and their one-line results for the pull request.
6. Commit with a Conventional Commit message (`feat(pipeline): ...`, `test: ...`, `docs: ...`). No attribution trailers.
