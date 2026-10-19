# 0004. Hash-chained Postgres audit log instead of append-only S3

- Status: Accepted (2026-10-17)

## Context

Every tool call, including denials and holds, needs a record that an auditor can query and that shows tampering. Operators filter by tenant, tool, status and approval, and the console pages through the results. Several gateway replicas write at once, and they must extend one history.

## Decision

Store audit events in the Postgres table `audit_events` (`migrations/00002_audit.sql`) and link them in a SHA-256 hash chain.

- Each event's hash is `sha256(prev_hash + canonical_json(event without hash))` (`internal/audit/event.go`, `ComputeHash`). The first `prev_hash` is 64 zeros.
- The writer appends batches in one transaction that first takes `pg_advisory_xact_lock` (`internal/audit/postgres.go`, `append`). It reads the head, assigns a gap-free `chain_pos` and writes the rows. Replicas therefore extend one chain. `TestAuditChainAppendPostgres` runs two writers against one table and verifies the result.
- A trigger rejects `UPDATE`, `DELETE` and `TRUNCATE` (`audit_events_append_only` in the migration). The gateway role `mcpgw_app` has `SELECT` and `INSERT` only.
- `mcp-gateway audit verify [--from-seq N]` walks the chain in `chain_pos` order (`cmd/mcp-gateway/audit.go`, `internal/audit/postgres.go`, `VerifyPostgres`). It exits 0 when the chain is intact and 1 naming the first bad `seq`. A deleted row shows as a `chain_pos` gap. `TestAuditVerifyDetectsTamper` changes a row as superuser and expects exit 1.
- A call reserves its audit slot before any side effect. If none is free in time, the call fails with `-32048` and nothing runs (`docs/DECISIONS.md`).

## Alternatives considered

- **Append-only S3 with object lock.** Rejected: it cannot be filtered by tenant, tool or approval without an extra index, and it has no cheap way to page the console. Replicas appending to one ordered chain would need external coordination. Object lock stops deletion but does not link events.
- **Postgres without a chain, only the trigger and role grants.** Rejected: a superuser can change rows without any trace. The chain makes that visible.

## Consequences

- One advisory lock serialises appends from all replicas. Batching reduces the cost.
- Verification proves that stored rows are unchanged, in order and complete up to the head. It does not detect removal of the newest rows by a superuser, because there is no external copy of the head hash. Exporting the head hash is a runbook step (`docs/KNOWN_LIMITATIONS.md`).
- A superuser can drop the trigger. The chain, not the trigger, is the detection.
- A call rejected for audit backpressure is not itself audited (`docs/KNOWN_LIMITATIONS.md`).
