//go:build integration

package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// TestBodyLimitThroughGateway (NFR-9) sends authenticated MCP requests over 1 MiB to the real
// endpoint (App.MCPHandler: body limit, authn, proxy and the MCP SDK), declared and streamed, and
// expects 413 for both; a small request is not refused.
func TestBodyLimitThroughGateway(t *testing.T) {
	st := stack.Start(t, stack.Options{})
	token := st.Token(t, "t_alpha", "agent-support", "crm:read")
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("a", 1<<20) + `"}}`
	for _, tc := range []struct {
		name    string
		body    string
		chunked bool
		want    int
	}{
		{"declared over", big, false, http.StatusRequestEntityTooLarge},
		{"streamed over", big, true, http.StatusRequestEntityTooLarge},
	} {
		var body io.Reader = bytes.NewReader([]byte(tc.body))
		if tc.chunked {
			body = io.MultiReader(body) // hides the length: the request is sent chunked
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, st.MCPURL, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// TestRedisOutageKeepsReadsServed (spec §4.6): with Redis unreachable the gateway stays ready
// (/readyz 200, redis check "unreachable"), reads are served on the per-process token bucket and
// writes are refused with -32049, so a Redis outage does not take every replica out of service.
func TestRedisOutageKeepsReadsServed(t *testing.T) {
	st := stack.Start(t, stack.Options{Configure: func(c *config.Config) { c.RedisURL = "redis://127.0.0.1:1/0" }})
	resp, err := http.Get(st.AdminURL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body.Checks["redis"], "unreachable") {
		t.Fatalf("readyz = %d %+v, want 200 with redis unreachable", resp.StatusCode, body)
	}
	sess, err := demo.Connect(t.Context(), st.MCPURL, st.Token(t, "t_alpha", "agent-support", "crm:read", "orders:write"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "crm.get_customer", Arguments: map[string]any{"customer_id": "C-0001"}})
	if err != nil || res.IsError {
		t.Fatalf("read during a Redis outage: %v %+v", err, res)
	}
	_, err = sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "orders.refund_order",
		Arguments: map[string]any{"order_id": "O-00001", "amount_cents": 100, "reason": "outage"},
		Meta:      mcp.Meta{"idempotency_key": "redis-outage-0001"}})
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != mcperr.CodeDependencyUnavailable {
		t.Fatalf("write during a Redis outage: %v, want -32049", err)
	}
}
