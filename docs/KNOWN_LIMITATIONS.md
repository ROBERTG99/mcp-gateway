# Known limitations

What is not done, or done differently from the specification, and why. Each item names where it is tracked.

- `.claude/settings.json` lives at `docs/harness/settings.json` until the maintainer copies it (see `PROGRESS.md`, "Needs the maintainer").
- `scripts/install_tools.sh` downloads hadolint, trivy and promtool release binaries without SHA-256 verification. In Claude Code on the web the GitHub release host is blocked, so the script falls back to the official container images (hadolint, promtool) and `go install` (trivy), which are verified by the registry digest and the Go checksum database respectively. Pinning release checksums is a follow-up.
- The three example upstreams share one database role (`mcpgw_upstream`) with write access to all three schemas. A per-schema role per upstream would show least privilege better; the gateway's own role (`mcpgw_app`) is least privilege.
- Idempotency locks identify their holder by the (tool-bound) args hash, not a random owner token. If an upstream call outlives `MCPGW_IDEMPOTENCY_LOCK_TTL` (30 s, three times the upstream timeout), a second caller can acquire the key and the first caller's later release could drop the second caller's lock. Upstream-side dedup (`pkg/mcpserver`, unique `(tenant, idempotency_key)`) still prevents a duplicate write. Owner tokens are a follow-up.
- The 2026-07-28 stateless protocol has no `Mcp-Session-Id`; per-principal session binding (`TestSessionBoundToPrincipal`) applies to clients that negotiate the legacy session protocol.
- A call rejected because the audit buffer is saturated (`-32048`) is not itself audited (there is no slot to write it); it is logged and will be counted by a metric (T-38).
- Truncation of the newest audit rows by a superuser is not detectable by `audit verify` alone (no external anchor of the head hash); periodic export of the head hash is a runbook item.
