-- Example upstream systems (ledger, crm, orders). Every row carries its tenant; amounts are
-- bigint cents with an ISO 4217 currency. Write tables are unique on (tenant, idempotency_key)
-- and each schema has an mcp_idempotency table for upstream-side dedup (pkg/mcpserver).
-- +goose Up
CREATE SCHEMA ledger;
CREATE SCHEMA crm;
CREATE SCHEMA orders;

CREATE TABLE ledger.accounts (
  tenant         text NOT NULL,
  customer_id    text NOT NULL,
  balance_cents  bigint NOT NULL,
  past_due_cents bigint NOT NULL,
  currency       char(3) NOT NULL,
  PRIMARY KEY (tenant, customer_id)
);
CREATE TABLE ledger.promises (
  id              bigserial PRIMARY KEY,
  tenant          text NOT NULL,
  customer_id     text NOT NULL,
  amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
  currency        char(3) NOT NULL,
  due_date        date NOT NULL,
  note            text NOT NULL DEFAULT '',
  idempotency_key text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, idempotency_key)
);
CREATE TABLE ledger.credit_notes (
  id              bigserial PRIMARY KEY,
  tenant          text NOT NULL,
  customer_id     text NOT NULL,
  amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
  currency        char(3) NOT NULL,
  reason          text NOT NULL,
  idempotency_key text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, idempotency_key)
);

CREATE TABLE crm.customers (
  tenant      text NOT NULL,
  customer_id text NOT NULL,
  name        text NOT NULL,
  email       text NOT NULL,
  phone       text NOT NULL,
  segment     text NOT NULL,
  PRIMARY KEY (tenant, customer_id)
);
CREATE TABLE crm.notes (
  id              bigserial PRIMARY KEY,
  tenant          text NOT NULL,
  customer_id     text NOT NULL,
  body            text NOT NULL,
  author          text NOT NULL,
  idempotency_key text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, idempotency_key)
);
CREATE INDEX crm_notes_customer ON crm.notes (tenant, customer_id, created_at DESC);

CREATE TABLE orders.orders (
  tenant       text NOT NULL,
  order_id     text NOT NULL,
  customer_id  text NOT NULL,
  amount_cents bigint NOT NULL,
  currency     char(3) NOT NULL,
  status       text NOT NULL,
  created_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant, order_id)
);
CREATE TABLE orders.refunds (
  id              bigserial PRIMARY KEY,
  tenant          text NOT NULL,
  order_id        text NOT NULL,
  amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
  currency        char(3) NOT NULL,
  reason          text NOT NULL,
  idempotency_key text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, idempotency_key)
);

CREATE TABLE ledger.mcp_idempotency (tenant text, key text, tool text NOT NULL, args_sha256 char(64) NOT NULL,
  result jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant, key));
CREATE TABLE crm.mcp_idempotency (tenant text, key text, tool text NOT NULL, args_sha256 char(64) NOT NULL,
  result jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant, key));
CREATE TABLE orders.mcp_idempotency (tenant text, key text, tool text NOT NULL, args_sha256 char(64) NOT NULL,
  result jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (tenant, key));

GRANT USAGE ON SCHEMA ledger, crm, orders TO mcpgw_upstream, mcpgw_readonly;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ledger, crm, orders TO mcpgw_upstream;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA ledger, crm, orders TO mcpgw_upstream;
GRANT SELECT ON ALL TABLES IN SCHEMA ledger, crm, orders TO mcpgw_readonly;

-- +goose Down
DROP SCHEMA orders CASCADE;
DROP SCHEMA crm CASCADE;
DROP SCHEMA ledger CASCADE;
