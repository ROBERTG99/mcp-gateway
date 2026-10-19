---
name: quality-gate
description: Run the mcp-gateway quality gate before closing a milestone or opening a pull request, and summarise failures.
---

# Quality gate

1. `make check-fast` (lint and unit, < 60 s). Fix everything before continuing.
2. `make lint typecheck` then `make test` (integration needs Docker; start it per `docs/DEVELOPMENT.md`).
3. `make policy-test hooks-test helm-check promtool`.
4. `make eval` and `make bench` (budgets in `scripts/bench_budget.py`).
5. `make security`; document any accepted finding in `docs/security.md` with a reason.
6. Run the `reviewer` subagent on the milestone diff and fix its blockers.
7. Write a one-line result per command into `PROGRESS.md`.
