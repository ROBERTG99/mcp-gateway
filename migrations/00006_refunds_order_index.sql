-- Refund totals per order (orders.get_order, and refund_order under the order lock) were summed
-- with a sequential scan of orders.refunds; at load-test rates that scan dominated Postgres CPU.
-- +goose Up
CREATE INDEX orders_refunds_order ON orders.refunds (tenant, order_id) INCLUDE (amount_cents);

-- +goose Down
DROP INDEX orders.orders_refunds_order;
