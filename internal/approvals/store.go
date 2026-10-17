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
	// ErrDuplicatePending is returned by Create when an unexpired pending approval for the same
	// call (binding) already exists.
	ErrDuplicatePending = errors.New("a pending approval already exists for this call")
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

// Binding identifies the exact call an approval authorises: tenant, agent, tool, the hash of the
// arguments and the idempotency key of the held call.
type Binding struct {
	Tenant, Agent, Server, Tool, ArgsSHA256 string
	IdempotencyKey                          string
}

// Matches reports whether the approval is bound to b.
func (a Approval) Matches(b Binding) bool {
	return a.Tenant == b.Tenant && a.Agent == b.Agent && a.Server == b.Server && a.Tool == b.Tool &&
		a.ArgsSHA256 == b.ArgsSHA256 && a.IdempotencyKey == b.IdempotencyKey
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
	// Decide moves a pending approval to approved or rejected. tenant ("" = no check) must match,
	// and the agent whose call was held (by) may reject it but not approve it (ErrForbidden).
	Decide(ctx context.Context, id, tenant, status, by, note string, now time.Time) (Approval, error)
	// Consume atomically checks and consumes an approved approval for b. It returns the approval
	// (with its status) and a -32045 reason when it cannot be consumed.
	Consume(ctx context.Context, id string, b Binding, now time.Time) (Approval, string, error)
	// Expire marks pending approvals past their expiry as expired.
	Expire(ctx context.Context, now time.Time) (int64, error)
	// CountPending returns the number of pending, unexpired approvals.
	CountPending(ctx context.Context, now time.Time) (int64, error)
	// OldestPending returns the creation time of the oldest pending, unexpired approval.
	OldestPending(ctx context.Context, now time.Time) (time.Time, bool, error)

	// DueOutbox claims up to limit due, undelivered items for OutboxLease: a claimed item is not
	// due again until the lease ends (or MarkFailed reschedules it), so replicas sharing the store
	// never deliver the same item concurrently, and a replica that dies mid-batch only delays it.
	DueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error)
	MarkDelivered(ctx context.Context, id int64, now time.Time) error
	// MarkFailed records a failed attempt; a zero next means "abandoned, never retry".
	MarkFailed(ctx context.Context, id int64, attempts int, next time.Time, lastErr string) error
	// Retention: delete decided approvals and findings older than before, delivered outbox rows
	// older than outboxBefore.
	Purge(ctx context.Context, before, outboxBefore time.Time) (int64, error)
}

// OutboxLease is how long DueOutbox claims an item: longer than a worst-case batch (50 items at the
// 10 s webhook timeout).
const OutboxLease = 15 * time.Minute

// ValidID reports whether id has the canonical 8-4-4-4-12 hex UUID shape; anything else can never
// name an approval.
func ValidID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F'):
			return false
		}
	}
	return true
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
	case a.Status == StatusConsumed:
		// Checked before the binding: any reuse of a spent approval is approval_consumed. The
		// pipeline lets the matching same-key retry through to the idempotency stage (replay).
		return ReasonConsumed
	case !a.Matches(b):
		return ReasonMismatch
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
