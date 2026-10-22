---
paths: ["**/*.go", "go.mod", "go.sum"]
---

# Go rules

- Wrap errors with `fmt.Errorf("doing x: %w", err)`; compare with `errors.Is`/`errors.As`.
- `context.Context` is the first parameter of anything that does I/O or can block.
- No package-level mutable state (globals). Pass dependencies through constructors; the clock comes from `internal/clock`.
- Code must be `go test -race` clean. No `time.Sleep` in tests: use the fake clock, channels or polling with a deadline.
- Table-driven tests; descriptive test names (`TestProxyForwardsCall`, ...).
- Imports grouped stdlib / third party / `github.com/ROBERTG99/mcp-gateway/...` (goimports `-local`).
- Never import a model provider SDK (depguard enforces this).
- Log with `log/slog` and pass values through `internal/redact` first when they may hold user data.
