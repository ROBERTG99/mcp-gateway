---
name: add-endpoint
description: Add an admin REST endpoint - chi route, RBAC scope, OpenAPI entry, handler test and Schemathesis coverage.
---

# Add an admin endpoint

1. Describe it first in `api/openapi.yaml` (path, parameters, responses, RFC 9457 problem responses, security scopes).
2. Write the handler test in `internal/admin` (RBAC: 401 without token, 403 with the wrong scope or tenant).
3. Add the route in `internal/admin/router.go` behind `requireScope(...)`.
4. Errors use `problem.Write` (RFC 9457 `application/problem+json`).
5. Run `make contract` (Schemathesis against the running stack) and `make check-fast`.
