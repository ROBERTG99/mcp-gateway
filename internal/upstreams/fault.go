package upstreams

import (
	"context"
	"hash/fnv"
	"time"
)

// TimeoutFault returns a RefundHook that, for a deterministic fraction rate of idempotency keys,
// stalls for stall inside the refund transaction (after the insert, before commit, holding the key
// claim) so the gateway's upstream call times out. The transaction is detached from the caller
// (pkg/mcpserver runs writes with context.WithoutCancel), so it still commits after the stall; the
// client's same-key retry blocks on the claim and then replays the stored result.
func TimeoutFault(rate float64, stall time.Duration) RefundHook {
	if rate <= 0 {
		return nil
	}
	return func(ctx context.Context, _ string, key string) error {
		if !FaultSelected(key, rate) {
			return nil
		}
		t := time.NewTimer(stall)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err() // only the transaction's own timeout ends the stall early
		}
	}
}

// FaultSelected reports whether key falls in the faulted fraction rate (deterministic).
func FaultSelected(key string, rate float64) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%10000) < rate*10000
}
