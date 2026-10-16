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

// RefundHook runs inside the refund transaction after the insert (the chaos fault injector uses it).
type RefundHook func(ctx context.Context, tenant, key string) error

// OrdersOptions customise the orders upstream.
type OrdersOptions struct {
	AfterRefund RefundHook
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
			var amount, refunded int64
			// Lock the order row so concurrent refunds of one order cannot exceed its amount.
			err := w.Tx.QueryRow(ctx, `SELECT o.amount_cents, o.currency,
				COALESCE((SELECT sum(r.amount_cents) FROM orders.refunds r WHERE r.tenant = o.tenant AND r.order_id = o.order_id), 0)
				FROM orders.orders o WHERE o.tenant = $1 AND o.order_id = $2 FOR UPDATE`, w.Tenant, in.OrderID).
				Scan(&amount, &out.Currency, &refunded)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("order", in.OrderID)
			}
			if err != nil {
				return out, fmt.Errorf("lookup order: %w", err)
			}
			if refunded+in.AmountCents > amount {
				return out, fmt.Errorf("refund of %d exceeds the unrefunded amount %d", in.AmountCents, amount-refunded)
			}
			err = w.Tx.QueryRow(ctx, `INSERT INTO orders.refunds (tenant, order_id, amount_cents, currency, reason, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, w.Tenant, in.OrderID, in.AmountCents, out.Currency, in.Reason,
				w.IdempotencyKey).Scan(&out.RefundID)
			if err != nil {
				return out, fmt.Errorf("insert refund: %w", err)
			}
			if opts.AfterRefund != nil {
				if err := opts.AfterRefund(ctx, w.Tenant, w.IdempotencyKey); err != nil {
					return out, err
				}
			}
			return out, nil
		})
}
