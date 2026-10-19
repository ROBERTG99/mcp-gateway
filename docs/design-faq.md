# Design FAQ

These are the questions a reviewer is most likely to ask about mcp-gateway, with short answers that point at the code. Paths are relative to the repository root, and each `path:line` was checked against the file when this page was written. Numbers come from a report or a make target named next to them; where a number would come from `docs/perf/`, this page links to `docs/perf/README.md` instead, because those files are regenerated. Known gaps live in `docs/KNOWN_LIMITATIONS.md`. Where something is design only and not built, the answer says so.

## 1. What happens if the gateway crashes right after an upstream write?

The write is not repeated. The agent may have to retry before it gets the result.

Before the upstream call, the gateway takes a lock on (tenant, key) that lasts 30 s by default (`internal/pipeline/pipeline.go:110-112`, `internal/pipeline/pipeline.go:227-235`, `internal/idempotency/redis.go:26-31`). It sends the key to the upstream in `_meta` (`internal/pipeline/pipeline.go:417-421`). The upstream runs the write and records the key in the same database transaction (`pkg/mcpserver/dedup.go:89-93`, `pkg/mcpserver/dedup.go:114-123`).

After a crash, a retry inside the lock time gets `-32047` (`internal/pipeline/idempotency.go:72-74`). A retry after it reaches the upstream, which returns the stored result and does not write again (`pkg/mcpserver/dedup.go:95-112`).

`evals/REPORT.md` (`make eval`, from `make chaos`) shows 0 duplicates for `chaos.v1`. That run makes the upstream stall after it commits, so the gateway times out. It does not kill the gateway process.

One gap, from reading the code and not covered by a test: audit events wait in memory until the next flush (`internal/audit/writer.go:124-133`, `internal/audit/writer.go:218-220`). A hard crash can lose events that were not flushed yet, including the event for a write that did happen.

## 2. Why two idempotency layers?

The gateway layer answers retries fast and without calling the upstream. It lives in Redis (default) or DynamoDB behind one interface (`internal/idempotency/idempotency.go:54-64`, `internal/idempotency/open.go:29-59`). It also rejects a key reused with other arguments or another tool (`internal/pipeline/idempotency.go:106-111`, `internal/idempotency/idempotency.go:78-88`).

It cannot make "the upstream committed" and "the gateway recorded it" one atomic step. If recording fails, the code only logs a warning (`internal/pipeline/idempotency.go:90-93`). A lock that expires during a slow call can also let a second caller in (see `docs/KNOWN_LIMITATIONS.md`).

The upstream layer closes that gap. It keeps a unique (tenant, key) row in the same transaction as the side effect (`pkg/mcpserver/mcpserver.go:155-166`, `pkg/mcpserver/dedup.go:77-125`). Large results are not stored by the gateway at all; a replay then goes to the upstream (`internal/pipeline/idempotency.go:41-47`, `internal/pipeline/idempotency.go:65-69`).

Costs:

- Two stores to run.
- Every upstream must use `pkg/mcpserver` or follow the same contract. Without it, only the first layer protects the write.
- I found no job that purges the example dedup tables (`migrations/00004_upstreams.sql:84-85`).

See `docs/adr/0008-upstream-side-dedup-second-idempotency-layer.md`.

## 3. How does one audit chain stay consistent across replicas?

Each replica queues events in memory and appends them in batches. Every batch runs in one Postgres transaction that first takes a fixed advisory lock (`internal/audit/postgres.go:17-18`, `internal/audit/postgres.go:120`). Under the lock it reads the newest row, extends the chain from there and commits (`internal/audit/postgres.go:124-130`, `internal/audit/postgres.go:138-152`, `internal/audit/event.go:222-235`). Unique constraints on `chain_pos` and `hash` make a fork fail loudly (`migrations/00002_audit.sql:9`, `migrations/00002_audit.sql:23`).

Sequence numbers come from a Postgres sequence, fetched in blocks (`internal/audit/postgres.go:226-345`). A `seq` is given when the call starts, and `chain_pos` when the batch is written. The two orders can differ, so `chain_pos` is a separate column that is not in the JSON (`internal/audit/event.go:29`). A test runs two writers against one database (`internal/audit/postgres_test.go:23-45`). A batch whose commit succeeded but whose acknowledgement was lost is recognised on retry (`internal/audit/postgres.go:29-68`).

