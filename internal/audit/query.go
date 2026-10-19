package audit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Filter selects audit events (newest first, keyset pagination on chain_pos).
type Filter struct {
	Tenant     string // "" = all tenants (only for cross-tenant viewers)
	Agent      string
	Tool       string // "server.tool" or tool name
	Status     string
	ApprovalID string
	From, To   time.Time // inclusive bounds on ts (zero = open)
	Cursor     string
	Limit      int // 1..200 (default 50)
}

// Page is one page of events.
type Page struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// Reader queries and verifies the audit log.
type Reader interface {
	List(ctx context.Context, f Filter) (Page, error)
	Get(ctx context.Context, seq int64) (Event, error)
	Verify(ctx context.Context, fromSeq int64) (VerifyResult, error)
}

// ErrBadCursor is returned for malformed cursors.
var ErrBadCursor = fmt.Errorf("invalid cursor")

// ErrEventNotFound is returned by Get.
var ErrEventNotFound = fmt.Errorf("audit event not found")

// The cursor is the chain position of the last event on the page; the next page holds the
// events strictly before it. Every positive integer is a valid cursor.
func encodeCursor(pos int64) string { return strconv.FormatInt(pos, 10) }

func decodeCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	pos, err := strconv.ParseInt(c, 10, 64)
	if err != nil || pos <= 0 {
		return 0, ErrBadCursor
	}
	return pos, nil
}

func (f *Filter) normalize() {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
}

func splitTool(t string) (server, tool string) {
	if s, n, ok := strings.Cut(t, "."); ok {
		return s, n
	}
	return "", t
}

// validUUID reports whether s has the canonical 8-4-4-4-12 hex shape.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// PostgresReader reads audit_events.
type PostgresReader struct{ pool *pgxpool.Pool }

// NewPostgresReader returns the reader.
func NewPostgresReader(pool *pgxpool.Pool) *PostgresReader { return &PostgresReader{pool: pool} }

// List implements Reader.
func (r *PostgresReader) List(ctx context.Context, f Filter) (Page, error) {
	f.normalize()
	before, err := decodeCursor(f.Cursor)
	if err != nil {
		return Page{}, err
	}
	server, tool := splitTool(f.Tool)
	// Only the filters in use reach the WHERE clause, so the planner can use the tenant and
	// approval indexes instead of evaluating "($n = '' OR col = $n)" for every row.
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	for _, c := range []struct{ col, v string }{
		{"tenant", f.Tenant}, {"agent", f.Agent}, {"server", server}, {"tool", tool}, {"status", f.Status},
	} {
		if c.v != "" {
			add(c.col+" = $%d", c.v)
		}
	}
	if f.ApprovalID != "" {
		if !validUUID(f.ApprovalID) {
			return Page{Events: []Event{}}, nil // no event can carry a malformed approval id
		}
		add("approval_id = $%d::uuid", f.ApprovalID)
	}
	if !f.From.IsZero() {
		add("ts >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("ts <= $%d", f.To)
	}
	if before > 0 {
		add("chain_pos < $%d", before)
	}
	q := selectEvents
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, f.Limit+1)
	q += fmt.Sprintf(" ORDER BY chain_pos DESC LIMIT $%d", len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	page := Page{Events: []Event{}}
	for rows.Next() {
		e, err := ScanEvent(rows)
		if err != nil {
			return Page{}, err
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list audit: %w", err)
	}
	if len(page.Events) > f.Limit {
		page.Events = page.Events[:f.Limit]
		page.NextCursor = encodeCursor(page.Events[f.Limit-1].ChainPos)
	}
	return page, nil
}

// Get implements Reader.
func (r *PostgresReader) Get(ctx context.Context, seq int64) (Event, error) {
	e, err := ScanEvent(r.pool.QueryRow(ctx, selectEvents+" WHERE seq = $1", seq))
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return Event{}, ErrEventNotFound
	}
	return e, err
}

// Verify implements Reader.
func (r *PostgresReader) Verify(ctx context.Context, fromSeq int64) (VerifyResult, error) {
	return VerifyPostgres(ctx, r.pool, fromSeq)
}

// MemoryReader reads a MemorySink (tests).
type MemoryReader struct{ Sink *MemorySink }

// List implements Reader.
func (m MemoryReader) List(_ context.Context, f Filter) (Page, error) {
	f.normalize()
	before, err := decodeCursor(f.Cursor)
	if err != nil {
		return Page{}, err
	}
	server, tool := splitTool(f.Tool)
	events := m.Sink.Events()
	page := Page{Events: []Event{}}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if (before != 0 && e.ChainPos >= before) || (f.Tenant != "" && e.Tenant != f.Tenant) || (f.Agent != "" && e.Agent != f.Agent) ||
			(server != "" && e.Server != server) || (tool != "" && e.Tool != tool) || (f.Status != "" && e.Status != f.Status) ||
			(f.ApprovalID != "" && (e.ApprovalID == nil || *e.ApprovalID != f.ApprovalID)) ||
			(!f.From.IsZero() && e.TS.Before(f.From)) || (!f.To.IsZero() && e.TS.After(f.To)) {
			continue
		}
		if len(page.Events) == f.Limit {
			page.NextCursor = encodeCursor(page.Events[f.Limit-1].ChainPos)
			break
		}
		page.Events = append(page.Events, e)
	}
	return page, nil
}

// Get implements Reader.
func (m MemoryReader) Get(_ context.Context, seq int64) (Event, error) {
	for _, e := range m.Sink.Events() {
		if e.Seq == seq {
			return e, nil
		}
	}
	return Event{}, ErrEventNotFound
}

// Verify implements Reader.
func (m MemoryReader) Verify(_ context.Context, fromSeq int64) (VerifyResult, error) {
	events := m.Sink.Events()
	if fromSeq == 0 {
		return VerifyChain(events, GenesisHash), nil
	}
	for i, e := range events {
		if e.Seq == fromSeq {
			return VerifyChain(events[i:], ""), nil
		}
	}
	return VerifyResult{}, ErrEventNotFound
}
