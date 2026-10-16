package main

import (
	"os"
	"strconv"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
)

// faultHook returns the chaos fault injector, or nil when MCPGW_FAULT_TIMEOUT_RATE is 0 (default).
// The decision is a deterministic function of the idempotency key, so a given key always
// behaves the same and the chaos eval is reproducible.
func faultHook() upstreams.RefundHook {
	rate, _ := strconv.ParseFloat(os.Getenv("MCPGW_FAULT_TIMEOUT_RATE"), 64)
	stall, err := time.ParseDuration(os.Getenv("MCPGW_FAULT_STALL"))
	if err != nil {
		stall = 12 * time.Second
	}
	return upstreams.TimeoutFault(rate, stall)
}
