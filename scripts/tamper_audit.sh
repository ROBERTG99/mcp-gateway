#!/usr/bin/env bash
# Incident drill: modify one audit row as the Postgres superuser (bypassing the append-only
# trigger with session_replication_role=replica) and print its seq. `mcp-gateway audit verify`
# must then fail naming that seq.
#   scripts/tamper_audit.sh [seq]          tamper with seq (default: the middle row)
#   scripts/tamper_audit.sh --restore seq  undo the edit, so the chain verifies again
set -euo pipefail
COMPOSE=${COMPOSE:-docker compose}
psql() { $COMPOSE exec -T postgres psql -U postgres -d mcpgw -v ON_ERROR_STOP=1 -At "$@"; }
if [[ "${1:-}" == "--restore" ]]; then
  seq=${2:?usage: tamper_audit.sh --restore seq}
  psql -c "SET session_replication_role = replica; UPDATE audit_events SET agent = left(agent, -length('-tampered')) WHERE seq = ${seq//[^0-9]/} AND agent LIKE '%-tampered';" >/dev/null
  echo "$seq"
  exit 0
fi
seq=${1:-$(psql -c "SELECT seq FROM audit_events ORDER BY chain_pos OFFSET (SELECT count(*) / 2 FROM audit_events) LIMIT 1")}
if [[ -z "$seq" ]]; then
  echo "no audit events to tamper with" >&2
  exit 1
fi
psql -c "SET session_replication_role = replica; UPDATE audit_events SET agent = agent || '-tampered' WHERE seq = ${seq//[^0-9]/};" >/dev/null
echo "$seq"
