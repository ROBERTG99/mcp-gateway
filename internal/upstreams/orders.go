package upstreams

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

type orderRef struct {
	OrderID string `json:"order_id" jsonschema:"order id, e.g. O-00001"`
}

type orderOut struct {
	OrderID       string    `json:"order_id"`
	CustomerID    string    `json:"customer_id"`
	AmountCents   int64     `json:"amount_cents"`
	RefundedCents int64     `json:"refunded_cents"`
	Currency      string    `json:"currency"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

type refundIn struct {
	OrderID     string `json:"order_id" jsonschema:"order id"`
	AmountCents int64  `json:"amount_cents" jsonschema:"refund amount in cents (> 0, at most the unrefunded amount)"`
	Reason      string `json:"reason" jsonschema:"refund reason"`
}

type refundOut struct {
	RefundID    int64  `json:"refund_id"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// RefundHook runs inside the refund transaction, after the idempotency key is claimed and before the
// order row is locked and the refund inserted (the chaos fault injector uses it).
type RefundHook func(ctx context.Context, tenant, key string) error

// OrdersOptions customise the orders upstream.
type OrdersOptions struct {
	BeforeRefund RefundHook
}

func registerOrders(s *mcpserver.Server, pool *pgxpool.Pool) {
	RegisterOrders(s, pool, OrdersOptions{})
}

// RegisterOrders registers the orders tools with options (used by cmd/upstream-orders for faults).
func RegisterOrders(s *mcpserver.Server, pool *pgxpool.Pool, opts OrdersOptions) {
	mcpserver.ReadTool(s, &mcp.Tool{Name: "get_order", Description: "Order detail with the refunded amount."},
		func(ctx context.Context, c mcpserver.CallInfo, in orderRef) (orderOut, error) {
			out := orderOut{OrderID: in.OrderID}
			err := pool.QueryRow(ctx, `SELECT o.customer_id, o.amount_cents, o.currency, o.status, o.created_at,
				COALESCE((SELECT sum(r.amount_cents) FROM orders.refunds r WHERE r.tenant = o.tenant AND r.order_id = o.order_id), 0)
				FROM orders.orders o WHERE o.tenant = $1 AND o.order_id = $2`, c.Tenant, in.OrderID).
				Scan(&out.CustomerID, &out.AmountCents, &out.Currency, &out.Status, &out.CreatedAt, &out.RefundedCents)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("order", in.OrderID)
			}
			if err != nil {
				return out, fmt.Errorf("get order: %w", err)
			}
			out.CreatedAt = out.CreatedAt.UTC()
			return out, nil
		})
	mcpserver.WriteTool(s, &mcp.Tool{Name: "refund_order", Description: "Refund part or all of an order."},
		func(ctx context.Context, w mcpserver.WriteContext, in refundIn) (refundOut, error) {
			out := refundOut{OrderID: in.OrderID, AmountCents: in.AmountCents}
			if in.AmountCents <= 0 {
				return out, errors.New("amount_cents must be positive")
			}
			if opts.BeforeRefund != nil {
				if err := opts.BeforeRefund(ctx, w.Tenant, w.IdempotencyKey); err != nil {
					return out, err
				}
			}
			if err := InsertRefund(ctx, w.Tx, w.Tenant, w.IdempotencyKey, in.OrderID, in.AmountCents, in.Reason, &out); err != nil {
				return out, err
			}
			return out, nil
		})
}

// InsertRefund validates and inserts a refund inside tx. The order row is locked in its own
// statement first, so the refunded sum (second statement) is read from a snapshot taken after the
// lock was granted: concurrent refunds of one order serialise and can never exceed its amount.
func InsertRefund(ctx context.Context, tx pgx.Tx, tenant, key, orderID string, amountCents int64, reason string, out *refundOut) error {
	var amount int64
	err := tx.QueryRow(ctx, `SELECT amount_cents, currency FROM orders.orders WHERE tenant = $1 AND order_id = $2 FOR UPDATE`,
		tenant, orderID).Scan(&amount, &out.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("order", orderID)
	}
	if err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	var refunded int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_cents), 0) FROM orders.refunds WHERE tenant = $1 AND order_id = $2`,
		tenant, orderID).Scan(&refunded); err != nil {
		return fmt.Errorf("sum refunds: %w", err)
	}
	if refunded+amountCents > amount {
		return fmt.Errorf("refund of %d exceeds the unrefunded amount %d", amountCents, amount-refunded)
	}
	err = tx.QueryRow(ctx, `INSERT INTO orders.refunds (tenant, order_id, amount_cents, currency, reason, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, tenant, orderID, amountCents, out.Currency, reason, key).Scan(&out.RefundID)
	if err != nil {
		return fmt.Errorf("insert refund: %w", err)
	}
	return nil
}
