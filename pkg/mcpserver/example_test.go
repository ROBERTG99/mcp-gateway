package mcpserver_test

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

// An upstream registers reads and writes; the gateway supplies tenant and idempotency key.
func Example() {
	type refundIn struct {
		OrderID     string `json:"order_id"`
		AmountCents int64  `json:"amount_cents"`
	}
	type refundOut struct {
		RefundID string `json:"refund_id"`
	}
	srv := mcpserver.New("orders", "v0.1.0", mcpserver.NewMemoryDedup())
	mcpserver.WriteTool(srv, &mcp.Tool{Name: "refund_order", Description: "Refund an order"},
		func(_ context.Context, w mcpserver.WriteContext, in refundIn) (refundOut, error) {
			// With NewPostgresDedup, w.Tx is the transaction that also records the key.
			return refundOut{RefundID: w.Tenant + "/" + in.OrderID}, nil
		})

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	_, _ = srv.MCP().Connect(ctx, st, nil)
	cs, _ := mcp.NewClient(&mcp.Implementation{Name: "gateway", Version: "v0"}, nil).Connect(ctx, ct, nil)
	defer func() { _ = cs.Close() }()
	params := &mcp.CallToolParams{
		Name:      "refund_order",
		Arguments: refundIn{OrderID: "O-00001", AmountCents: 5000},
		Meta: mcp.Meta{
			"idempotency_key": "refund-O-00001-1",
			"mcpgw":           map[string]any{"tenant": "t_alpha", "agent": "agent-support", "request_id": "req-1"},
		},
	}
	for range 2 { // the retry replays the stored result
		res, _ := cs.CallTool(ctx, params)
		fmt.Println(res.StructuredContent)
	}
	// Output:
	// map[refund_id:t_alpha/O-00001]
	// map[refund_id:t_alpha/O-00001]
}
