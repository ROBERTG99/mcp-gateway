package approvals

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// Memory is an in-process Store for tests.
type Memory struct {
	mu        sync.Mutex
	approvals map[string]Approval
	outbox    []memOutbox
}

type memOutbox struct {
	item      OutboxItem
	next      time.Time
	delivered *time.Time
	lastErr   string
	created   time.Time
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{approvals: map[string]Approval{}} }

// Create implements Store.
func (m *Memory) Create(_ context.Context, a Approval, webhook json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvals[a.ID] = a
	m.outbox = append(m.outbox, memOutbox{item: OutboxItem{ID: int64(len(m.outbox) + 1), ApprovalID: a.ID, Payload: webhook},
		next: a.CreatedAt, created: a.CreatedAt})
	return nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, id string) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

// FindPending implements Store.
func (m *Memory) FindPending(_ context.Context, b Binding, now time.Time) (Approval, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.approvals {
		if a.Status == StatusPending && a.Matches(b) && now.Before(a.ExpiresAt) {
			return a, true, nil
		}
	}
	return Approval{}, false, nil
}

// List implements Store.
func (m *Memory) List(_ context.Context, f ListFilter) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.approvals {
		if (f.Tenant == "" || a.Tenant == f.Tenant) && (f.Status == "" || a.Status == f.Status) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(x, y Approval) int { return y.CreatedAt.Compare(x.CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Decide implements Store.
func (m *Memory) Decide(_ context.Context, id, tenant, status, by, note string, now time.Time) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	switch {
	case !ok:
		return Approval{}, ErrNotFound
	case tenant != "" && a.Tenant != tenant:
		return Approval{}, ErrForbidden
	case a.Status == StatusPending && !now.Before(a.ExpiresAt):
		a.Status, a.UpdatedAt = StatusExpired, now
		m.approvals[id] = a
		return a, ErrExpired
	case a.Status == StatusExpired:
		return a, ErrExpired
	case a.Status != StatusPending:
		return a, ErrNotPending
	}
	t := now
	a.Status, a.DecidedBy, a.DecidedAt, a.DecisionNote, a.UpdatedAt = status, by, &t, note, now
	m.approvals[id] = a
	return a, nil
}

// Consume implements Store.
func (m *Memory) Consume(_ context.Context, id string, b Binding, now time.Time) (Approval, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ReasonNotFound, nil
	}
	if r := consumeDecision(a, b, now); r != "" {
		if r == ReasonExpired && a.Status == StatusPending {
			a.Status, a.UpdatedAt = StatusExpired, now
			m.approvals[id] = a
		}
		return a, r, nil
	}
	t := now
	a.Status, a.ConsumedAt, a.UpdatedAt = StatusConsumed, &t, now
	m.approvals[id] = a
	return a, "", nil
}

// Expire implements Store.
func (m *Memory) Expire(_ context.Context, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, a := range m.approvals {
		if a.Status == StatusPending && !now.Before(a.ExpiresAt) {
			a.Status, a.UpdatedAt = StatusExpired, now
			m.approvals[id] = a
			n++
		}
	}
	return n, nil
}

// CountPending implements Store.
func (m *Memory) CountPending(_ context.Context, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, a := range m.approvals {
		if a.Status == StatusPending && now.Before(a.ExpiresAt) {
			n++
		}
	}
	return n, nil
}

// DueOutbox implements Store.
func (m *Memory) DueOutbox(_ context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OutboxItem
	for _, o := range m.outbox {
		if o.delivered == nil && !o.next.IsZero() && !o.next.After(now) && len(out) < limit {
			out = append(out, o.item)
		}
	}
	return out, nil
}

// MarkDelivered implements Store.
func (m *Memory) MarkDelivered(_ context.Context, id int64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].item.ID == id {
			t := now
			m.outbox[i].delivered = &t
		}
	}
	return nil
}

// MarkFailed implements Store.
func (m *Memory) MarkFailed(_ context.Context, id int64, attempts int, next time.Time, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].item.ID == id {
			m.outbox[i].item.Attempts, m.outbox[i].next, m.outbox[i].lastErr = attempts, next, lastErr
		}
	}
	return nil
}

// Purge implements Store.
func (m *Memory) Purge(_ context.Context, before, outboxBefore time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	kept := m.outbox[:0]
	for _, o := range m.outbox {
		if o.delivered != nil && o.delivered.Before(outboxBefore) {
			n++
			continue
		}
		kept = append(kept, o)
	}
	m.outbox = kept
	for id, a := range m.approvals {
		if a.Status != StatusPending && a.UpdatedAt.Before(before) {
			delete(m.approvals, id)
			n++
		}
	}
	return n, nil
}

// OutboxState exposes an outbox row for tests: attempts, next attempt, delivered.
func (m *Memory) OutboxState(id int64) (attempts int, next time.Time, delivered bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.outbox {
		if o.item.ID == id {
			return o.item.Attempts, o.next, o.delivered != nil
		}
	}
	return 0, time.Time{}, false
}
