//go:build integration

package server_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestNoSecretsInLogs (NFR-4): with the production logger at debug level, calls carrying card
// numbers, emails and bearer tokens in their arguments (including ones the upstream echoes back
// in an error), an invalid JWT and an upstream failure leave no secret, token or raw PII in the
// logs.
func TestNoSecretsInLogs(t *testing.T) {
	logs := &syncBuffer{}
	st := stack.Start(t, stack.Options{LogWriter: logs})
	token := st.Token(t, "t_alpha", "agent-support", "orders:read", "orders:write", "crm:read")
	sess, err := demo.Connect(t.Context(), st.MCPURL, token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	card, email, bearer := "4111 1111 1111 1111", "ana.lane42@example.com", "Bearer s3cr3tTokenValue0123456789abcdef"
	calls := []*mcp.CallToolParams{
		// The order id carries a card number; the upstream echoes it in "order ... not found".
		{Name: "orders.get_order", Arguments: map[string]any{"order_id": card}},
		{Name: "orders.refund_order", Arguments: map[string]any{"order_id": "O-00007", "amount_cents": 100,
			"reason": "customer " + email + " sent " + bearer}, Meta: mcp.Meta{"idempotency_key": "logs-test-0001"}},
		{Name: "orders.refund_order", Arguments: map[string]any{"order_id": card, "amount_cents": 100, "reason": email},
			Meta: mcp.Meta{"idempotency_key": "logs-test-0002"}},
		{Name: "crm.get_customer", Arguments: map[string]any{"customer_id": email}},
	}
	for _, c := range calls {
		_, _ = sess.CallTool(t.Context(), c) // outcomes vary; only the logs matter here
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, st.MCPURL, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token+"tampered")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	out := logs.String()
	if strings.Count(out, `"msg":"step"`) < len(calls) {
		t.Fatalf("expected a step record per call; the test would prove nothing:\n%s", out)
	}
	for _, secret := range []string{card, "4111111111111111", email, "s3cr3tTokenValue", token, strings.Split(token, ".")[1]} {
		if strings.Contains(out, secret) {
			i := strings.Index(out, secret)
			t.Errorf("logs contain %q: ...%s...", secret, out[max(0, i-200):min(len(out), i+100)])
		}
	}
}