Trade-off: the lock serialises all appends. Batching keeps it off the request path. For throughput see `docs/perf/README.md`.

## 4. What does `audit verify` prove, and what does it not?

It walks the chain in `chain_pos` order from the genesis hash. For every row, the stored hash must equal the SHA-256 of the previous hash plus the canonical JSON of the row, and `chain_pos` must have no gap (`internal/audit/postgres.go:181-224`, `internal/audit/event.go:199-220`). It prints the first bad `seq` (`cmd/mcp-gateway/audit.go:44-49`). The integration test edits and deletes rows as a superuser, past the append-only trigger, and expects a failure (`cmd/mcp-gateway/audit_integration_test.go:63-101`, `migrations/00002_audit.sql:30-43`).

It does not prove:

- That the newest rows are all still there. Nothing outside the table records the head hash, so deleting the last rows goes unseen (`docs/KNOWN_LIMITATIONS.md`). The command prints the head hash (`cmd/mcp-gateway/audit.go:48`); storing it elsewhere is the mitigation.
- That the whole chain was not rewritten. The hash has no secret key (`internal/audit/event.go:178-188`).
- Anything before the start row when `--from-seq` is used. It trusts that row's `prev_hash` (`internal/audit/postgres.go:192-194`, `internal/audit/event.go:204-206`).
- That every call was recorded. A call rejected with `-32048` is not audited, and unflushed events die with a crash.

## 5. How do you test default deny?

In Rego, `default decision` denies (`policies/authz.rego:10`). An allow needs both the tenant allowlist and the scope (`policies/authz.rego:17-19`, `policies/authz.rego:35`, `policies/authz.rego:43`). Unit tests cover empty input and a missing `readOnlyHint`, which counts as a write (`policies/authz_test.rego:52-64`, `internal/catalog/catalog.go:52-53`).

In Go, the engine returns a deny for no bundle, an evaluation error or a malformed decision (`internal/policy/policy.go:180-199`, `internal/policy/policy.go:214-239`). A broken bundle at start denies everything (`internal/policy/policy_test.go:102-118`). In the pipeline, any error from the engine falls into the deny branch (`internal/pipeline/pipeline.go:400-413`). Null arguments cannot skip argument rules (`internal/pipeline/policy_stage_test.go:60`).

Commands: `make policy-test` runs the Rego and Go policy tests. `make eval` runs `policy.v1`, which scores decision accuracy 1 on the test partition (`evals/REPORT.md`).

Caveat: the `policy.v1` labels were written from the spec, not taken from the bundle (`docs/DECISIONS.md`). Accuracy 1 shows the bundle agrees with that reading of the spec. It does not show the spec is right.

## 6. How do you hide tools from an agent?

Each principal (tenant, agent and scope set) gets its own MCP server object that holds only the tools it may see (`internal/proxy/proxy.go:172-206`, `internal/authn/principal.go:25-32`). The sync step asks the policy about every catalog tool with `args = null`, and an error hides the tool (`internal/proxy/factory.go:11-18`, `internal/proxy/proxy.go:227-255`).

A hidden tool is simply not registered. Calling it returns the same "unknown tool" error as a name that does not exist (`internal/proxy/factory_test.go:131-150`, `internal/mcperr/mcperr.go:54-58`). After a policy reload, cached servers are resynced and connected clients get `list_changed` (`internal/proxy/proxy.go:158-170`, `internal/server/server.go:169`, `internal/proxy/factory_test.go:152-187`).

Hiding is not the control. Every call runs the policy again (`internal/pipeline/pipeline.go:217-226`, `internal/pipeline/pipeline.go:390-413`), so a stale list cannot grant access.

Trade-offs:

- Listing ignores argument rules, so a tool that may be denied for its arguments (a large refund) is still listed (`policies/limits.rego:5-8`).
- One server object per distinct scope set stays in memory until idle for the cache time (`internal/proxy/proxy.go:208-223`).

## 7. How are approvals bound to the call?

