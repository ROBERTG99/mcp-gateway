# Decisions

Decisions taken during the autonomous build where the specification was ambiguous or the environment forced a choice. Architectural decisions also have an ADR in `docs/adr/`.

| Date | Decision | Alternatives | Reason |
|---|---|---|---|
| 2026-10-16 | `.claude/settings.json` is kept at `docs/harness/settings.json` until the maintainer copies it into place. | Work around the denial; drop the file. | The Claude Code auto-mode classifier denies writes to the agent's own settings as self-modification. The content is complete and tested; only its location differs. Listed under "Needs the maintainer". |
| 2026-10-16 | Go module requires `go 1.25.0` with `toolchain go1.27.2`. | `go 1.27.0` as the language version. | 1.27.2 is the latest stable toolchain at build time; the language floor is the one required by `modelcontextprotocol/go-sdk` v1.8.0 so that linters built with older toolchains still parse the code. |
| 2026-10-16 | A policy bundle that fails to compile at startup denies every call and fails `/readyz` (FR-4); a failed hot reload keeps the last good bundle serving, `/readyz` stays 200 but reports `policy: stale: last reload failed: ...`, and the failure is logged (a metric and alert are planned in T-38). | Deny everything and fail readiness on any failed reload. | §4.1 requires that "a failed reload keeps the previous bundle and alerts"; failing readiness would remove every replica from service because of a typo in a not-yet-active bundle. FR-4's deny-all applies when no bundle has compiled. See ADR 0002. |
| 2026-10-16 | Idempotency records are bound to the tool as well as the args hash (`sha256(server.tool + NUL + args_sha256)`); `args_sha256` in the audit log stays the hash of the arguments only. | Bind only to args (contract text). | M2 review: the same key and args on another tool would otherwise replay the other tool's result. |
| 2026-10-16 | `arguments: null` or absent in tools/call is evaluated as `{}`; non-object arguments are rejected with -32602. | Pass null through. | M2 review: `args = null` is the tools/list signal, so argument rules (approval for credit notes, refund limits) would not fire. |
