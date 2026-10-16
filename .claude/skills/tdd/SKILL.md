---
name: tdd
description: Test-first loop for one mcp-gateway task. Use at the start of every task in PROGRESS.md.
---

# TDD loop

1. Read the task in `PROGRESS.md` and its definition in `docs/specs/000-build-spec.md` §8 (files, named test, done-when).
2. Write the named test first. Run it: `go test -race -run '^TestName$' ./path/...` (or `uv run pytest -k name`). Confirm it fails because the behaviour is missing.
3. Implement the smallest change that makes it pass. Keep `internal/clock` injection and `%w` error wrapping.
4. Run the area checks: `make check-fast` (always), plus `make policy-test`, `make hooks-test`, `make integration` when touched.
5. Record evidence in `PROGRESS.md`: status `done`, the command and a one-line result.
6. Commit with a Conventional Commit message (`feat(pipeline): ...`, `test: ...`, `docs: ...`). No attribution trailers.
