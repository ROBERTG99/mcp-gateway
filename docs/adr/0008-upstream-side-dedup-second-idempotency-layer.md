# 0008. Upstream-side dedup as a second idempotency layer

- Status: Accepted (2026-10-17)

## Context

The gateway records each write key in Redis or DynamoDB (ADR 0003). That record can fall short in three ways. The gateway can crash after the upstream committed but before it recorded the result. The lock can expire while a slow upstream call still runs. A result can be too large to store. In each case a retry could reach the upstream twice. For a refund, a second write is the failure this design exists to prevent.

## Decision

The upstream also deduplicates writes. The gateway forwards the key in `_meta.idempotency_key` with `_meta.mcpgw` (`internal/pipeline/pipeline.go`, `forward`), and `pkg/mcpserver` uses it.

- `WriteTool` in `pkg/mcpserver/mcpserver.go` rejects a write without a valid key and runs the handler through `DedupStore.Once` for `(tenant, key)`.
- `NewPostgresDedup` in `pkg/mcpserver/dedup.go` claims the key with `INSERT ... ON CONFLICT DO NOTHING` into `<schema>.mcp_idempotency`, whose primary key is `(tenant, key)` (`migrations/00004_upstreams.sql`). The handler runs in the same transaction, and the result is stored before commit. The side effect and the dedup record commit together or not at all.
- A retry reads the stored result and returns it. The same key with another tool or arguments hash returns `-32040`.
- The transaction uses a context detached from the caller (`context.WithoutCancel`, capped by a timeout). A write that started finishes even if the caller timed out, so the retry replays it (`docs/DECISIONS.md`).
- The example write tables also have `UNIQUE (tenant, idempotency_key)` (`migrations/00004_upstreams.sql`).
- When a stored gateway result is too large, the gateway records the key as completed without a body and forwards the retry, so this layer answers it (`internal/pipeline/idempotency.go`, `storedResult`).

`TestWriteToolDedup` and `TestWriteToolConcurrentDedup` (`pkg/mcpserver/mcpserver_test.go`) and `TestPostgresDedupConcurrent` (`pkg/mcpserver/postgres_test.go`) cover it. The chaos eval counts refunds beyond the distinct keys sent; the result is in `evals/REPORT.md` (`make chaos`, `make eval`).

## Alternatives considered

- **Gateway-side idempotency only.** Rejected: it fails in the crash, lock-expiry and oversized-result cases above, because the gateway cannot know what the upstream committed.
- **Upstream-side dedup only.** Rejected: every retry, in-progress call and key conflict would reach the upstream. The gateway layer answers the common retry without an upstream call.

## Consequences

- Authors of new upstreams must use `pkg/mcpserver` or provide the same guarantee. The gateway cannot check that they do.
- The gateway layer is the fast path, and the upstream layer is the safety net. The retention job deletes approvals, delivered outbox rows and findings (`internal/approvals/postgres.go`) but not `mcp_idempotency` rows, so those grow without a cleanup job in v0.1.0.
