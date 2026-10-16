// Package upstreams implements the example upstream MCP servers (ledger, crm, orders) on
// Postgres with pkg/mcpserver. Every query is scoped by the tenant the gateway forwards.
package upstreams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/version"
	"github.com/ROBERTG99/mcp-gateway/pkg/mcpserver"
)

// Names lists the example upstreams.
func Names() []string { return []string{"ledger", "crm", "orders"} }

// ErrNotFound is returned (as a tool error) for unknown customers or orders in the tenant.
var ErrNotFound = errors.New("not found")

// New builds the named upstream server.
func New(name string, pool *pgxpool.Pool) (*mcpserver.Server, error) {
	dedup, err := mcpserver.NewPostgresDedup(pool, name)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", name, err)
	}
	s := mcpserver.New(name, version.Version(), dedup)
	switch name {
	case "ledger":
		registerLedger(s, pool)
	case "crm":
		registerCRM(s, pool)
	case "orders":
		registerOrders(s, pool)
	default:
		return nil, fmt.Errorf("unknown upstream %q", name)
	}
	return s, nil
}

// Handler serves an upstream over Streamable HTTP at /mcp with /healthz.
func Handler(s *mcpserver.Server, pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.MCP() },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func notFound(kind, id string) error { return fmt.Errorf("%s %q: %w", kind, id, ErrNotFound) }
