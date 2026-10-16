package datagen

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Counts are the row counts of the seeded upstream tables.
type Counts struct {
	Accounts, Customers, Orders, Notes int64
}

// Seed writes the dataset into the ledger, crm and orders schemas. It is idempotent: rows are
// keyed by (tenant, id) or (tenant, idempotency_key) and existing rows are left untouched, so
// running it twice yields the same counts.
func Seed(ctx context.Context, pool *pgxpool.Pool, d Dataset) error {
	batch := &pgx.Batch{}
	for _, c := range d.Customers {
		batch.Queue(`INSERT INTO crm.customers (tenant, customer_id, name, email, phone, segment)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			c.Tenant, c.CustomerID, c.Name, c.Email, c.Phone, c.Segment)
		batch.Queue(`INSERT INTO ledger.accounts (tenant, customer_id, balance_cents, past_due_cents, currency)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
			c.Tenant, c.CustomerID, c.BalanceCents, c.PastDueCents, c.Currency)
	}
	for _, o := range d.Orders {
		batch.Queue(`INSERT INTO orders.orders (tenant, order_id, customer_id, amount_cents, currency, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			o.Tenant, o.OrderID, o.CustomerID, o.AmountCents, o.Currency, o.Status, o.CreatedAt)
	}
	for _, n := range d.Notes {
		batch.Queue(`INSERT INTO crm.notes (tenant, customer_id, body, author, idempotency_key, created_at)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			n.Tenant, n.CustomerID, n.Body, n.Author, n.Key, n.CreatedAt)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("seed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("seed: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed: commit: %w", err)
	}
	return nil
}

// CountRows returns the seeded table counts.
func CountRows(ctx context.Context, pool *pgxpool.Pool) (Counts, error) {
	var c Counts
	err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger.accounts), (SELECT count(*) FROM crm.customers),
		(SELECT count(*) FROM orders.orders), (SELECT count(*) FROM crm.notes)`).
		Scan(&c.Accounts, &c.Customers, &c.Orders, &c.Notes)
	if err != nil {
		return Counts{}, fmt.Errorf("count rows: %w", err)
	}
	return c, nil
}
