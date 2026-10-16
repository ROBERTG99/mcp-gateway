---
name: audit-forensics
description: Investigate the audit log after an incident - verify the chain, find the first tampered seq, and trace a call by approval or idempotency key.
---

# Audit forensics

1. Verify the chain: `make audit-verify` (or `mcp-gateway audit verify --from-seq N`). Exit 1 names the first bad `seq`.
2. Inspect around it: `GET /admin/audit?from=...&to=...` or the console audit page; compare `prev_hash` with the previous row's `hash`.
3. Trace a call: filter by `approval_id`, or search `idempotency_key`; approval decisions appear as `server=mcpgw`, `tool=approval.approve|approval.reject`.
4. Never modify audit rows; export evidence with `docker compose exec postgres psql ... \copy`.
5. Record findings in an incident note; `docs/runbook.md` covers archival and key rotation.
