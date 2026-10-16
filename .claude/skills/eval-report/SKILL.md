---
name: eval-report
description: Regenerate evals/REPORT.md from a fresh offline eval run and compare against the baseline.
---

# Eval report

1. `make eval` (runs `go run ./cmd/evalgen run --out evals/reports` and the evalgate check).
2. Never edit `evals/REPORT.md` by hand; it is rendered from `evals/reports/*.json`.
3. If a gate fails, run the `eval-analyst` subagent on the dev partition. Do not touch `evals/datasets/test/`.
4. Only refresh `evals/baseline/` deliberately (`go run ./cmd/evalgen baseline`), with the reason in `docs/DECISIONS.md`.
5. Cite the report path next to any number copied into the README.
