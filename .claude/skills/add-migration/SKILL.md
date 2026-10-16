---
name: add-migration
description: Add a goose SQL migration for Postgres with up and down sections, and test it.
---

# Add a migration

1. Create `migrations/NNNNN_description.sql` (next number) with `-- +goose Up` and `-- +goose Down` sections. Use `-- +goose StatementBegin/End` around functions and triggers.
2. Keep `mcpgw_app` grants least privilege: INSERT and SELECT only on `audit_events`.
3. Add or extend a test in `internal/store` (integration tag) that migrates up, checks the change, and migrates down cleanly.
4. `make integration` (or `go test -tags=integration ./internal/store/...`), then `make migrate` against the stack.
