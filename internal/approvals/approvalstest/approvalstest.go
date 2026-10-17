// Package approvalstest is the conformance suite for approvals.Store implementations.
package approvalstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

var counter atomic.Int64

func newApproval(tenant string) approvals.Approval {
	n := counter.Add(1)
	return approvals.Approval{
		ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Tenant: tenant, Agent: "agent-support", Server: "orders",
		Tool: "refund_order", ArgsSHA256: strings.Repeat("a", 64), ArgsRedacted: json.RawMessage(`{"amount_cents":50000}`),
		Reasons: []string{"refund_requires_approval"}, IdempotencyKey: "refund-key-0001", Status: approvals.StatusPending,
		ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now, UpdatedAt: now,
	}
}

func binding(a approvals.Approval) approvals.Binding {
	return approvals.Binding{Tenant: a.Tenant, Agent: a.Agent, Server: a.Server, Tool: a.Tool, ArgsSHA256: a.ArgsSHA256,
		IdempotencyKey: a.IdempotencyKey}
}

// Run executes the suite; newStore returns an empty store per subtest.
func Run(t *testing.T, newStore func(t *testing.T) approvals.Store) {
	ctx := context.Background()
	create := func(t *testing.T, s approvals.Store, a approvals.Approval) {
		t.Helper()
		if err := s.Create(ctx, a, json.RawMessage(`{"approval_id":"`+a.ID+`"}`)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("create, find pending, approve, consume once", func(t *testing.T) {
		s := newStore(t)
		a := newApproval("t_alpha")
		create(t, s, a)
		got, ok, err := s.FindPending(ctx, binding(a), now)
		if err != nil || !ok || got.ID != a.ID {
			t.Fatalf("find pending: %+v %v %v", got, ok, err)
		}
		if _, reason, _ := s.Consume(ctx, a.ID, binding(a), now); reason != approvals.ReasonPending {
			t.Fatalf("consume pending: reason %q", reason)
		}
		d, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusApproved, "jo.approver", "ok", now.Add(time.Minute))
		if err != nil || d.Status != approvals.StatusApproved || d.DecidedBy != "jo.approver" || d.DecidedAt == nil {
			t.Fatalf("decide: %+v %v", d, err)
		}
		if _, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusRejected, "x", "", now.Add(time.Minute)); !errors.Is(err, approvals.ErrNotPending) {
			t.Fatalf("second decision: %v", err)
		}
		c, reason, err := s.Consume(ctx, a.ID, binding(a), now.Add(2*time.Minute))
		if err != nil || reason != "" || c.Status != approvals.StatusConsumed {
			t.Fatalf("consume: %+v %q %v", c, reason, err)
		}
		if _, reason, _ := s.Consume(ctx, a.ID, binding(a), now.Add(3*time.Minute)); reason != approvals.ReasonConsumed {
			t.Fatalf("second consume: %q", reason)
		}
	})

	t.Run("decision rules", func(t *testing.T) {
		s := newStore(t)
		a := newApproval("t_alpha")
		create(t, s, a)
		if _, err := s.Decide(ctx, a.ID, "t_beta", approvals.StatusApproved, "x", "", now); !errors.Is(err, approvals.ErrForbidden) {
			t.Fatalf("other tenant: %v", err)
		}
		// No self-approval: the agent whose call was held may not approve it, but may withdraw it.
		if _, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusApproved, a.Agent, "", now); !errors.Is(err, approvals.ErrForbidden) {
			t.Fatalf("self-approval: %v", err)
		}
		w := newApproval("t_alpha")
		w.IdempotencyKey = "refund-key-withdraw" // a different call: one pending approval per call
		create(t, s, w)
		if d, err := s.Decide(ctx, w.ID, "t_alpha", approvals.StatusRejected, w.Agent, "no longer needed", now); err != nil || d.Status != approvals.StatusRejected {
			t.Fatalf("self-withdrawal: %+v %v", d, err)
		}
		if _, err := s.Decide(ctx, "00000000-0000-4000-8000-999999999999", "", approvals.StatusApproved, "x", "", now); !errors.Is(err, approvals.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if _, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusApproved, "x", "", now.Add(16*time.Minute)); !errors.Is(err, approvals.ErrExpired) {
			t.Fatalf("expired: %v", err)
		}
		r := newApproval("t_alpha")
		create(t, s, r)
		if _, err := s.Decide(ctx, r.ID, "t_alpha", approvals.StatusRejected, "jo", "too large", now); err != nil {
			t.Fatal(err)
		}
		if _, reason, _ := s.Consume(ctx, r.ID, binding(r), now); reason != approvals.ReasonRejected {
			t.Fatalf("rejected: %q", reason)
		}
	})

	t.Run("binding", func(t *testing.T) {
		s := newStore(t)
		a := newApproval("t_alpha")
		create(t, s, a)
		if _, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusApproved, "jo", "", now); err != nil {
			t.Fatal(err)
		}
		b := binding(a)
		other := b
		other.ArgsSHA256 = strings.Repeat("b", 64)
		if _, reason, _ := s.Consume(ctx, a.ID, other, now); reason != approvals.ReasonMismatch {
			t.Fatalf("other args: %q", reason)
		}
		other = b
		other.Agent = "agent-collections"
		if _, reason, _ := s.Consume(ctx, a.ID, other, now); reason != approvals.ReasonMismatch {
			t.Fatalf("other agent: %q", reason)
		}
		other = b
		other.Tenant = "t_beta"
		if _, reason, _ := s.Consume(ctx, a.ID, other, now); reason != approvals.ReasonNotFound {
			t.Fatalf("other tenant: %q", reason)
		}
		if _, reason, _ := s.Consume(ctx, "00000000-0000-4000-8000-999999999998", b, now); reason != approvals.ReasonNotFound {
			t.Fatalf("missing: %q", reason)
		}
		if _, reason, _ := s.Consume(ctx, a.ID, b, now.Add(time.Hour)); reason != approvals.ReasonExpired {
			t.Fatalf("approved but past expiry: %q", reason)
		}
		// Once consumed, any reuse in the tenant is approval_consumed; other tenants still see nothing.
		if _, reason, err := s.Consume(ctx, a.ID, b, now); err != nil || reason != "" {
			t.Fatalf("consume: %q %v", reason, err)
		}
		other = b
		other.ArgsSHA256 = strings.Repeat("b", 64)
		if _, reason, _ := s.Consume(ctx, a.ID, other, now); reason != approvals.ReasonConsumed {
			t.Fatalf("consumed, other args: %q", reason)
		}
		other.Tenant = "t_beta"
		if _, reason, _ := s.Consume(ctx, a.ID, other, now); reason != approvals.ReasonNotFound {
			t.Fatalf("consumed, other tenant: %q", reason)
		}
	})

	t.Run("one pending approval per call", func(t *testing.T) {
		s := newStore(t)
		a := newApproval("t_alpha")
		create(t, s, a)
		dup := newApproval("t_alpha") // same binding, new id
		if err := s.Create(ctx, dup, json.RawMessage(`{}`)); !errors.Is(err, approvals.ErrDuplicatePending) {
			t.Fatalf("second pending approval for one call: %v, want ErrDuplicatePending", err)
		}
		// Concurrent identical holds: exactly one is created (one approval, one webhook).
		var created atomic.Int64
		var wg sync.WaitGroup
		other := newApproval("t_beta")
		for range 8 {
			wg.Go(func() {
				c := other
				c.ID = newApproval("t_beta").ID
				if err := s.Create(ctx, c, json.RawMessage(`{}`)); err == nil {
					created.Add(1)
				} else if !errors.Is(err, approvals.ErrDuplicatePending) {
					t.Errorf("create: %v", err)
				}
			})
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("created %d pending approvals for one call", created.Load())
		}
		// A pending approval past its expiry does not block a new one for the same call.
		later := newApproval("t_alpha")
		later.CreatedAt, later.UpdatedAt, later.ExpiresAt = now.Add(20*time.Minute), now.Add(20*time.Minute), now.Add(35*time.Minute)
		if err := s.Create(ctx, later, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("create after the old one expired: %v", err)
		}
		if got, ok, err := s.FindPending(ctx, binding(later), now.Add(21*time.Minute)); err != nil || !ok || got.ID != later.ID {
			t.Fatalf("find pending = %+v %v %v", got, ok, err)
		}
		// Malformed ids never match.
		if _, err := s.Get(ctx, "not-a-uuid"); !errors.Is(err, approvals.ErrNotFound) {
			t.Fatalf("get malformed id: %v", err)
		}
		if _, reason, err := s.Consume(ctx, "'; DROP TABLE approvals; --", binding(a), now); err != nil || reason != approvals.ReasonNotFound {
			t.Fatalf("consume malformed id: %q %v", reason, err)
		}
	})

	t.Run("outbox items are claimed once", func(t *testing.T) {
		s := newStore(t)
		for _, tenant := range []string{"t_alpha", "t_beta", "t_gamma"} {
			create(t, s, newApproval(tenant))
		}
		var claimed sync.Map
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				due, err := s.DueOutbox(ctx, now, 10)
				if err != nil {
					t.Errorf("due: %v", err)
				}
				for _, it := range due {
					if _, dup := claimed.LoadOrStore(it.ID, true); dup {
						t.Errorf("item %d claimed twice", it.ID)
					}
				}
			})
		}
		wg.Wait()
		n := 0
		claimed.Range(func(any, any) bool { n++; return true })
		if n != 3 {
			t.Fatalf("claimed %d items, want 3", n)
		}
		if due, _ := s.DueOutbox(ctx, now.Add(time.Minute), 10); len(due) != 0 {
			t.Fatalf("claimed items due again inside the lease: %+v", due)
		}
		if due, _ := s.DueOutbox(ctx, now.Add(approvals.OutboxLease), 10); len(due) != 3 {
			t.Fatalf("items of a dead claimant after the lease: %d, want 3", len(due))
		}
	})

	t.Run("concurrent consume executes once", func(t *testing.T) {
		s := newStore(t)
		a := newApproval("t_alpha")
		create(t, s, a)
		if _, err := s.Decide(ctx, a.ID, "t_alpha", approvals.StatusApproved, "jo", "", now); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var wins atomic.Int64
		for range 12 {
			wg.Go(func() {
				if _, reason, err := s.Consume(ctx, a.ID, binding(a), now); err == nil && reason == "" {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("consumed %d times", wins.Load())
		}
	})

	t.Run("expire, count, list, outbox, purge", func(t *testing.T) {
		s := newStore(t)
		a, b := newApproval("t_alpha"), newApproval("t_beta")
		b.CreatedAt = now.Add(time.Second)
		create(t, s, a)
		create(t, s, b)
		if n, _ := s.CountPending(ctx, now); n != 2 {
			t.Fatalf("pending = %d", n)
		}
		if oldest, ok, err := s.OldestPending(ctx, now.Add(5*time.Second)); err != nil || !ok || !oldest.Equal(a.CreatedAt) {
			t.Fatalf("oldest pending = %v %v %v, want %v", oldest, ok, err, a.CreatedAt)
		}
		if _, ok, _ := s.OldestPending(ctx, now.Add(16*time.Minute)); ok {
			t.Fatal("expired approvals counted as pending")
		}
		list, err := s.List(ctx, approvals.ListFilter{Tenant: "t_alpha"})
		if err != nil || len(list) != 1 || list[0].ID != a.ID {
			t.Fatalf("list t_alpha: %+v %v", list, err)
		}
		due, err := s.DueOutbox(ctx, now.Add(time.Second), 10)
		if err != nil || len(due) != 2 {
			t.Fatalf("due outbox: %+v %v", due, err)
		}
		if err := s.MarkFailed(ctx, due[0].ID, 1, now.Add(2*time.Second), "503"); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDelivered(ctx, due[1].ID, now); err != nil {
			t.Fatal(err)
		}
		if due2, _ := s.DueOutbox(ctx, now.Add(time.Second), 10); len(due2) != 0 {
			t.Fatalf("not yet due: %+v", due2)
		}
		if due3, _ := s.DueOutbox(ctx, now.Add(2*time.Second), 10); len(due3) != 1 || due3[0].Attempts != 1 {
			t.Fatalf("due after backoff: %+v", due3)
		}
		if n, _ := s.Expire(ctx, now.Add(16*time.Minute)); n != 2 {
			t.Fatalf("expired %d", n)
		}
		if n, _ := s.CountPending(ctx, now.Add(16*time.Minute)); n != 0 {
			t.Fatalf("pending after expiry = %d", n)
		}
		if n, err := s.Purge(ctx, now.Add(91*24*time.Hour), now.Add(8*24*time.Hour)); err != nil || n < 3 {
			t.Fatalf("purge removed %d (%v)", n, err)
		}
		if _, err := s.Get(ctx, a.ID); !errors.Is(err, approvals.ErrNotFound) {
			t.Fatalf("purged approval still present: %v", err)
		}
	})
}
