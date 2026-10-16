package approvals

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
	"github.com/ROBERTG99/mcp-gateway/pkg/canonicaljson"
)

// ScopeApprove is required to decide approvals.
const ScopeApprove = "mcpgw:approve"

// More service errors.
var (
	ErrReasonRequired   = errors.New("a rejection needs a reason")
	ErrAuditUnavailable = errors.New("audit unavailable")
)

// Auditor reserves audit slots (audit.Writer).
type Auditor interface {
	Reserve(ctx context.Context) (*audit.Ticket, error)
}

// Options configure the service.
type Options struct {
	TTL      time.Duration // pending lifetime (MCPGW_APPROVAL_TTL, default 15 min)
	Clock    clock.Clock
	Redactor *redact.Redactor
	Audit    Auditor
	Logger   *slog.Logger
}

// Service implements the approval lifecycle on a Store.
type Service struct {
	store Store
	opts  Options
}

// NewService returns the service.
func NewService(store Store, opts Options) *Service {
	if opts.TTL == 0 {
		opts.TTL = 15 * time.Minute
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Redactor == nil {
		opts.Redactor = redact.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, opts: opts}
}

// HoldRequest describes a call that needs approval. Args are the raw arguments; they are
// redacted before being stored or sent.
type HoldRequest struct {
	Binding
	Args    json.RawMessage
	Reasons []string
}

// WebhookPayload is the approval webhook body (contract v0.1.0).
type WebhookPayload struct {
	ApprovalID   string          `json:"approval_id"`
	Tenant       string          `json:"tenant"`
	Agent        string          `json:"agent"`
	Tool         string          `json:"tool"`
	ArgsRedacted json.RawMessage `json:"args_redacted"`
	Reasons      []string        `json:"reasons"`
	ExpiresAt    string          `json:"expires_at"`
}

// Hold returns the pending approval for this exact call, creating it (with its webhook) if none.
func (s *Service) Hold(ctx context.Context, r HoldRequest) (Approval, error) {
	now := s.opts.Clock.Now().UTC()
	if a, ok, err := s.store.FindPending(ctx, r.Binding, now); err != nil || ok {
		return a, err
	}
	a := Approval{ID: newUUID(), Tenant: r.Tenant, Agent: r.Agent, Server: r.Server, Tool: r.Tool, ArgsSHA256: r.ArgsSHA256,
		ArgsRedacted: s.opts.Redactor.JSON(r.Args), Reasons: r.Reasons, IdempotencyKey: r.IdempotencyKey,
		Status: StatusPending, ExpiresAt: now.Add(s.opts.TTL).Truncate(time.Second), CreatedAt: now, UpdatedAt: now}
	if a.Reasons == nil {
		a.Reasons = []string{}
	}
	payload, err := json.Marshal(WebhookPayload{ApprovalID: a.ID, Tenant: a.Tenant, Agent: a.Agent,
		Tool: a.Server + "." + a.Tool, ArgsRedacted: a.ArgsRedacted, Reasons: a.Reasons, ExpiresAt: a.ExpiresAt.Format(time.RFC3339)})
	if err != nil {
		return Approval{}, fmt.Errorf("encode webhook: %w", err)
	}
	if err := s.store.Create(ctx, a, payload); errors.Is(err, ErrDuplicatePending) {
		// A concurrent identical call created it first: return that one (one approval, one webhook).
		if p, ok, ferr := s.store.FindPending(ctx, r.Binding, now); ferr != nil || ok {
			return p, ferr
		}
		return Approval{}, fmt.Errorf("hold: %w", err)
	} else if err != nil {
		return Approval{}, err
	}
	return a, nil
}

// visible reports whether viewer may see approvals of tenant.
func visible(viewer authn.Principal, tenant string) bool {
	return viewer.Tenant == "*" || viewer.Tenant == tenant
}

// Get returns an approval visible to viewer (other tenants' approvals look missing).
func (s *Service) Get(ctx context.Context, id string, viewer authn.Principal) (Approval, error) {
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return Approval{}, err
	}
	if !visible(viewer, a.Tenant) {
		return Approval{}, ErrNotFound
	}
	return s.withExpiry(a), nil
}

// List returns approvals visible to viewer.
func (s *Service) List(ctx context.Context, viewer authn.Principal, status string, limit int) ([]Approval, error) {
	f := ListFilter{Status: status, Limit: limit}
	if viewer.Tenant != "*" {
		f.Tenant = viewer.Tenant
	}
	list, err := s.store.List(ctx, f)
	for i := range list {
		list[i] = s.withExpiry(list[i])
	}
	return list, err
}

func (s *Service) withExpiry(a Approval) Approval {
	if a.Status == StatusPending && !s.opts.Clock.Now().Before(a.ExpiresAt) {
		a.Status = StatusExpired
	}
	return a
}

// Decide approves or rejects. The approver needs mcpgw:approve and the approval's tenant;
// rejections need a reason. Successful decisions are audited as server "mcpgw",
// tool "approval.approve" / "approval.reject", agent = approver.
func (s *Service) Decide(ctx context.Context, id string, approver authn.Principal, approve bool, note string) (Approval, error) {
	if !approver.HasScope(ScopeApprove) || approver.Tenant == "*" || approver.Tenant == "" {
		return Approval{}, ErrForbidden
	}
	if !approve && note == "" {
		return Approval{}, ErrReasonRequired
	}
	var ticket *audit.Ticket
	if s.opts.Audit != nil {
		t, err := s.opts.Audit.Reserve(ctx)
		if err != nil {
			return Approval{}, fmt.Errorf("%w: %w", ErrAuditUnavailable, err)
		}
		ticket = t
	}
	status, tool := StatusApproved, "approval.approve"
	if !approve {
		status, tool = StatusRejected, "approval.reject"
	}
	start := s.opts.Clock.Now()
	a, err := s.store.Decide(ctx, id, approver.Tenant, status, approver.Agent, note, start.UTC())
	if err != nil {
		if ticket != nil {
			ticket.Cancel()
		}
		return a, err
	}
	if ticket != nil {
		args, _ := json.Marshal(map[string]any{"approval_id": id, "note": note})
		hash, _ := canonicaljson.SHA256Hex(args)
		approvalID := a.ID
		ticket.Commit(audit.Event{TS: start, Tenant: a.Tenant, Agent: approver.Agent, Server: "mcpgw", Tool: tool,
			ArgsRedacted: s.opts.Redactor.JSON(args), ArgsSHA256: hash, Decision: "allow", ApprovalID: &approvalID,
			Status: "ok", LatencyMS: float64(s.opts.Clock.Now().Sub(start).Microseconds()) / 1000})
	}
	return a, nil
}

// Consume checks and consumes an approval for the retried call b; see Store.Consume.
func (s *Service) Consume(ctx context.Context, id string, b Binding) (Approval, string, error) {
	return s.store.Consume(ctx, id, b, s.opts.Clock.Now().UTC())
}

// PendingCount is the number of pending approvals (metric mcpgw_approvals_pending).
func (s *Service) PendingCount(ctx context.Context) (int64, error) {
	return s.store.CountPending(ctx, s.opts.Clock.Now().UTC())
}

// Maintain expires overdue approvals and applies retention (approvals and findings 90 days,
// delivered outbox rows 7 days).
func (s *Service) Maintain(ctx context.Context) error {
	now := s.opts.Clock.Now().UTC()
	if _, err := s.store.Expire(ctx, now); err != nil {
		return err
	}
	_, err := s.store.Purge(ctx, now.Add(-90*24*time.Hour), now.Add(-7*24*time.Hour))
	return err
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
