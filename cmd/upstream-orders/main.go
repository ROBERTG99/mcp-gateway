// Command upstream-orders is the example orders MCP upstream (Streamable HTTP on :8215/mcp).
// With MCPGW_FAULT_TIMEOUT_RATE > 0 it injects post-commit stalls for the chaos eval (fault.go).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/mcp-gateway/internal/upstreams"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := upstreams.Main(ctx, "orders", ":8215", os.Args[1:], os.Stderr, build)
	stop()
	os.Exit(code)
}

func build(pool *pgxpool.Pool) (*mcpserver.Server, error) {
	dedup, err := mcpserver.NewPostgresDedup(pool, "orders")
	if err != nil {
		return nil, err
	}
	s := mcpserver.New("orders", version.Version(), dedup)
	upstreams.RegisterOrders(s, pool, upstreams.OrdersOptions{AfterRefund: faultHook()})
	return s, nil
}
