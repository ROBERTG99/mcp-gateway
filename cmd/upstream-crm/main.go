// Command upstream-crm is the example crm MCP upstream (Streamable HTTP on :8214/mcp).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := upstreams.Main(ctx, "crm", ":8214", os.Args[1:], os.Stderr, nil)
	stop()
	os.Exit(code)
}
