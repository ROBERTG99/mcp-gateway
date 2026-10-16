---
paths: ["**/*.md", "docs/**"]
---

# Documentation rules

- Every number in the README and docs cites the command or generated report it came from, next to it.
- Never invent metrics, screenshots or results. If something was not run, say so.
- Markdown passes markdownlint-cli2 (`make lint`).
- ADRs live in `docs/adr/NNNN-title.md` and name the rejected alternative (use the `new-adr` skill).
- Decisions taken during the build go in `docs/DECISIONS.md` with date, decision, alternatives and reason.
