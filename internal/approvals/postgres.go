package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Store (tables approvals and webhook_outbox).
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns the store.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

const approvalColumns = `id::text, tenant, agent, server, tool, args_sha256, args_redacted::text, reasons, COALESCE(idempotency_key, ''),
	status, expires_at, COALESCE(decided_by, ''), decided_at, COALESCE(decision_note, ''), consumed_at, created_at, updated_at`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var args string
	err := row.Scan(&a.ID, &a.Tenant, &a.Agent, &a.Server, &a.Tool, &a.ArgsSHA256, &args, &a.Reasons, &a.IdempotencyKey,
		&a.Status, &a.ExpiresAt, &a.DecidedBy, &a.DecidedAt, &a.DecisionNote, &a.ConsumedAt, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("scan approval: %w", err)
	}
	a.ArgsRedacted = json.RawMessage(args)
	a.ArgsSHA256 = trimHash(a.ArgsSHA256)
	return a, nil
}

func trimHash(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// Create implements Store.
func (p *Postgres) Create(ctx context.Context, a Approval, webhook json.RawMessage) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create approval: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var key any
	if a.IdempotencyKey != "" {
		key = a.IdempotencyKey
	}
	_, err = tx.Exec(ctx, `INSERT INTO approvals (id, tenant, agent, server, tool, args_sha256, args_redacted, reasons,
		idempotency_key, status, expires_at, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
		a.ID, a.Tenant, a.Agent, a.Server, a.Tool, a.ArgsSHA256, string(a.ArgsRedacted), a.Reasons, key, a.Status, a.ExpiresAt, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO webhook_outbox (approval_id, payload, next_attempt_at, created_at) VALUES ($1, $2, $3, $3)`,
		a.ID, string(webhook), a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create approval: commit: %w", err)
	}
	return nil
}

// Get implements Store.
func (p *Postgres) Get(ctx context.Context, id string) (Approval, error) {
	return scanApproval(p.pool.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id::text = $1`, id))
}

// FindPending implements Store.
func (p *Postgres) FindPending(ctx context.Context, b Binding, now time.Time) (Approval, bool, error) {
	a, err := scanApproval(p.pool.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE status = 'pending'
		AND tenant = $1 AND agent = $2 AND server = $3 AND tool = $4 AND args_sha256 = $5 AND expires_at > $6
		AND COALESCE(idempotency_key, '') = $7 ORDER BY created_at DESC LIMIT 1`,
		b.Tenant, b.Agent, b.Server, b.Tool, b.ArgsSHA256, now, b.IdempotencyKey))
	if errors.Is(err, ErrNotFound) {
		return Approval{}, false, nil
	}
	return a, err == nil, err
}

