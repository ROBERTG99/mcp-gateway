# Decisions

Decisions taken during the autonomous build where the specification was ambiguous or the environment forced a choice. Architectural decisions also have an ADR in `docs/adr/`.

| Date | Decision | Alternatives | Reason |
|---|---|---|---|
| 2026-10-16 | `.claude/settings.json` is kept at `docs/harness/settings.json` until the maintainer copies it into place. | Work around the denial; drop the file. | The Claude Code auto-mode classifier denies writes to the agent's own settings as self-modification. The content is complete and tested; only its location differs. Listed under "Needs the maintainer". |
| 2026-10-16 | Go module requires `go 1.25.0` with `toolchain go1.27.2`. | `go 1.27.0` as the language version. | 1.27.2 is the latest stable toolchain at build time; the language floor is the one required by `modelcontextprotocol/go-sdk` v1.8.0 so that linters built with older toolchains still parse the code. |
