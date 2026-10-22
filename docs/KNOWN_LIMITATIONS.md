# Known limitations

What is not done, or done differently from the requirements, and why. Each item names where it is tracked.

- The Claude Code settings are kept as a template at `docs/harness/settings.json`; copy it to `.claude/settings.json` to enable the guard hooks locally.
- `scripts/install_tools.sh` downloads hadolint, trivy and promtool release binaries without SHA-256 verification. Where the GitHub release host is blocked (as in some sandboxed development environments), so the script falls back to the official container images (hadolint, promtool) and `go install` (trivy), which are verified by the registry digest and the Go checksum database respectively. Pinning release checksums is a follow-up.
- The three example upstreams share one database role (`mcpgw_upstream`) with write access to all three schemas. A per-schema role per upstream would show least privilege better; the gateway's own role (`mcpgw_app`) is least privilege.
- Idempotency locks identify their holder by the (tool-bound) args hash, not a random owner token. If an upstream call outlives `MCPGW_IDEMPOTENCY_LOCK_TTL` (30 s, three times the upstream timeout), a second caller can acquire the key and the first caller's later release could drop the second caller's lock. Upstream-side dedup (`pkg/mcpserver`, unique `(tenant, idempotency_key)`) still prevents a duplicate write. Owner tokens are a follow-up.
- The 2026-07-28 stateless protocol has no `Mcp-Session-Id`; per-principal session binding (`TestSessionBoundToPrincipal`) applies to clients that negotiate the legacy session protocol.
- A call rejected because the audit buffer is saturated (`-32048`) is not itself audited (there is no slot to write it); it is logged and counted by `mcpgw_audit_rejections_total`.
- The NFR-9 result limit (4 MiB) is checked after the MCP client has read and decoded the upstream response, so a hostile or broken upstream can make the gateway buffer one larger response before it is refused. Capping the read in the upstream HTTP transport is a follow-up (`docs/security.md`, "Limits and their edges").
- Truncation of the newest audit rows by a superuser is not detectable by `audit verify` alone (no external anchor of the head hash); periodic export of the head hash is a runbook item.

## Gateway behaviour worth knowing

