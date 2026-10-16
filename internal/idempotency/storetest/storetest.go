// Package storetest is the conformance suite every idempotency.Store must pass.
package storetest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
)

// Harness builds a fresh, empty store and can move its notion of time forward.
type Harness struct {
	Store idempotency.Store
	// Advance makes d of time pass for the store's TTLs.
	Advance func(d time.Duration)
}

const (
	lockTTL = 30 * time.Second
	ttl     = 24 * time.Hour
)

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

// Run executes the conformance suite. newHarness is called once per subtest.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()
	ctx := context.Background()
	acquire := func(t *testing.T, s idempotency.Store, tenant, key, hash string) idempotency.Acquisition {
		t.Helper()
		a, err := s.Acquire(ctx, tenant, key, hash, lockTTL)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		return a
	}
	expect := func(t *testing.T, got idempotency.Acquisition, want idempotency.Outcome) {
		t.Helper()
		if got.Outcome != want {
			t.Fatalf("outcome = %s, want %s", got.Outcome, want)
		}
	}

	t.Run("acquire, in progress, complete, replay", func(t *testing.T) {
		h := newHarness(t)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000001", hashA), idempotency.Acquired)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000001", hashA), idempotency.InProgress)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000001", hashB), idempotency.Conflict)
		if err := h.Store.Complete(ctx, "t_alpha", "key-00000001", hashA, []byte(`{"ok":true}`), ttl); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, h.Store, "t_alpha", "key-00000001", hashA)
		expect(t, a, idempotency.Replay)
		if string(a.Result) != `{"ok":true}` {
			t.Fatalf("replayed result = %s", a.Result)
		}
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000001", hashB), idempotency.Conflict)
	})

	t.Run("release lets a retry acquire, never deletes completed", func(t *testing.T) {
		h := newHarness(t)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000002", hashA), idempotency.Acquired)
		if err := h.Store.Release(ctx, "t_alpha", "key-00000002", hashA); err != nil {
			t.Fatal(err)
		}
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000002", hashA), idempotency.Acquired)
		if err := h.Store.Complete(ctx, "t_alpha", "key-00000002", hashA, []byte(`1`), ttl); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.Release(ctx, "t_alpha", "key-00000002", hashA); err != nil {
			t.Fatal(err)
		}
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000002", hashA), idempotency.Replay)
	})

	t.Run("lock and record TTLs expire", func(t *testing.T) {
		h := newHarness(t)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000003", hashA), idempotency.Acquired)
		h.Advance(lockTTL + time.Second)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000003", hashA), idempotency.Acquired)
		if err := h.Store.Complete(ctx, "t_alpha", "key-00000003", hashA, []byte(`2`), time.Hour); err != nil {
			t.Fatal(err)
		}
		h.Advance(30 * time.Minute)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000003", hashA), idempotency.Replay)
		h.Advance(31 * time.Minute)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000003", hashA), idempotency.Acquired)
	})

	t.Run("tenants are isolated", func(t *testing.T) {
		h := newHarness(t)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000004", hashA), idempotency.Acquired)
		expect(t, acquire(t, h.Store, "t_beta", "key-00000004", hashB), idempotency.Acquired)
	})

	t.Run("concurrent acquire has exactly one winner", func(t *testing.T) {
		h := newHarness(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		counts := map[idempotency.Outcome]int{}
		for range 24 {
			wg.Go(func() {
				a, err := h.Store.Acquire(ctx, "t_alpha", "key-00000005", hashA, lockTTL)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				mu.Lock()
				counts[a.Outcome]++
				mu.Unlock()
			})
		}
		wg.Wait()
		if counts[idempotency.Acquired] != 1 || counts[idempotency.InProgress] != 23 {
			t.Fatalf("outcomes = %v, want 1 acquired and 23 in progress", counts)
		}
	})

	t.Run("complete requires the owner's hash", func(t *testing.T) {
		h := newHarness(t)
		expect(t, acquire(t, h.Store, "t_alpha", "key-00000006", hashA), idempotency.Acquired)
		if err := h.Store.Complete(ctx, "t_alpha", "key-00000006", hashB, []byte(`3`), ttl); err == nil {
			t.Fatal("complete with another args hash succeeded")
		}
		if err := h.Store.Ping(ctx); err != nil {
			t.Fatalf("ping: %v", err)
		}
	})
}
