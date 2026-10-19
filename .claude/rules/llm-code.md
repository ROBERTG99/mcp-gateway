---
paths: ["**/*.go", "**/*.py", "**/*.ts"]
---

# LLM code rules

This repository calls no model, and no provider SDK may be imported (golangci-lint depguard enforces it).
If model code is ever added, Appendix B of `docs/specs/000-build-spec.md` applies in full:

- Load the `claude-api` skill first. One gateway module per language owns every model call.
- Model IDs come from configuration; defaults `claude-opus-5-5` and `claude-haiku-5-5`, no date suffixes.
- Structured output with schemas, adaptive thinking with explicit effort, no forced tool choice, no prefill.
- Check `stop_reason`; retry only 408, 409, 429, 5xx and connection errors with backoff and jitter.
- Every provider has a deterministic fake driven by recorded fixtures; CI never calls a live model.
