---
paths: [".claude/hooks/**/*.py", "scripts/**/*.py", "tests/harness/**/*.py"]
---

# Python rules

- Hooks use the standard library only (they run before `uv sync`). Scripts and tests may use the dev group in `pyproject.toml`.
- Type hints everywhere; `mypy --strict` and `ruff check` must be clean (`make typecheck`, `make lint`).
- Run with `uv run`; never install packages globally.
- Hooks fail open on their own bugs (print to stderr, exit 0) but fail closed on policy matches (exit 2 with a reason).
