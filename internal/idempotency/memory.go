package idempotency

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// Memory is an in-process Store (tests and single-process demos).
type Memory struct {
	mu      sync.Mutex
	clock   clock.Clock
	entries map[string]memEntry
}

type memEntry struct {
	rec     record
	expires time.Time
}

// NewMemory returns an empty in-memory store.
func NewMemory(c clock.Clock) *Memory {
	if c == nil {
		c = clock.Real{}
	}
	return &Memory{clock: c, entries: map[string]memEntry{}}
}

func memKey(tenant, key string) string { return tenant + "\x00" + key }

func (m *Memory) live(k string) (memEntry, bool) {
	e, ok := m.entries[k]
	if ok && !m.clock.Now().Before(e.expires) {
		delete(m.entries, k)
		return memEntry{}, false
	}
	return e, ok
}

// Acquire implements Store.
func (m *Memory) Acquire(_ context.Context, tenant, key, argsHash string, lockTTL time.Duration) (Acquisition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenant, key)
	if e, ok := m.live(k); ok {
		return decide(e.rec, argsHash), nil
	}
	m.entries[k] = memEntry{rec: record{State: stateInProgress, ArgsHash: argsHash}, expires: m.clock.Now().Add(lockTTL)}
	return Acquisition{Outcome: Acquired}, nil
}

// Complete implements Store.
func (m *Memory) Complete(_ context.Context, tenant, key, argsHash string, result json.RawMessage, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenant, key)
	e, ok := m.live(k)
	if !ok || e.rec.State != stateInProgress || e.rec.ArgsHash != argsHash {
		return ErrNotOwner
	}
	m.entries[k] = memEntry{rec: record{State: stateCompleted, ArgsHash: argsHash, Result: result}, expires: m.clock.Now().Add(ttl)}
	return nil
}

// Release implements Store.
func (m *Memory) Release(_ context.Context, tenant, key, argsHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(tenant, key)
	if e, ok := m.live(k); ok && e.rec.State == stateInProgress && e.rec.ArgsHash == argsHash {
		delete(m.entries, k)
	}
	return nil
}

// Ping implements Store.
func (m *Memory) Ping(context.Context) error { return nil }
