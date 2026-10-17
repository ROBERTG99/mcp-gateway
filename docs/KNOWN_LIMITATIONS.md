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

- Injection recall on the test partition is exactly at the gate: 0.8000 (95% CI 0.6765-0.9167) against 0.9167 on dev (`evals/REPORT.md`, `make eval`). Families still missed on dev include exfiltration instructions that name an email address rather than a link, and forged chat transcripts ("assistant: … user: …") without other signals. Injection scoring is defence in depth; untrusted marking and policy are the controls.
- Redaction scores 1.0 recall and precision on both partitions. The cases are generated from a fixed set of formats (test card numbers, checksum-valid IBANs, fictional phone ranges), so real-world recall will be lower; names and street addresses are out of scope for the detectors.
- `chaos.v1` is produced by `make chaos` against the running stack (T-35), not by `make eval`.

## Chaos eval parameters

`make chaos` uses a 2 s gateway timeout, a 3 s stall on 10% of refund keys and a 5 s breaker cooldown so 10,000 calls finish in minutes (215 s on the last run, `evals/reports/chaos.v1.json`). Under those settings the run saw 1,620 upstream timeouts and 2,012 breaker rejections across 15,632 attempts; retries come both from those failures (3,632) and from 2,000 deliberate re-sends. The defaults (10 s timeout, 30 s cooldown) are not exercised by the chaos run.
