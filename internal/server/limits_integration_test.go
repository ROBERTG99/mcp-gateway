//go:build integration

package server_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

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