An approval names the exact call: tenant, agent, server, tool, the SHA-256 of the canonical arguments and the idempotency key (`internal/approvals/store.go:55-66`, `internal/pipeline/pipeline.go:302-306`). When the policy says `require_approval`, the first call creates a pending approval and a webhook row in one transaction. A unique index folds identical concurrent holds into one (`internal/approvals/postgres.go:59-88`, `migrations/00005_approvals_one_pending.sql:13-14`).

An approver needs `mcpgw:approve` and the approval's tenant, and may not approve a call it made itself (`internal/approvals/service.go:154-157`, `internal/approvals/postgres.go:149-151`). The agent then retries with `_meta.approval_id` and the same key. `Consume` locks the row, checks the binding and marks it consumed in one transaction (`internal/approvals/postgres.go:179-212`, `internal/approvals/store.go:149-169`). Any field that differs gives `approval_mismatch`.

The policy then runs again. An approval lifts only a hold, and a deny still wins (`internal/pipeline/approval.go:57-72`, `policies/authz.rego:12-20`). Test: `internal/pipeline/approval_test.go:139-175`.

Self-approval is refused: the store's atomic `Decide` compares the approver with the held call's agent before approving (the agent may still withdraw its own hold by rejecting it) (`internal/approvals/postgres.go:149-151`, tested in `TestDecideRBACAndAudit`). Another principal of the same tenant with `mcpgw:approve` can still approve, so keep that scope for people, not agent tokens.

## 8. What happens when Redis goes down?

Redis holds the rate limits, budgets and idempotency records by default. Writes fail closed with `-32049`, because both the limiter and the idempotency store must answer (`internal/pipeline/pipeline.go:363-369`, `internal/pipeline/idempotency.go:55-60`). Tests: `internal/pipeline/ratelimit_test.go:59-78`, `internal/pipeline/idempotency_test.go:144-157`.

Reads carry on. They use a per-process in-memory limiter (`internal/pipeline/pipeline.go:370-375`, `internal/ratelimit/ratelimit.go:64-135`, wired at `internal/server/server.go:160`), and they never touch the idempotency store (`internal/pipeline/pipeline.go:227`).

Trade-offs:

- Each replica has its own bucket, so a tenant's real limit grows with the replica count. The in-memory daily budget restarts from zero when a replica restarts.
- Choosing DynamoDB for idempotency does not remove Redis from the write path, because the limiter is always Redis (`internal/server/server.go:160`).
- Redis does not gate readiness: `/readyz` stays 200 and reports `redis` as unreachable (`internal/server/server.go:220-224`), so replicas keep receiving the reads they can still serve (`TestRedisOutageKeepsReadsServed`).

## 9. How do the injection heuristics work, and what is their recall?

The scorer is deterministic. It normalises text (Unicode folding, hidden characters removed, look-alike letters mapped), builds variants for spaced-out letters and leetspeak, matches phrase rules, adds a fixed weight per signal and clips at 1 (`internal/injection/injection.go:166-181`, `internal/injection/injection.go:224-240`, `internal/injection/injection.go:242-293`). The weights are in `internal/injection/injection.go:31-34`. A base64 blob is decoded one level deep (`internal/injection/injection.go:262-273`).

A result is flagged at a score of 0.5 or more by default (`internal/pipeline/pipeline.go:122-124`, `internal/pipeline/output.go:46`). `mark` mode keeps the text and records a finding. `block` mode withholds it (`internal/pipeline/output.go:52-58`, `internal/pipeline/output.go:94-102`). The untrusted prefix and `untrusted: true` go on every result whatever its score (`internal/pipeline/output.go:50-90`).

Recall on the test partition is 0.8 (95% CI 0.6765-0.9167), against 0.9167 on dev, with a false positive rate of 0 (`evals/REPORT.md`, `make eval`). The data is synthetic and split per case, not per template, so this is not a held-out estimate for new phrasings (`docs/KNOWN_LIMITATIONS.md`).

A single signal can stay below the threshold: an override phrase alone is weighted 0.35 (`internal/injection/injection_test.go:25`). This is defence in depth. Marking and policy are the controls (`docs/adr/0005-heuristic-injection-scoring-vs-ml-classifier.md`).

