-- At most one pending approval per call (tenant, agent, tool, args hash, idempotency key), so two
-- concurrent identical held calls create one approval and send one webhook (M3 review).
-- +goose Up
CREATE UNIQUE INDEX approvals_one_pending ON approvals
  (tenant, agent, server, tool, args_sha256, COALESCE(idempotency_key, '')) WHERE status = 'pending';

-- +goose Down
DROP INDEX approvals_one_pending;
