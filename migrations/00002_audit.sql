-- Tamper-evident audit log (spec §4.3, §4.4). seq is allocated by the gateway from
-- audit_events_seq; chain_pos is the position in the hash chain, assigned by the audit writer
-- under a transaction advisory lock so that every replica extends one chain.
-- +goose Up
CREATE SEQUENCE audit_events_seq AS bigint START 1;

CREATE TABLE audit_events (
  seq             bigint PRIMARY KEY,
  chain_pos       bigint NOT NULL UNIQUE,
  ts              timestamptz NOT NULL,
  tenant          text NOT NULL,
  agent           text NOT NULL,
  server          text NOT NULL,
  tool            text NOT NULL,
  args_redacted   jsonb,
  args_sha256     char(64) NOT NULL,
  decision        text NOT NULL CHECK (decision IN ('allow', 'require_approval', 'deny', 'none')),
  approval_id     uuid,
  idempotency_key text,
  status          text NOT NULL CHECK (status IN ('ok', 'replayed', 'held', 'denied', 'rejected', 'error')),
  latency_ms      double precision NOT NULL,
  prev_hash       char(64) NOT NULL,
  hash            char(64) NOT NULL UNIQUE
);
CREATE INDEX audit_events_tenant_ts ON audit_events (tenant, ts DESC);
CREATE INDEX audit_events_tenant_tool_ts ON audit_events (tenant, tool, ts DESC);
CREATE INDEX audit_events_approval ON audit_events (approval_id) WHERE approval_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER audit_events_no_update_delete BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
  FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();

GRANT SELECT, INSERT ON audit_events TO mcpgw_app;
GRANT USAGE ON SEQUENCE audit_events_seq TO mcpgw_app;
GRANT SELECT ON audit_events TO mcpgw_readonly;

-- +goose Down
DROP TABLE audit_events;
DROP FUNCTION audit_events_append_only();
DROP SEQUENCE audit_events_seq;
