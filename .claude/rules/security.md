---
paths: ["internal/**", "pkg/**", "cmd/**"]
---

# Security rules

- Redact before logging: anything that can hold arguments, results or tokens goes through `internal/redact`. Never log the `Authorization` header or a JWT.
- The tenant comes from the verified token, never from the request body or `_meta`.
- Fail closed: no policy, no decision; idempotency store down means writes are refused; audit saturation means `-32048`.
- Side effects happen only after policy, approval and idempotency acquisition.
- RS256 only, `kid` required. Secrets come from the environment only.
- Admin endpoints check scopes and the approver's tenant.
