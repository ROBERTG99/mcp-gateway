#!/usr/bin/env bash
# make demo: the scripted scenario of spec §16 against the running compose stack (make up && make
# seed), offline and with no model. demo-agent prints PASS or FAIL per step:
#   1 only allowed tools are listed          5 an injected CRM note is flagged as untrusted
#   2 a USD 50 refund executes               6 the audit chain verifies
#   3 a USD 500 refund is held, approved     7 scripts/tamper_audit.sh edits one row as superuser
#     through the admin API, retried once      and verify fails at exactly that seq
#   4 the orders upstream is killed mid-call (its refund stalls past the gateway timeout and the
#     container is restarted); the same-key retry leaves one refund row
# Afterwards the tampered row is restored and `mcp-gateway audit verify` must pass again, so the
# stack stays usable (make verify runs the e2e tests next).
set -euo pipefail
cd "$(dirname "$0")/.."
COMPOSE=${COMPOSE:-docker compose}
GATEWAY_URL=${GATEWAY_URL:-http://localhost:8210/mcp}
ADMIN_URL=${ADMIN_URL:-http://localhost:8200}
DEV_ISSUER_URL=${DEV_ISSUER_URL:-http://localhost:8212}
WEBHOOK_ADMIN_URL=${WEBHOOK_ADMIN_URL:-http://localhost:8241}

if ! curl -fsS "$ADMIN_URL/readyz" >/dev/null; then
  echo "demo: the stack is not ready at $ADMIN_URL (run make up && make seed)" >&2
  exit 1
fi
tamper_log=$(mktemp)
trap 'rm -f "$tamper_log"' EXIT
status=0
go run ./cmd/demo-agent --scenario --gateway "$GATEWAY_URL" --admin "$ADMIN_URL" --issuer "$DEV_ISSUER_URL" \
  --webhook-admin "$WEBHOOK_ADMIN_URL" \
  --on-timeout-cmd "$COMPOSE restart upstream-orders && $COMPOSE up -d --wait upstream-orders" \
  --tamper-cmd "scripts/tamper_audit.sh | tee $(printf %q "$tamper_log")" || status=$?

if [[ -s "$tamper_log" ]]; then
  seq=$(tail -n1 "$tamper_log")
  scripts/tamper_audit.sh --restore "$seq" >/dev/null
  if $COMPOSE exec -T gateway /mcp-gateway audit verify >/dev/null; then
    echo "PASS restore: seq $seq restored; mcp-gateway audit verify passes again"
  else
    echo "FAIL restore: mcp-gateway audit verify still fails after restoring seq $seq"
    status=1
  fi
fi
exit "$status"
