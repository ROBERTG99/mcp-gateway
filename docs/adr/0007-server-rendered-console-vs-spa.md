# 0007. Server-rendered console instead of a single-page app

- Status: Accepted (2026-10-17)

## Context

Approvers and auditors need a small web console. It lists the audit log, shows approvals with exactly what will run, tests policy inputs and shows upstream health. The console shows text that came from upstream systems, some of it written by an attacker. It also holds the approve button, so a script injected into a page is a serious risk.

## Decision

Render the console on the server with Go `html/template` and send a strict Content Security Policy.

- The pages and templates live in `internal/console` (`console.go`, `templates/*.html`) and are embedded in the binary with `go:embed`. There is no Node build step at runtime.
- `html/template` escapes everything by context, including upstream text. The console marks upstream content as untrusted (`.claude/rules/frontend.md`).
- The CSP is `default-src 'none'; script-src 'self'; style-src 'self'; ...; frame-ancestors 'none'; base-uri 'none'` (`securityHeaders` in `internal/console/console.go`). No inline script is allowed, and `TestConsoleRequiresSession` checks `script-src 'self'`.
- The only script is optional: `internal/console/static/console.js` handles the theme toggle, a loading state and focus. The budget is 3 KB, enforced by `TestThemesAndStatic`. Every page works without it.
- Sessions are server-side, with an `HttpOnly`, `SameSite=Strict` cookie. Every POST carries a per-session CSRF token (`TestSessionCookieAttributes`, `TestCSRFRejected`).
- Pages need the same scopes as the admin API routes (`TestConsoleRequiresScope`).
- Playwright tests run axe on every page and fail on serious or critical findings (`e2e/tests/console.spec.ts`; `make e2e`).

## Alternatives considered

- **A single-page app (React or similar).** Rejected: it brings a build chain, a large dependency tree and client-side rendering of untrusted text. It would need a looser CSP or a nonce scheme, and it would need a JSON API session design with tokens in the browser. The console has a few table and form pages, so the extra weight buys little.
- **No console, API only.** Rejected: approvals and audit review would depend on `curl` and `api/openapi.yaml`, and approvers would have no page showing exactly what will run.

## Consequences

- Pages reload on each action. This is acceptable for the approval and audit workflows.
- Console sign-in is development-only. It uses dev issuer personas, and a production deployment needs an OIDC login in front (`docs/KNOWN_LIMITATIONS.md`).
- Sessions live in memory, so they do not survive a restart and are not shared between replicas (`docs/KNOWN_LIMITATIONS.md`).
- New interactivity must fit the script budget, or the budget decision must be revisited in a new ADR.
