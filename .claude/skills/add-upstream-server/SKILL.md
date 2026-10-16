---
name: add-upstream-server
description: Connect a new upstream MCP server to the gateway with configuration and one Rego rule only (the "one day" journey).
---

# Add an upstream server

1. Add an entry to `config/upstreams.yaml`: `name` (`^[a-z][a-z0-9_]{1,31}$`), `url` (Streamable HTTP endpoint), `timeout`.
2. Allow it for tenants in `policies/data/tenants.json` (and `config/tenants.yaml` if limits differ).
3. Scopes `<name>:read` / `<name>:write` work automatically; add special limits in `policies/limits.rego` with tests.
4. Reload: the catalog picks it up within `MCPGW_CATALOG_REFRESH`; `GET /admin/upstreams` shows health and tools.
5. `tests/integration/add_upstream_test.go` shows the pattern end to end.