## 10. How do you keep PII out of logs?

The production logger wraps the JSON handler in a redacting handler (`internal/telemetry/telemetry.go:131-135`). It redacts the message, string attributes, errors and nested values. It replaces the value of keys such as `authorization` and `token` whatever they contain (`internal/redact/slog.go:10-13`, `internal/redact/slog.go:28-75`). The detectors cover tokens, IBANs, cards, national IDs, emails and phones (`internal/redact/redact.go:43-55`).

The step log does not carry arguments (`internal/pipeline/pipeline.go:283-295`). Arguments are redacted before they go to the audit event, and before approval storage and webhooks (`internal/pipeline/pipeline.go:333-337`, `internal/approvals/service.go:90-92`). Tests: `internal/telemetry/telemetry_test.go:94-100`, `internal/server/logs_integration_test.go:39-50`.

Limits:

- Detection is pattern based. Names and street addresses are not detected, and the recall of 1.0 in `evals/REPORT.md` is on synthetic formats only (`docs/KNOWN_LIMITATIONS.md`).
- Redaction does not apply to what the agent receives, or to the replay copy of a write result in the idempotency store (`internal/pipeline/idempotency.go:81-94`). That copy lives for 24 h by default (`internal/pipeline/pipeline.go:107-109`). Treat that store as sensitive.

## 11. How is tenant isolation enforced?

The tenant comes from the verified JWT and nowhere else (`internal/authn/verifier.go:79-117`). The pipeline uses that principal for the policy input, rate limits, idempotency and the audit event (`internal/pipeline/pipeline.go:395-399`, `internal/pipeline/pipeline.go:363`, `internal/pipeline/idempotency.go:55`, `internal/pipeline/pipeline.go:339-341`). The `_meta` sent upstream is built fresh, so client-supplied values are not copied (`internal/pipeline/pipeline.go:417-426`).

Shared stores key on the tenant (`internal/idempotency/redis.go:23`, `internal/ratelimit/redis.go:70`). The policy checks a per-tenant upstream allowlist (`policies/authz.rego:35`, `policies/data/tenants.json:2-4`). Other tenants' approvals look missing (`internal/approvals/store.go:152-153`). Admin queries force the viewer's tenant, and foreign audit events return not found (`internal/admin/admin.go:171-173`, `internal/admin/admin.go:233-238`). Upstream helpers require a tenant, and the example queries filter on it (`pkg/mcpserver/mcpserver.go:128-130`, `internal/upstreams/orders.go:62-63`).

Limits: tenants share one Postgres, one Redis and one policy bundle, and I found no row-level security. An upstream trusts the tenant in `_meta.mcpgw`, so it must be reachable only from the gateway.

## 12. How is gateway overhead measured?

Overhead is the time of one call minus the time spent in the upstream call (`internal/pipeline/pipeline.go:182`, `internal/pipeline/pipeline.go:236-240`). It feeds the `mcpgw_overhead_seconds` histogram (`internal/telemetry/metrics.go:44-46`, `internal/telemetry/metrics.go:107-112`).

Three routes use it:

- `make bench` runs `BenchmarkGatewayOverhead`. It uses the real policy bundle, injection scorer, redactor and audit writer, but in-memory stores and an instant in-process upstream (`internal/pipeline/bench_test.go:44-100`). It measures the CPU cost of the stages. It leaves out Redis and Postgres round trips. `scripts/bench_budget.py` checks its budget.
- `make loadtest` runs the full compose stack. It reports throughput, error ratio, client-side latency and gateway memory. It does not read the overhead histogram.
- In a running deployment, the histogram includes real Redis and Postgres calls. The `GatewayOverheadSLO` alert watches its p95 (`deploy/prometheus/alerts.yaml:31-37`).

On a saturated CPU the histogram also counts time spent waiting to run, so it can overstate the gateway's own cost (`docs/KNOWN_LIMITATIONS.md`). The results are in `docs/perf/README.md`. They are not repeated here because that directory is regenerated. Policy evaluation has its own measure, `latency.v1` (`evals/REPORT.md`).

## 13. What happens under audit backpressure?

