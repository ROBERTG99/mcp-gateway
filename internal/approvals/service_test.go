package approvals_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func hold(t *testing.T, s *approvals.Service) approvals.Approval {
	t.Helper()
	a, err := s.Hold(t.Context(), approvals.HoldRequest{
		Binding: approvals.Binding{Tenant: "t_alpha", Agent: "agent-support", Server: "orders", Tool: "refund_order",
			ArgsSHA256: strings.Repeat("a", 64)},
		Args:    json.RawMessage(`{"order_id":"O-00001","amount_cents":50000,"reason":"card 4111 1111 1111 1111"}`),
		Reasons: []string{"refund_requires_approval"}, IdempotencyKey: "refund-key-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApprovalWebhookContract(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var sigs []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies, sigs = append(bodies, b), append(sigs, r.Header.Get("X-MCPGW-Signature"))
		mu.Unlock()
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
		}
	}))
	t.Cleanup(sink.Close)
	c := clock.NewFake(t0)
	store := approvals.NewMemory()
	svc := approvals.NewService(store, approvals.Options{TTL: 15 * time.Minute, Clock: c, Redactor: redact.New()})
	a := hold(t, svc)
	// Holding the same call again while pending returns the same approval (no new webhook).
	if again := hold(t, svc); again.ID != a.ID {
		t.Fatalf("second hold created %s, want %s", again.ID, a.ID)
	}
	w := approvals.NewOutboxWorker(store, approvals.NewWebhookNotifier(sink.URL, "s3cret", nil), approvals.OutboxOptions{Clock: c})
	if delivered, _, err := w.RunOnce(t.Context()); err != nil || delivered != 1 {
		t.Fatalf("run once: delivered=%d err=%v", delivered, err)
	}
	if len(bodies) != 1 {
		t.Fatalf("webhook calls = %d", len(bodies))
	}
	var payload map[string]any
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"agent", "approval_id", "args_redacted", "expires_at", "reasons", "tenant", "tool"}
	if !slices.Equal(keys, want) {
		t.Fatalf("payload keys = %v, want %v", keys, want)
	}
	if payload["approval_id"] != a.ID || payload["tool"] != "orders.refund_order" || payload["tenant"] != "t_alpha" ||
		payload["expires_at"] != t0.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("payload = %s", bodies[0])
	}
	if strings.Contains(string(bodies[0]), "4111") || !strings.Contains(string(bodies[0]), "[REDACTED:CARD]") {
		t.Fatalf("webhook args not redacted: %s", bodies[0])
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(bodies[0])
	if sigs[0] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q does not verify", sigs[0])
	}
	if !approvals.VerifySignature("s3cret", bodies[0], sigs[0]) || approvals.VerifySignature("wrong", bodies[0], sigs[0]) {
		t.Fatal("VerifySignature")
	}
	if delivered, _, _ := w.RunOnce(t.Context()); delivered != 0 {
		t.Fatal("delivered twice")
	}
}

type failingNotifier struct{ calls int }

func (f *failingNotifier) Deliver(context.Context, []byte) error {
	f.calls++
	return errors.New("503 from sink")
}

