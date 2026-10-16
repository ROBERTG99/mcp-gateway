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

type customerRef struct {
	CustomerID string `json:"customer_id" jsonschema:"customer id, e.g. C-0042"`
}

type balanceOut struct {
	CustomerID   string `json:"customer_id"`
	BalanceCents int64  `json:"balance_cents"`
	PastDueCents int64  `json:"past_due_cents"`
	Currency     string `json:"currency"`
}

type promiseIn struct {
	CustomerID  string `json:"customer_id" jsonschema:"customer id"`
	AmountCents int64  `json:"amount_cents" jsonschema:"promised amount in cents (> 0)"`
	Currency    string `json:"currency" jsonschema:"ISO 4217 currency of the account"`
	DueDate     string `json:"due_date" jsonschema:"due date, YYYY-MM-DD"`
	Note        string `json:"note" jsonschema:"free-text note"`
}

type promiseOut struct {
	PromiseID   int64  `json:"promise_id"`
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	DueDate     string `json:"due_date"`
}

type creditIn struct {
	CustomerID  string `json:"customer_id" jsonschema:"customer id"`
	AmountCents int64  `json:"amount_cents" jsonschema:"credit amount in cents (> 0)"`
	Reason      string `json:"reason" jsonschema:"why the credit is issued"`
}

type creditOut struct {
	CreditNoteID int64  `json:"credit_note_id"`
	CustomerID   string `json:"customer_id"`
	AmountCents  int64  `json:"amount_cents"`
	Currency     string `json:"currency"`
}

func registerLedger(s *mcpserver.Server, pool *pgxpool.Pool) {
	mcpserver.ReadTool(s, &mcp.Tool{Name: "get_balance", Description: "Account balance and past-due amount of a customer."},
		func(ctx context.Context, c mcpserver.CallInfo, in customerRef) (balanceOut, error) {
			out := balanceOut{CustomerID: in.CustomerID}
			err := pool.QueryRow(ctx, `SELECT balance_cents, past_due_cents, currency FROM ledger.accounts
				WHERE tenant = $1 AND customer_id = $2`, c.Tenant, in.CustomerID).Scan(&out.BalanceCents, &out.PastDueCents, &out.Currency)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("customer", in.CustomerID)
			}
			if err != nil {
				return out, fmt.Errorf("get balance: %w", err)
			}
			return out, nil
		})
	mcpserver.WriteTool(s, &mcp.Tool{Name: "record_promise_to_pay", Description: "Record a customer's promise to pay an amount by a date."},
		func(ctx context.Context, w mcpserver.WriteContext, in promiseIn) (promiseOut, error) {
			out := promiseOut{CustomerID: in.CustomerID, AmountCents: in.AmountCents, Currency: in.Currency, DueDate: in.DueDate}
			if in.AmountCents <= 0 {
				return out, errors.New("amount_cents must be positive")
			}
			due, err := time.Parse(time.DateOnly, in.DueDate)
			if err != nil {
				return out, fmt.Errorf("due_date must be YYYY-MM-DD: %w", err)
			}
			var currency string
			err = w.Tx.QueryRow(ctx, `SELECT currency FROM ledger.accounts WHERE tenant = $1 AND customer_id = $2`,
				w.Tenant, in.CustomerID).Scan(&currency)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("customer", in.CustomerID)
			}
			if err != nil {
				return out, fmt.Errorf("lookup account: %w", err)
			}
			if currency != in.Currency {
				return out, fmt.Errorf("currency %s does not match the account currency %s", in.Currency, currency)
			}
			err = w.Tx.QueryRow(ctx, `INSERT INTO ledger.promises (tenant, customer_id, amount_cents, currency, due_date, note, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
				w.Tenant, in.CustomerID, in.AmountCents, in.Currency, due, in.Note, w.IdempotencyKey).Scan(&out.PromiseID)
			if err != nil {
				return out, fmt.Errorf("insert promise: %w", err)
			}
			return out, nil
		})
	mcpserver.WriteTool(s, &mcp.Tool{Name: "issue_credit_note", Description: "Issue a credit note to a customer's account."},
		func(ctx context.Context, w mcpserver.WriteContext, in creditIn) (creditOut, error) {
			out := creditOut{CustomerID: in.CustomerID, AmountCents: in.AmountCents}
			if in.AmountCents <= 0 {
				return out, errors.New("amount_cents must be positive")
			}
			err := w.Tx.QueryRow(ctx, `SELECT currency FROM ledger.accounts WHERE tenant = $1 AND customer_id = $2`,
				w.Tenant, in.CustomerID).Scan(&out.Currency)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("customer", in.CustomerID)
			}
			if err != nil {
				return out, fmt.Errorf("lookup account: %w", err)
			}
			err = w.Tx.QueryRow(ctx, `INSERT INTO ledger.credit_notes (tenant, customer_id, amount_cents, currency, reason, idempotency_key)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
				w.Tenant, in.CustomerID, in.AmountCents, out.Currency, in.Reason, w.IdempotencyKey).Scan(&out.CreditNoteID)
			if err != nil {
				return out, fmt.Errorf("insert credit note: %w", err)
			}
			return out, nil
		})
}
