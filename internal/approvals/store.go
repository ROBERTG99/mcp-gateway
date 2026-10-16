// Package approvals holds calls that need a human decision. A held call creates a pending
// approval and, in the same transaction, a webhook outbox row; an approver (scope mcpgw:approve,
// same tenant) approves or rejects it; the agent retries with _meta.approval_id and the same
// idempotency key, and the approval is consumed exactly once if it is bound to that exact call.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
	StatusConsumed = "consumed"
)

// Errors map to the admin API's status codes (403, 404, 409, 410) and to -32045 reasons.
var (
	ErrNotFound   = errors.New("approval not found")
	ErrForbidden  = errors.New("approval belongs to another tenant")
	ErrNotPending = errors.New("approval is not pending")
	ErrExpired    = errors.New("approval expired")
)

// Approval is one held call.
type Approval struct {
	ID             string          `json:"id"`
	Tenant         string          `json:"tenant"`
	Agent          string          `json:"agent"`
	Server         string          `json:"server"`
	Tool           string          `json:"tool"`
	ArgsSHA256     string          `json:"args_sha256"`
	ArgsRedacted   json.RawMessage `json:"args_redacted"`
	Reasons        []string        `json:"reasons"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Status         string          `json:"status"`
	ExpiresAt      time.Time       `json:"expires_at"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	DecisionNote   string          `json:"decision_note,omitempty"`
	ConsumedAt     *time.Time      `json:"consumed_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Binding identifies the exact call an approval authorises.
type Binding struct {
	Tenant, Agent, Server, Tool, ArgsSHA256 string
}

// Matches reports whether the approval is bound to b.
func (a Approval) Matches(b Binding) bool {
	return a.Tenant == b.Tenant && a.Agent == b.Agent && a.Server == b.Server && a.Tool == b.Tool && a.ArgsSHA256 == b.ArgsSHA256
}

// ListFilter selects approvals (newest first).
type ListFilter struct {
	Tenant string // "" = all tenants
	Status string // "" = any
	Limit  int
}

// OutboxItem is a pending webhook delivery.
type OutboxItem struct {
	ID         int64
	ApprovalID string
	Payload    json.RawMessage
	Attempts   int
}

// Store persists approvals and the webhook outbox.
type Store interface {
	// Create inserts a pending approval and its webhook outbox row atomically.
	Create(ctx context.Context, a Approval, webhook json.RawMessage) error
	Get(ctx context.Context, id string) (Approval, error)
	// FindPending returns an unexpired pending approval bound to b, if any.
	FindPending(ctx context.Context, b Binding, now time.Time) (Approval, bool, error)
	List(ctx context.Context, f ListFilter) ([]Approval, error)
	// Decide moves a pending approval to approved or rejected. tenant ("" = no check) must match.
	Decide(ctx context.Context, id, tenant, status, by, note string, now time.Time) (Approval, error)
	// Consume atomically checks and consumes an approved approval for b. It returns the approval
	// (with its status) and a -32045 reason when it cannot be consumed.
	Consume(ctx context.Context, id string, b Binding, now time.Time) (Approval, string, error)
	// Expire marks pending approvals past their expiry as expired.
	Expire(ctx context.Context, now time.Time) (int64, error)
	// CountPending returns the number of pending, unexpired approvals.
	CountPending(ctx context.Context, now time.Time) (int64, error)

	DueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error)
	MarkDelivered(ctx context.Context, id int64, now time.Time) error
	// MarkFailed records a failed attempt; a zero next means "abandoned, never retry".
	MarkFailed(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error
	// Retention: delete decided approvals and findings older than before, delivered outbox rows
	// older than outboxBefore.
	Purge(ctx context.Context, before, outboxBefore time.Time) (int64, error)
}

// Consume reasons (-32045 data.reason).
const (
	ReasonRejected = "approval_rejected"
	ReasonExpired  = "approval_expired"
	ReasonConsumed = "approval_consumed"
	ReasonMismatch = "approval_mismatch"
	ReasonNotFound = "approval_not_found"
	ReasonPending  = "approval_pending" // not an error: the held result is returned again
)

// consumeDecision is shared by the stores: what happens to approval a when b retries at now.
func consumeDecision(a Approval, b Binding, now time.Time) string {
	switch {
	case a.Tenant != b.Tenant:
		return ReasonNotFound // other tenants' approvals are invisible
	case !a.Matches(b):
		return ReasonMismatch
	case a.Status == StatusConsumed:
		return ReasonConsumed
	case a.Status == StatusRejected:
		return ReasonRejected
	case a.Status == StatusExpired, !now.Before(a.ExpiresAt):
		return ReasonExpired
	case a.Status == StatusPending:
		return ReasonPending
	default:
		return ""
	}
}
