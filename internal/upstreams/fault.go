package upstreams

import (
	"context"
	"hash/fnv"
	"time"
)

// TimeoutFault returns a RefundHook that, for a deterministic fraction rate of idempotency keys,
// stalls for stall before the refund transaction commits, so the gateway's upstream call times
// out. Only the first attempt for a key stalls; the retry finds the key already recorded (the
// transaction still commits after the stall) and replays the stored result.
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
		case <-ctx.Done():
			// The client went away: keep committing anyway, like a real system that finishes
			// work after its caller timed out. Returning nil lets the transaction commit.
		}
		return nil
	}
}

// FaultSelected reports whether key falls in the faulted fraction rate (deterministic).
func FaultSelected(key string, rate float64) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%10000) < rate*10000
}
