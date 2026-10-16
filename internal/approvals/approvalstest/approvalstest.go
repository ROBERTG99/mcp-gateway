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
	return approvals.Binding{Tenant: a.Tenant, Agent: a.Agent, Server: a.Server, Tool: a.Tool, ArgsSHA256: a.ArgsSHA256}
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