// List implements Store.
func (p *Postgres) List(ctx context.Context, f ListFilter) ([]Approval, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 200
	}
	rows, err := p.pool.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE ($1 = '' OR tenant = $1)
		AND ($2 = '' OR status = $2) ORDER BY created_at DESC LIMIT $3`, f.Tenant, f.Status, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Decide implements Store.
func (p *Postgres) Decide(ctx context.Context, id, tenant, status, by, note string, now time.Time) (Approval, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Approval{}, fmt.Errorf("decide: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id::text = $1 FOR UPDATE`, id))
	if err != nil {
		return Approval{}, err
	}
	switch {
	case tenant != "" && a.Tenant != tenant:
		return Approval{}, ErrForbidden
	case a.Status == StatusPending && !now.Before(a.ExpiresAt):
		if _, err := tx.Exec(ctx, `UPDATE approvals SET status = 'expired', updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
			return Approval{}, fmt.Errorf("expire approval: %w", err)
		}
		a.Status = StatusExpired
		if err := tx.Commit(ctx); err != nil {
			return Approval{}, fmt.Errorf("decide: commit: %w", err)
		}
		return a, ErrExpired
	case a.Status == StatusExpired:
		return a, ErrExpired
	case a.Status != StatusPending:
		return a, ErrNotPending
	}
	a, err = scanApproval(tx.QueryRow(ctx, `UPDATE approvals SET status = $2, decided_by = $3, decided_at = $4,
		decision_note = $5, updated_at = $4 WHERE id = $1 RETURNING `+approvalColumns, a.ID, status, by, now, note))
	if err != nil {
		return Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Approval{}, fmt.Errorf("decide: commit: %w", err)
	}
	return a, nil
}

// Consume implements Store: the row is locked, checked and marked consumed in one transaction,
// so two concurrent retries cannot both execute.
func (p *Postgres) Consume(ctx context.Context, id string, b Binding, now time.Time) (Approval, string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Approval{}, "", fmt.Errorf("consume: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id::text = $1 FOR UPDATE`, id))
	if errors.Is(err, ErrNotFound) {
		return Approval{}, ReasonNotFound, nil
	}
	if err != nil {
		return Approval{}, "", err
	}
	if r := consumeDecision(a, b, now); r != "" {
		if r == ReasonExpired && a.Status == StatusPending {
			_, _ = tx.Exec(ctx, `UPDATE approvals SET status = 'expired', updated_at = $2 WHERE id = $1`, a.ID, now)
			a.Status = StatusExpired
			_ = tx.Commit(ctx)
		}
		return a, r, nil
	}
	a, err = scanApproval(tx.QueryRow(ctx, `UPDATE approvals SET status = 'consumed', consumed_at = $2, updated_at = $2
		WHERE id = $1 RETURNING `+approvalColumns, a.ID, now))
	if err != nil {
		return Approval{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Approval{}, "", fmt.Errorf("consume: commit: %w", err)
	}
	return a, "", nil
}

// Expire implements Store.
func (p *Postgres) Expire(ctx context.Context, now time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE approvals SET status = 'expired', updated_at = $1 WHERE status = 'pending' AND expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("expire approvals: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountPending implements Store.
func (p *Postgres) CountPending(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE status = 'pending' AND expires_at > $1`, now).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// DueOutbox implements Store.
func (p *Postgres) DueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, approval_id::text, payload::text, attempts FROM webhook_outbox
		WHERE delivered_at IS NULL AND next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var o OutboxItem
		var payload string
		if err := rows.Scan(&o.ID, &o.ApprovalID, &payload, &o.Attempts); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		o.Payload = json.RawMessage(payload)
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkDelivered implements Store.
func (p *Postgres) MarkDelivered(ctx context.Context, id int64, now time.Time) error {
	if _, err := p.pool.Exec(ctx, `UPDATE webhook_outbox SET delivered_at = $2, attempts = attempts + 1, last_error = NULL WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	return nil
}

// MarkFailed implements Store.
func (p *Postgres) MarkFailed(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error {
	var nextArg any = next
	if next.IsZero() {
		nextArg = nil // abandoned
	}
	if _, err := p.pool.Exec(ctx, `UPDATE webhook_outbox SET attempts = $2,
		next_attempt_at = COALESCE($3::timestamptz, 'infinity'::timestamptz), last_error = $4 WHERE id = $1`,
		id, attempts, nextArg, lastErr); err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// Purge implements Store.
func (p *Postgres) Purge(ctx context.Context, before, outboxBefore time.Time) (int64, error) {
	var n int64
	for _, q := range []struct {
		sql string
		arg time.Time
	}{
		{`DELETE FROM webhook_outbox WHERE delivered_at IS NOT NULL AND delivered_at < $1`, outboxBefore},
		{`DELETE FROM approvals WHERE status <> 'pending' AND updated_at < $1`, before},
		{`DELETE FROM injection_findings WHERE ts < $1`, before},
	} {
		tag, err := p.pool.Exec(ctx, q.sql, q.arg)
		if err != nil {
			return n, fmt.Errorf("purge: %w", err)
		}
		n += tag.RowsAffected()
	}
	return n, nil
}
