-- Human approvals, webhook outbox and injection findings (spec §4.3).
-- +goose Up
CREATE TABLE approvals (
  id              uuid PRIMARY KEY,
  tenant          text NOT NULL,
  agent           text NOT NULL,
  server          text NOT NULL,
  tool            text NOT NULL,
  args_sha256     char(64) NOT NULL,
  args_redacted   jsonb NOT NULL,
  reasons         text[] NOT NULL DEFAULT '{}',
  idempotency_key text,
  status          text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'consumed')),
  expires_at      timestamptz NOT NULL,
  decided_by      text,
  decided_at      timestamptz,
  decision_note   text,
  consumed_at     timestamptz,
  created_at      timestamptz NOT NULL,
  updated_at      timestamptz NOT NULL
);
CREATE INDEX approvals_pending ON approvals (tenant, expires_at) WHERE status = 'pending';
CREATE INDEX approvals_tenant_created ON approvals (tenant, created_at DESC);

CREATE TABLE webhook_outbox (
  id              bigserial PRIMARY KEY,
  approval_id     uuid NOT NULL REFERENCES approvals (id) ON DELETE CASCADE,
  payload         jsonb NOT NULL,
  attempts        integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  delivered_at    timestamptz,
  last_error      text,
  created_at      timestamptz NOT NULL
);
CREATE INDEX webhook_outbox_undelivered ON webhook_outbox (next_attempt_at) WHERE delivered_at IS NULL;

CREATE TABLE injection_findings (
  id        bigserial PRIMARY KEY,
  audit_seq bigint,
  tenant    text NOT NULL,
  server    text NOT NULL,
  tool      text NOT NULL,
  score     real NOT NULL,
  signals   text[] NOT NULL,
  ts        timestamptz NOT NULL
);
CREATE INDEX injection_findings_tenant_ts ON injection_findings (tenant, ts DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON approvals, webhook_outbox, injection_findings TO mcpgw_app;
GRANT USAGE ON SEQUENCE webhook_outbox_id_seq, injection_findings_id_seq TO mcpgw_app;
GRANT SELECT ON approvals, webhook_outbox, injection_findings TO mcpgw_readonly;

-- +goose Down
DROP TABLE injection_findings;
DROP TABLE webhook_outbox;
DROP TABLE approvals;