func TestOutboxBackoff(t *testing.T) {
	c := clock.NewFake(t0)
	store := approvals.NewMemory()
	svc := approvals.NewService(store, approvals.Options{TTL: 48 * time.Hour, Clock: c, Redactor: redact.New()})
	hold(t, svc)
	n := &failingNotifier{}
	w := approvals.NewOutboxWorker(store, n, approvals.OutboxOptions{Clock: c})
	var delays []time.Duration
	last := c.Now()
	for range 40 {
		if _, failed, _ := w.RunOnce(t.Context()); failed == 1 {
			_, next, _ := store.OutboxState(1)
			if next.IsZero() { // abandoned after the last attempt
				break
			}
			delays = append(delays, next.Sub(c.Now()))
			last = next
		}
		c.Set(last)
	}
	if n.calls != 24 {
		t.Fatalf("delivery attempts = %d, want 24", n.calls)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	for i, d := range want {
		if delays[i] != d {
			t.Fatalf("delay %d = %s, want %s (all %v)", i, delays[i], d, delays)
		}
	}
	if len(delays) != 23 {
		t.Fatalf("retries scheduled = %d, want 23", len(delays))
	}
	for _, d := range delays {
		if d > time.Hour {
			t.Fatalf("delay %s exceeds the 1 h cap", d)
		}
	}
	if delays[22] != time.Hour {
		t.Fatalf("delay before the last attempt = %s, want the 1 h cap", delays[22])
	}
}

type auditSpy struct {
	sink *audit.MemorySink
	w    *audit.Writer
}

func newAuditSpy(t *testing.T) auditSpy {
	sink := audit.NewMemorySink()
	w := audit.NewWriter(sink, audit.NewMemorySeq(), audit.Options{FlushInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return auditSpy{sink: sink, w: w}
}

func TestDecideRBACAndAudit(t *testing.T) {
	c := clock.NewFake(t0)
	spy := newAuditSpy(t)
	svc := approvals.NewService(approvals.NewMemory(), approvals.Options{TTL: 15 * time.Minute, Clock: c, Redactor: redact.New(), Audit: spy.w})
	a := hold(t, svc)
	approver := authn.Principal{Tenant: "t_alpha", Agent: "jo.approver", Scopes: []string{"mcpgw:approve"}}
	reviewer := authn.Principal{Tenant: "*", Agent: "sam.security", Scopes: []string{"mcpgw:audit:read"}}
	otherTenant := authn.Principal{Tenant: "t_beta", Agent: "kim.approver", Scopes: []string{"mcpgw:approve"}}
	for name, p := range map[string]authn.Principal{"reviewer": reviewer, "other tenant": otherTenant} {
		if _, err := svc.Decide(t.Context(), a.ID, p, true, "note"); !errors.Is(err, approvals.ErrForbidden) {
			t.Fatalf("%s: %v, want ErrForbidden", name, err)
		}
	}
	d, err := svc.Decide(t.Context(), a.ID, approver, true, "looks right")
	if err != nil || d.Status != approvals.StatusApproved || d.DecidedBy != "jo.approver" {
		t.Fatalf("approve: %+v %v", d, err)
	}
	if _, err := svc.Decide(t.Context(), a.ID, approver, false, "changed my mind"); !errors.Is(err, approvals.ErrNotPending) {
		t.Fatalf("second decision: %v", err)
	}
	r := hold(t, svc) // the first is approved, so this is a new pending approval
	if _, err := svc.Decide(t.Context(), r.ID, approver, false, ""); !errors.Is(err, approvals.ErrReasonRequired) {
		t.Fatalf("reject without reason: %v", err)
	}
	if _, err := svc.Decide(t.Context(), r.ID, approver, false, "amount too high"); err != nil {
		t.Fatal(err)
	}
	if err := spy.w.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := spy.sink.Events()
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2 (one per decision)", len(events))
	}
	if e := events[0]; e.Server != "mcpgw" || e.Tool != "approval.approve" || e.Agent != "jo.approver" || e.Tenant != "t_alpha" ||
		e.ApprovalID == nil || *e.ApprovalID != a.ID || e.Status != "ok" {
		t.Fatalf("approve event = %+v", e)
	}
	if e := events[1]; e.Tool != "approval.reject" || *e.ApprovalID != r.ID {
		t.Fatalf("reject event = %+v", e)
	}
	// Visibility: other tenants cannot see the approval; the reviewer ("*") can.
	if _, err := svc.Get(t.Context(), a.ID, otherTenant); !errors.Is(err, approvals.ErrNotFound) {
		t.Fatalf("other tenant get: %v", err)
	}
	if _, err := svc.Get(t.Context(), a.ID, reviewer); err != nil {
		t.Fatalf("reviewer get: %v", err)
	}
}
