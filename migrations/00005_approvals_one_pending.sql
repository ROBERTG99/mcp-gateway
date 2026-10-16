-- At most one pending approval per call (tenant, agent, tool, args hash, idempotency key), so two
-- concurrent identical held calls create one approval and send one webhook (M3 review).
-- +goose Up
-- Databases from before this migration may hold duplicate pending rows: keep the newest pending
-- approval per call and expire the others.
UPDATE approvals a SET status = 'expired', updated_at = now()
WHERE a.status = 'pending' AND EXISTS (
  SELECT 1 FROM approvals b
  WHERE b.status = 'pending' AND b.tenant = a.tenant AND b.agent = a.agent AND b.server = a.server
    AND b.tool = a.tool AND b.args_sha256 = a.args_sha256
    AND COALESCE(b.idempotency_key, '') = COALESCE(a.idempotency_key, '')
    AND (b.created_at, b.id) > (a.created_at, a.id));
CREATE UNIQUE INDEX approvals_one_pending ON approvals
  (tenant, agent, server, tool, args_sha256, COALESCE(idempotency_key, '')) WHERE status = 'pending';

-- +goose Down
DROP INDEX approvals_one_pending;
