package upstreams

import (
	"context"
	"hash/fnv"
	"strings"
	"time"
)

// TimeoutFault returns a RefundHook that, for a deterministic fraction rate of idempotency keys,
// stalls for stall inside the refund transaction (holding the key claim, before the order row is
// locked, so other keys of the same order are not held up) so the gateway's upstream call times
// out. The transaction is detached from the caller (pkg/mcpserver runs writes with
// context.WithoutCancel), so the refund still commits after the stall; the client's same-key retry
// blocks on the claim and then replays the stored result.
func TimeoutFault(rate float64, stall time.Duration) RefundHook {
	return StallFault(rate, "", stall)
}

// StallFault stalls (like TimeoutFault) for keys selected by rate or starting with prefix. The
// demo uses the prefix to fail one specific call mid-flight; the chaos eval uses the rate.
func StallFault(rate float64, prefix string, stall time.Duration) RefundHook {
	if rate <= 0 && prefix == "" {
		return nil
	}
	return func(ctx context.Context, _ string, key string) error {
		if !FaultSelected(key, rate) && (prefix == "" || !strings.HasPrefix(key, prefix)) {
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
	if rate <= 0 {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%10000) < rate*10000
}
