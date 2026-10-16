//go:build linux || darwin

package audit_test

import (
	"time"

	"golang.org/x/sys/unix"
)

// threadCPU is the CPU time consumed by the calling OS thread (the caller locks its goroutine to
// the thread). Unlike wall-clock time it excludes time the thread spent preempted by other
// processes, so it measures the cost of the code path itself.
func threadCPU() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts); err != nil {
		panic(err)
	}
	return time.Duration(ts.Nano())
}
