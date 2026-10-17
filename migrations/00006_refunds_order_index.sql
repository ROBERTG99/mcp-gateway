-- Refund totals per order (orders.get_order, and refund_order under the order lock) were summed
-- with a sequential scan of orders.refunds; at load-test rates that scan dominated Postgres CPU.
-- A plain CREATE INDEX blocks writes to refunds while it builds, which is fine for the example
-- upstreams; on a large real table build it CONCURRENTLY in a goose migration marked NO TRANSACTION.
-- +goose Up
CREATE INDEX orders_refunds_order ON orders.refunds (tenant, order_id) INCLUDE (amount_cents);

-- +goose Down
DROP INDEX orders.orders_refunds_order;
