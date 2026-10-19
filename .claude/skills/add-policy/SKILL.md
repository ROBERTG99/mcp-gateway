---
name: add-policy
description: Add or change a Rego rule in policies/ with tests, golden decisions and eval cases, keeping default deny.
---

# Add a policy rule

1. Write the test first in the sibling `*_test.rego` (allow, hold and deny cases, plus `args == null` for listing).
2. Implement in `policies/*.rego` (package under `mcpgw.authz`), stable snake_case reasons, no `http.send`.
3. `make policy-test`; then add golden decisions in `internal/policy/testdata/golden.json` if the contract changes.
4. Reload a running gateway with `POST /admin/policy/reload` (admin scope) and check `/readyz`.
5. Ask the `policy-reviewer` subagent for a least-privilege review.
