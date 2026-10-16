# Known limitations

What is not done, or done differently from the specification, and why. Each item names where it is tracked.

- `.claude/settings.json` lives at `docs/harness/settings.json` until the maintainer copies it (see `PROGRESS.md`, "Needs the maintainer").
- `scripts/install_tools.sh` downloads hadolint, trivy and promtool release binaries without SHA-256 verification. In Claude Code on the web the GitHub release host is blocked, so the script falls back to the official container images (hadolint, promtool) and `go install` (trivy), which are verified by the registry digest and the Go checksum database respectively. Pinning release checksums is a follow-up.
- The three example upstreams share one database role (`mcpgw_upstream`) with write access to all three schemas. A per-schema role per upstream would show least privilege better; the gateway's own role (`mcpgw_app`) is least privilege.
