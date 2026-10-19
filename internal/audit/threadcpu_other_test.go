//go:build !(linux || darwin)

package audit_test

import "time"

var epoch = time.Now()

// threadCPU falls back to wall-clock time where no per-thread CPU clock exists (stricter).
func threadCPU() time.Duration { return time.Since(epoch) }
