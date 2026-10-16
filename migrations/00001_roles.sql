-- Roles are cluster-wide: created idempotently, never dropped by a down migration.
-- Passwords are set by `mcp-gateway migrate` from MCPGW_*_DB_PASSWORD, never stored here.
-- +goose Up
-- +goose StatementBegin
DO $roles$
DECLARE r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['mcpgw_app', 'mcpgw_readonly', 'mcpgw_upstream'] LOOP
    BEGIN
      EXECUTE format('CREATE ROLE %I LOGIN', r);
    EXCEPTION WHEN duplicate_object OR unique_violation THEN
      NULL; -- already exists (possibly created concurrently by another database's migration)
    END;
  END LOOP;
END
$roles$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $grant$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO mcpgw_app, mcpgw_readonly, mcpgw_upstream', current_database()); END $grant$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $revoke$ BEGIN EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM mcpgw_app, mcpgw_readonly, mcpgw_upstream', current_database()); END $revoke$;
-- +goose StatementEnd
