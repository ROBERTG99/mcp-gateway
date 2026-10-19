# 0003. Redis and DynamoDB idempotency stores behind one interface

- Status: Accepted (2026-10-17)

## Context

Every write needs a key, and a retry with the same key and arguments must not run the write twice through the gateway. The gateway keeps this state per `(tenant, key)`. Redis is the default because the stack already uses it for rate limits. Some deployments have no Redis but do have DynamoDB. The pipeline should not care which one it talks to.

## Decision

The pipeline depends on the `idempotency.Store` interface in `internal/idempotency/idempotency.go`: `Acquire`, `Complete`, `Release` and `Ping`. `Acquire` returns one of four outcomes: acquired, replay, conflict (`-32040`) or in progress (`-32047`). The shared `decide` function maps a stored record to an outcome, so the backends cannot disagree on that rule.

- `internal/idempotency/redis.go` is the default. It uses one key per record, `mcpgw:idem:{tenant}:{key}`, and Lua scripts so acquire, complete and release are atomic.
- `internal/idempotency/dynamodb.go` is the option. It uses conditional writes on the partition key `{tenant}#{key}`. It checks expiry in the condition (`expires_at_ms`), because DynamoDB deletes expired items late.
- `internal/idempotency/memory.go` serves tests.
- `idempotency.Open` in `internal/idempotency/open.go` picks the backend from `MCPGW_IDEMPOTENCY_STORE` (`redis`, `dynamodb` or `memory`; `internal/config/config.go`).

All backends must pass the conformance suite `storetest.Run` in `internal/idempotency/storetest/storetest.go`. It covers replay, conflict, release, lock and record expiry, tenant isolation, a single winner under 24 concurrent acquires, and owner-only completion. The memory and miniredis stores run it in unit tests (`internal/idempotency/memory_test.go`). Real Redis and DynamoDB run it as integration tests (`make test`).

## Alternatives considered

- **Redis only.** Rejected: it ties the gateway to one store and blocks deployments that cannot run Redis.
- **Postgres for idempotency state.** Rejected: it adds a write to the database on every call, and the gateway's Postgres is the audit log's store. A Redis or DynamoDB outage already fails writes closed (`-32049`) without touching audit.
- **Two backends without a shared test suite.** Rejected: the stores would drift. The suite makes a third backend a matter of passing it.

## Consequences

- A new backend implements four methods and passes `storetest.Run`.
- Locks identify their holder by the tool-bound args hash, not an owner token. A call that outlives the lock lifetime can lose its lock to a retry. The upstream-side dedup in ADR 0008 still prevents a duplicate write (`docs/KNOWN_LIMITATIONS.md`).
- The DynamoDB "single winner" case failed once against dynamodb-local under parallel load. It is recorded, not retried or weakened (`docs/KNOWN_LIMITATIONS.md`).
- Gateway-side idempotency is one layer of two. See ADR 0008.
