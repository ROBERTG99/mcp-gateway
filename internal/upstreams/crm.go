package upstreams

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

type noteOut struct {
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

type customerOut struct {
	CustomerID string    `json:"customer_id"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Phone      string    `json:"phone"`
	Segment    string    `json:"segment"`
	Notes      []noteOut `json:"notes"`
}

type addNoteIn struct {
	CustomerID string `json:"customer_id" jsonschema:"customer id"`
	Text       string `json:"text" jsonschema:"note text (1-4000 characters)"`
}

type addNoteOut struct {
	NoteID     int64  `json:"note_id"`
	CustomerID string `json:"customer_id"`
}

func registerCRM(s *mcpserver.Server, pool *pgxpool.Pool) {
	mcpserver.ReadTool(s, &mcp.Tool{Name: "get_customer", Description: "Customer profile with the last 5 notes."},
		func(ctx context.Context, c mcpserver.CallInfo, in customerRef) (customerOut, error) {
			out := customerOut{CustomerID: in.CustomerID, Notes: []noteOut{}}
			err := pool.QueryRow(ctx, `SELECT name, email, phone, segment FROM crm.customers WHERE tenant = $1 AND customer_id = $2`,
				c.Tenant, in.CustomerID).Scan(&out.Name, &out.Email, &out.Phone, &out.Segment)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, notFound("customer", in.CustomerID)
			}
			if err != nil {
				return out, fmt.Errorf("get customer: %w", err)
			}
			rows, err := pool.Query(ctx, `SELECT body, author, created_at FROM crm.notes WHERE tenant = $1 AND customer_id = $2
				ORDER BY created_at DESC, id DESC LIMIT 5`, c.Tenant, in.CustomerID)
			if err != nil {
				return out, fmt.Errorf("get notes: %w", err)
			}
			notes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (noteOut, error) {
				var n noteOut
				err := r.Scan(&n.Body, &n.Author, &n.CreatedAt)
				n.CreatedAt = n.CreatedAt.UTC()
				return n, err
			})
			if err != nil {
				return out, fmt.Errorf("scan notes: %w", err)
			}
			out.Notes = append(out.Notes, notes...)
			return out, nil
		})
	mcpserver.WriteTool(s, &mcp.Tool{Name: "add_note", Description: "Append a note to a customer."},
		func(ctx context.Context, w mcpserver.WriteContext, in addNoteIn) (addNoteOut, error) {
			out := addNoteOut{CustomerID: in.CustomerID}
			text := strings.TrimSpace(in.Text)
			if text == "" || utf8.RuneCountInString(text) > 4000 {
				return out, errors.New("text must be 1-4000 characters")
			}
			var exists bool
			if err := w.Tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM crm.customers WHERE tenant = $1 AND customer_id = $2)`,
				w.Tenant, in.CustomerID).Scan(&exists); err != nil {
				return out, fmt.Errorf("lookup customer: %w", err)
			}
			if !exists {
				return out, notFound("customer", in.CustomerID)
			}
			err := w.Tx.QueryRow(ctx, `INSERT INTO crm.notes (tenant, customer_id, body, author, idempotency_key)
				VALUES ($1, $2, $3, $4, $5) RETURNING id`, w.Tenant, in.CustomerID, text, w.Agent, w.IdempotencyKey).Scan(&out.NoteID)
			if err != nil {
				return out, fmt.Errorf("insert note: %w", err)
			}
			return out, nil
		})
}
