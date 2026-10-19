---
paths: ["policies/**/*.rego", "policies/**/*.json"]
---

# Rego rules

- Package `mcpgw.authz` (and sub-packages imported by it). Use OPA v1 syntax (`if`, `contains`).
- Default deny: `default decision := {"allow": false, "require_approval": false, "reasons": ["default_deny"]}`.
- Every rule has a test in the `*_test.rego` file beside it; run `make policy-test`.
- Never use `http.send`, `net.*`, `opa.runtime` or any other non-deterministic builtin.
- Argument rules must not fire when `input.args` is `null` (that is `tools/list`).
- Reasons are stable snake_case identifiers; tests and evals assert on them.
- Format with `mcp-gateway policy fmt policies`.