Every call reserves an audit slot first, reads included (`internal/pipeline/pipeline.go:162-172`). `Reserve` takes a slot from a bounded channel, waits at most the enqueue timeout (1 ms by default), then needs a sequence number (`internal/audit/writer.go:94-122`, `internal/config/config.go:66-69`). If it cannot, the call fails with `-32048` before policy or upstream run (`internal/pipeline/pipeline.go:165-170`). `Commit` never blocks because the slot is already held (`internal/audit/writer.go:124-133`).

The writer keeps failed batches and retries them (`internal/audit/writer.go:164-200`). One event the database refuses is replaced by a placeholder so it cannot stall the log (`internal/audit/writer.go:251-262`). Tests: `internal/audit/writer_test.go:138-160`, `internal/pipeline/audit_test.go:132-147`, which also checks the upstream was not called. The counter is `mcpgw_audit_rejections_total` (`internal/telemetry/metrics.go:58`).

Trade-off: audit completeness wins over availability. In a Postgres outage, once the prefetched sequence block is used (`internal/audit/postgres.go:253-258`, `internal/server/server.go:136`) or the queue is full, every call fails, reads too. A rejected call is not audited (`docs/KNOWN_LIMITATIONS.md`).

## 14. Can a team add an upstream in a day?

For a stateless read tool, yes: it needs config and no gateway code. For a real write system, the day goes on the upstream, not the gateway. The one-day target is a design goal and is not measured.

The steps are in `.claude/skills/add-upstream-server/SKILL.md:8-12`:

- Add an entry to `config/upstreams.yaml:7-15`. The name rule is in `internal/catalog/catalog.go:27` and the checks in `internal/config/config.go:195-219`.
- Allow it per tenant in `policies/data/tenants.json:2-4`. Scopes `<server>:read` and `<server>:write` work with no new rule (`policies/authz.rego:37-43`).
- Tools appear as `<server>.<tool>` (`internal/catalog/catalog.go:49-50`).

`tests/integration/add_upstream_test.go:34-143` does this for an echo upstream. It checks the tool is visible only to an allowed tenant with the scope, that output is marked untrusted, and that the call is audited.

For a write system, the upstream must also:

- Set `readOnlyHint`. A tool without it counts as a write and needs a key (`internal/catalog/catalog.go:52-53`).
- Honour the key and tenant. Go upstreams can use `pkg/mcpserver` (`pkg/mcpserver/mcpserver.go:136-179`, example `cmd/upstream-orders/main.go:25-33`).

Caveat: the gateway reads `upstreams.yaml` once at start (`internal/server/server.go:68`), so a new entry needs a restart. Policy data does hot reload (`internal/policy/policy.go:155-176`).

## 15. What would have to change for multiple regions?

This is design only. Nothing in the repository is region aware. The only "region" is the AWS SDK setting for DynamoDB (`internal/config/config.go:57`). A multi-region deployment would need these changes:

- **Audit chain.** One advisory lock, one sequence and one chain per database (`internal/audit/postgres.go:17-18`, `internal/audit/postgres.go:120`, `internal/audit/postgres.go:327`). A lock cannot span regions. The likely design is one chain per region, with head hashes anchored elsewhere. `VerifyPostgres` and the admin audit view would change.
- **Idempotency.** DynamoDB conditional writes are atomic within one region. Global tables replicate asynchronously, so two regions could both acquire a key (`internal/idempotency/dynamodb.go:92-97`). The safe option is a home region per tenant for writes, with upstream dedup as the backstop. Redis keys are local too (`internal/idempotency/redis.go:23`).
- **Rate limits and budgets.** Redis counters are per region (`internal/ratelimit/redis.go:70`). Use per-region budgets or the same home region.
- **Approvals.** Row locks assume a single primary (`internal/approvals/postgres.go:179-212`).
- **Policy and catalog.** Both load per instance (`internal/policy/policy.go:155-176`, `internal/server/server.go:68`). Rollout would need to compare bundle hashes (`internal/policy/policy.go:110-116`).
- **Sessions.** They live in each replica's memory (`internal/proxy/proxy.go:77-78`).

The failure rule would stay "fail closed", which means a partition between regions refuses writes.
