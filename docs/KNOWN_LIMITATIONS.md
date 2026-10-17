# Known limitations

What is not done, or done differently from the specification, and why. Each item names where it is tracked.

- `.claude/settings.json` lives at `docs/harness/settings.json` until the maintainer copies it (see `PROGRESS.md`, "Needs the maintainer").
- `scripts/install_tools.sh` downloads hadolint, trivy and promtool release binaries without SHA-256 verification. In Claude Code on the web the GitHub release host is blocked, so the script falls back to the official container images (hadolint, promtool) and `go install` (trivy), which are verified by the registry digest and the Go checksum database respectively. Pinning release checksums is a follow-up.
- The three example upstreams share one database role (`mcpgw_upstream`) with write access to all three schemas. A per-schema role per upstream would show least privilege better; the gateway's own role (`mcpgw_app`) is least privilege.
- Idempotency locks identify their holder by the (tool-bound) args hash, not a random owner token. If an upstream call outlives `MCPGW_IDEMPOTENCY_LOCK_TTL` (30 s, three times the upstream timeout), a second caller can acquire the key and the first caller's later release could drop the second caller's lock. Upstream-side dedup (`pkg/mcpserver`, unique `(tenant, idempotency_key)`) still prevents a duplicate write. Owner tokens are a follow-up.
- The 2026-07-28 stateless protocol has no `Mcp-Session-Id`; per-principal session binding (`TestSessionBoundToPrincipal`) applies to clients that negotiate the legacy session protocol.
- A call rejected because the audit buffer is saturated (`-32048`) is not itself audited (there is no slot to write it); it is logged and will be counted by a metric (T-38).
- Truncation of the newest audit rows by a superuser is not detectable by `audit verify` alone (no external anchor of the head hash); periodic export of the head hash is a runbook item.

## Console sign-in is development-only

The console signs in with the dev issuer's personas: there is no password and no external identity provider. Sign-in is mounted only when `MCPGW_DEV_ISSUER_URL` is set (compose sets it; the default is empty, which disables sign-in), and anyone who can reach port 8200 on such a stack can sign in as any persona. Cookies get the `Secure` flag only with `MCPGW_CONSOLE_SECURE_COOKIES=true` (default false for the local HTTP stack). Sessions live in memory (capped at 10,000), so they do not survive a restart and are not shared between replicas. A production deployment needs an OIDC login in front of the console; that is out of scope for v0.1.0.

## Eval results are on synthetic, template-built data

- Injection recall on the test partition is exactly at the gate: 0.8000 (95% CI 0.6765-0.9167) against 0.9167 on dev (`evals/REPORT.md`, `make eval`). The partitions are split per case, not per template: all 18 malicious templates in test also occur in dev with different fills, the detector rules added on 2026-10-17 were written against dev misses from those templates, and the `decode … follow` rule matches the generator's own base64 wrapper sentence. Test recall is therefore not a held-out estimate for unseen phrasings. A later `injection.v2` should split by template and add out-of-template hard negatives (the M5 review found ordinary notes such as "Note for the agent: …" flagged before the rules were narrowed). Families still missed on dev include exfiltration instructions that name an email address rather than a link, and forged chat transcripts ("assistant: … user: …") without other signals. Injection scoring is defence in depth; untrusted marking and policy are the controls.
- Redaction scores 1.0 recall and precision on both partitions. The cases are generated from a fixed set of formats (test card numbers, checksum-valid IBANs, fictional phone ranges), so real-world recall will be lower; names and street addresses are out of scope for the detectors.
- `chaos.v1` is produced by `make chaos` against the running stack (T-35), not by `make eval`.

## Chaos eval parameters

`make chaos` uses a 2 s gateway timeout, a 3 s stall on 10% of refund keys and a 5 s breaker cooldown so 10,000 calls finish in minutes (215 s on the last run, `evals/reports/chaos.v1.json`). Under those settings the run saw 1,620 upstream timeouts and 2,012 breaker rejections across 15,632 attempts; retries come both from those failures (3,632) and from 2,000 deliberate re-sends. The defaults (10 s timeout, 30 s cooldown) are not exercised by the chaos run.

## Throughput below NFR-2 on the build machine

NFR-2 asks for at least 2,000 calls/s on an 8-vCPU laptop. The only machine available to the build has 4 vCPUs and runs the whole stack plus the load generator. On it, `make loadtest` reached 642.8 calls/s with 0 errors and a gateway RSS of at most 87.3 MB (`docs/perf/loadtest.json`). That passes the CI budget (>= 500 calls/s, `scripts/bench_budget.py --ci`) and NFR-7, but not NFR-2.

The machine is CPU-bound during the run. Per-process CPU during a host-run profile at 699 calls/s:

- gateway: 1.44 cores, about 2 ms of CPU per call;
- example upstreams: 0.83 cores;
- load generator: 0.37 cores;
- Docker's port proxy: 0.35 cores;
- Postgres: 0.31 cores.

Without the gateway, everything else already costs about 2.9 ms of CPU per call (2.04 cores at 699 calls/s, Redis and kernel workers included). That caps this machine near 1,400 calls/s whatever the gateway does. Two fixes came out of the profiling:

- a missing index on refunds (migration 00006): Postgres used 1.1 cores before it;
- literal prefilters and a hand-written tokenizer in the injection scorer: the same results, about 4 times less CPU (`BenchmarkScoreToolResult`, 0.77-0.82 ms to 0.18-0.19 ms per result).

The remaining gateway cost is spread across:

- the injection scorer (14%);
- OPA evaluation (14%);
- HTTP and the MCP SDK;
- the upstream MCP client;
- GC;
- JWT verification.

Under CPU saturation, the gateway's measured overhead (`mcpgw_overhead_seconds`) includes run-queue waiting, so the `GatewayOverheadSLO` alert can go pending during a load test on this machine. The in-process benchmark puts the gateway's own overhead at p95 0.409 ms (`docs/perf/bench.txt`). NFR-7's "at 2,000 calls/s" was therefore measured at 642.8 calls/s. NFR-2 needs a re-run on an 8-vCPU machine, ideally with the load generator on a separate host. This is listed under "Needs the maintainer".
