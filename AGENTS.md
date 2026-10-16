# AGENTS.md

Instructions for any coding agent working on mcp-gateway. `CLAUDE.md` has the full map; these are the essentials.

- **What it is:** a Go MCP gateway (Streamable HTTP on :8210, admin and console on :8200) that governs LLM agents' tool calls with JWT auth, OPA policy, approvals, idempotency, redaction, rate limits, injection scoring and a hash-chained audit log. It calls no model.
- **Build and test:** `make check-fast` before every commit (gofmt, vet, golangci-lint, `go test -race -short`). `make test` for everything on the host (needs Docker). `make verify` is the single gate.
- **Test first:** write the failing test named in the spec, then implement. Never skip or weaken a test.
- **Conventions:** errors wrapped with `%w`, `context.Context` first, no globals, injectable clock, no sleeps in tests, no model provider SDKs.
- **Safety:** tenant comes from the token; redact before logging; fail closed; never edit `evals/datasets/test/`.
- **Commits:** Conventional Commits under the maintainer's identity. No `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" lines. Never change git identity, never force-push, never push tags.
- **Never:** `terraform apply/destroy`, cloud deploys, repository creation or visibility changes, messages to real recipients, secrets or real personal data in the repo.
- **Plan and progress:** `docs/specs/000-build-spec.md`, `PROGRESS.md`, `docs/DECISIONS.md`, `docs/adr/`, `docs/KNOWN_LIMITATIONS.md`.
