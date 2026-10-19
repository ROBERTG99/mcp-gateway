#!/usr/bin/env bash
# Contract test of the admin API against api/openapi.yaml on the running stack (make contract).
# Mints a cross-tenant admin token from the dev issuer and runs Schemathesis with its default
# checks (no server errors, documented status codes, content types, headers and response schemas).
set -euo pipefail
SCHEMATHESIS_VERSION=${SCHEMATHESIS_VERSION:-4.31.1}
ADMIN_URL=${ADMIN_URL:-http://localhost:8200}
ISSUER=${DEV_ISSUER_URL:-http://localhost:8212}
token=$(go run ./cmd/mcp-gateway dev-token --issuer "$ISSUER" --tenant '*' --agent schemathesis \
  --scopes mcpgw:admin,mcpgw:audit:read,mcpgw:approve)
exec uvx "schemathesis==${SCHEMATHESIS_VERSION}" run api/openapi.yaml \
  --url "$ADMIN_URL" \
  --header "Authorization: Bearer ${token}" \
  --max-examples "${SCHEMATHESIS_MAX_EXAMPLES:-30}" \
  --seed 20260 \
  "$@"
