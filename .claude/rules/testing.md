---
paths: ["**/*_test.go", "tests/**", "e2e/tests/**"]
---

# Testing rules

- Write the named failing test first, then implement (see the `tdd` skill).
- Never skip, disable, mark as expected-failure or weaken a test or threshold to get green. Fix the code or record the gap in `docs/KNOWN_LIMITATIONS.md`.
- No sleeps; inject `clock.Clock`. Fixed seeds for anything random. Flaky tests are fixed, never retried.
- Integration tests carry `//go:build integration` and use testcontainers-go, or the compose services when `MCPGW_TEST_USE_COMPOSE=1`.
- Assert on behaviour and contracts (codes, shapes, headers), not on log text.