- **Audit events in flight are lost on a hard crash.** `Ticket.Commit` queues the event in memory (`internal/audit/writer.go`); batches are persisted every 50 ms or 500 events, and a graceful shutdown drains the queue. A `kill -9` or node loss after an upstream write but before the flush leaves that write without an audit event. A durable local queue (or writing the event before the upstream call and completing it after) would close the gap.
- **Redis stays on the write path with the DynamoDB store.** Rate limits and daily budgets always use Redis (`internal/server/server.go`), and the limiter fails closed for writes, so choosing `MCPGW_IDEMPOTENCY_STORE=dynamodb` does not remove Redis as a dependency of writes. Reads continue on a per-process bucket while Redis is down (`TestRedisOutageKeepsReadsServed`).
- **Stored replay results are not redacted.** For an idempotent replay the store keeps the upstream's raw result for `MCPGW_IDEMPOTENCY_TTL` (default 24 h) in Redis or DynamoDB. Redaction covers the audit log, approval webhooks and logs, not this copy; protect the store accordingly.
- **The example upstreams' dedup tables are never purged.** `ledger`, `crm` and `orders` keep their `mcp_idempotency` rows forever (`migrations/00004_upstreams.sql`); a real upstream needs a retention job.
- **Upstreams trust the gateway.** The example upstreams take the tenant from `_meta.mcpgw.tenant` without checking a token; they must be reachable only from the gateway (network policy, mTLS or a service mesh), as `docs/adr/0001-gateway-auth-vs-per-server-auth.md` states.
- **Adding an upstream needs a restart.** `upstreams.yaml` is read at start-up; policy data hot-reloads, the upstream list does not.
- **Re-execution of an approved write inside its window.** Within `MCPGW_IDEMPOTENCY_TTL` of consumption, a same-key retry of an approved call continues to the idempotency stage; if the first attempt failed, or its idempotency record could not be completed, the write runs again and only the upstream's own dedup (`pkg/mcpserver`) keeps it single. Upstreams without dedup should not be put behind approvals.
- **Tool annotations are hints.** A tool listed under `write_tools` in `upstreams.yaml` is always a write (`TestWriteToolsOverrideReadOnlyHint`); a tool not listed there is classified by the upstream's own `readOnlyHint`, so an operator connecting a new upstream should list its write tools.
- **No cumulative refund limit.** Each refund is checked on its own amount; five refunds of 10,000 cents on one order are each allowed (the example upstream still refuses refunds beyond the order total).
- **`/readyz` is unauthenticated** and pings Postgres and Redis on every request, returning the policy compile error text; keep the admin port inside the cluster (the chart's NetworkPolicy does) and point probes at it only.
- **No read or write timeouts on the listeners.** MCP keeps server-to-client SSE streams open, so only a header timeout and an idle timeout are set; a slow authenticated client can hold a connection while sending a body of up to 1 MiB.
- **A lone override phrase is not flagged.** "Ignore all previous instructions" alone scores 0.35, below the default threshold of 0.5; a second signal is needed. The output is marked untrusted either way (`docs/evaluation.md`).

## Docker, Helm and CI

- **Image size.** The gateway image is 17.6 MB compressed, under the 40 MB budget, but 57.6 MB unpacked (`make image-size`, 2026-10-17). The budget is applied to the compressed size (`docs/DECISIONS.md`); an unpacked budget of 40 MB would need a smaller binary than OPA, the DynamoDB SDK and the rest allow.
- **Helm.** The NetworkPolicy restricts ingress only; egress stays open because the gateway's targets (upstreams, the issuer's JWKS, Postgres, Redis, DynamoDB) depend on the deployment. `make kind-smoke` (`scripts/kind_smoke.sh`, restricted Pod Security admission on a local kind cluster) was not run: kind is not installed in the build environment. `make helm-check` downloads the Kubernetes JSON schemas for kubeconform from GitHub when it runs.
- **CI and release.** The workflows were checked with actionlint and `tests/harness/test_workflows.py`, but no GitHub Actions run was observed from this environment. Actions are pinned to major-version tags, not commit SHAs (`docs/DECISIONS.md`). `release.yml` publishes on any pushed `v*` tag: protect `v*` tags with a ruleset, require a green `ci` run on the tagged commit, and add image signing (cosign) and an SBOM attestation before relying on it.

## Console sign-in is development-only

The console signs in with the dev issuer's personas: there is no password and no external identity provider. Sign-in is mounted only when `MCPGW_DEV_ISSUER_URL` is set (compose sets it; the default is empty, which disables sign-in), and anyone who can reach port 8200 on such a stack can sign in as any persona. Cookies get the `Secure` flag only with `MCPGW_CONSOLE_SECURE_COOKIES=true` (default false for the local HTTP stack). Sessions live in memory (capped at 10,000), so they do not survive a restart and are not shared between replicas. A production deployment needs an OIDC login in front of the console; that is out of scope for v0.1.0.

## Eval results are on synthetic, template-built data

- Injection recall on the test partition is exactly at the gate: 0.8000 (95% CI 0.6765-0.9167) against 0.9167 on dev (`evals/REPORT.md`, `make eval`). The partitions are split per case, not per template: all 18 malicious templates in test also occur in dev with different fills, the detector rules added on 2026-10-17 were written against dev misses from those templates, and the `decode … follow` rule matches the generator's own base64 wrapper sentence. Test recall is therefore not a held-out estimate for unseen phrasings. A later `injection.v2` should split by template and add out-of-template hard negatives (a review of the detector found ordinary notes such as "Note for the agent: …" flagged before the rules were narrowed). Families still missed on dev include exfiltration instructions that name an email address rather than a link, and forged chat transcripts ("assistant: … user: …") without other signals. Injection scoring is defence in depth; untrusted marking and policy are the controls.
- Redaction scores 1.0 recall and precision on both partitions. The cases are generated from a fixed set of formats (test card numbers, checksum-valid IBANs, fictional phone ranges), so real-world recall will be lower; names and street addresses are out of scope for the detectors.
- `chaos.v1` is produced by `make chaos` against the running stack, not by `make eval`.

