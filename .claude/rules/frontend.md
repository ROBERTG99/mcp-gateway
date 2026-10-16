---
paths: ["internal/console/**", "e2e/**"]
---

# Frontend rules

- Server-rendered `html/template` only; at most 3 KB of optional JavaScript; the console works without it.
- Colours and fonts come from `internal/console/static/tokens.css` (light and dark). Status is always text plus colour.
- Every page has loading (`aria-busy`), empty, error (banner with retry) and success (`role="status"`) states.
- WCAG 2.2 AA: labels on every input, visible focus, keyboard-only approval, 375/768/1280 px layouts.
- Every Playwright test runs axe and fails on serious or critical violations.
- Upstream content is shown with the "untrusted output" chip and is always escaped.
