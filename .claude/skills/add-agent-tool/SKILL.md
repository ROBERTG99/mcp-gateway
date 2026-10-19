---
name: add-agent-tool
description: Add a tool to an example upstream (ledger, crm, orders) with its scope, idempotency behaviour and policy case.
---

# Add an upstream tool

1. Define it in the upstream's `internal/upstreams/<server>.go` with `mcpserver.ReadTool` or `mcpserver.WriteTool` (writes set `readOnlyHint=false` and get upstream-side dedup).
2. Input schema with required fields; amounts are integer cents with a `currency` when relevant.
3. Policy: scope `<server>:read` or `<server>:write` is automatic in `policies/authz.rego`; add limits in `policies/limits.rego` if it moves money, with tests in `limits_test.rego`.
4. Add a policy eval case (`add-eval-case` skill) and a test in the upstream package.
5. Update the tool table in `README.md` and `docs/architecture.md`.