## Chaos eval parameters

`make chaos` uses a 2 s gateway timeout, a 3 s stall on 10% of refund keys and a 5 s breaker cooldown so 10,000 calls finish in minutes (215 s on the last run, `evals/reports/chaos.v1.json`). Under those settings the run saw 1,620 upstream timeouts and 2,012 breaker rejections across 15,632 attempts; retries come both from those failures (3,632) and from 2,000 deliberate re-sends. The defaults (10 s timeout, 30 s cooldown) are not exercised by the chaos run.

## NFR-2 not met; NFR-7 not verified at 2,000 calls/s

NFR-2 asks for at least 2,000 calls/s on an 8-vCPU laptop, and NFR-7 for a gateway RSS under 128 MB at that rate. The development machine used for these measurements has 4 vCPUs (Intel Xeon @ 2.10 GHz) and runs the whole compose stack plus the load generator. On it, `make loadtest-profile` measured 630.0 calls/s with 0 errors and a gateway RSS of at most 93.9 MB (`docs/perf/loadtest.json`). That passes the CI budget (>= 500 calls/s, `scripts/bench_budget.py --ci`) but **fails NFR-2**, and NFR-7 holds only at 630 calls/s, so it is **not verified at 2,000 calls/s**.

The machine was CPU-bound: 3.52 of 4 cores busy over the measured window (`docs/perf/cpu.txt`, `scripts/cpu_usage.py`). Per component, in average cores (`docs/perf/cpu.txt`):

- gateway 1.45, which is about 2.3 ms of CPU per call (1.45 / 630);
- example upstreams 0.88 (orders 0.48, crm 0.38, ledger 0.02);
- load generator 0.37;
- Postgres 0.26;
- Docker's port proxy, dockerd and containerd 0.26;
- Redis 0.09.

The whole machine spent about 5.6 ms of CPU per call (3.52 / 630), or 5.0 ms without the load generator. At the same per-core speed, 8 vCPUs would therefore reach about 1,400 calls/s with everything on one host (8 / 5.6 ms), or about 1,600 calls/s with the load generator elsewhere (8 / 5.0 ms). That is a projection, not a measurement, and it means **more cores alone are not expected to meet NFR-2**: the gateway's ~2.3 ms and the example upstreams' CPU per call would have to drop too.

What the profiling changed:

- **Refunds index.** Per-order refund sums scanned `orders.refunds` sequentially. Migration 00006 adds an index, and `TestRefundTotalsUseIndex` guards it.
- **Injection scorer.** Literal prefilters and a hand-written tokenizer give the same results (`TestRulePrefiltersAreExact`, `TestTokensMatchRegexp`, `make eval` metrics unchanged) for less CPU. `BenchmarkScoreToolResult` now takes 0.23-0.24 ms per result with the stack running on the same machine (`docs/perf/scorer.txt`). The pre-change time was read by hand and is not kept, so no before/after ratio is claimed.

Where the gateway's remaining CPU goes was read from a pprof profile of a throwaway build that was not committed. The main costs were the scorer, OPA evaluation, HTTP and the MCP SDK, the upstream MCP client and GC; the split is not reproducible from the repository and is not quoted here.

Under CPU saturation the gateway's measured overhead (`mcpgw_overhead_seconds`) includes run-queue waiting, so the `GatewayOverheadSLO` alert can go pending during a load test on this machine. The in-process benchmark puts the gateway's own overhead at p95 0.398 ms (`docs/perf/bench.txt`, `make bench`).

Next steps: re-run `make loadtest-profile` on an 8-vCPU machine with the load generator on another host, then profile the gateway's per-call CPU against the remaining gap.
