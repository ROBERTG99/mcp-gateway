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
	var from, to any
	if !f.From.IsZero() {
		from = f.From
	}
	if !f.To.IsZero() {
		to = f.To
	}
	rows, err := r.pool.Query(ctx, selectEvents+` WHERE ($1 = '' OR tenant = $1) AND ($2 = '' OR agent = $2)
		AND ($3 = '' OR server = $3) AND ($4 = '' OR tool = $4) AND ($5 = '' OR status = $5)
		AND ($6 = '' OR approval_id::text = $6) AND ($7::timestamptz IS NULL OR ts >= $7)
		AND ($8::timestamptz IS NULL OR ts <= $8) AND ($9::bigint = 0 OR chain_pos < $9::bigint)
		ORDER BY chain_pos DESC LIMIT $10::int`,
		f.Tenant, f.Agent, server, tool, f.Status, f.ApprovalID, from, to, before, f.Limit+1)
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
