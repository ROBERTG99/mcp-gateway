//go:build integration

// Package integration_test holds end-to-end journeys that span the whole gateway.
package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/testupstream"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

// copyPolicies copies the repository's policy bundle so the test can edit its data.
func copyPolicies(t *testing.T) string {
	t.Helper()
	src := filepath.Join(stack.Root(), "policies")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestAddFourthUpstreamByConfigOnly is the platform engineer's journey (spec §3.4 a, FR-11): a
// fourth MCP server ("weather") is connected with one upstreams.yaml entry and one policy data
// change (t_alpha may use it), and no gateway code. Its tools are namespaced, visible only to the
// allowed tenant with the "weather:read" scope, callable through the full pipeline, audited, and
// listed by the admin API.
func TestAddFourthUpstreamByConfigOnly(t *testing.T) {
	weather := httptest.NewServer(testupstream.EchoHandler("weather"))
	t.Cleanup(weather.Close)

	policies := copyPolicies(t)
	tenantsFile := filepath.Join(policies, "data", "tenants.json")
	var tenants map[string]struct {
		Upstreams []string `json:"upstreams"`
	}
	b, err := os.ReadFile(tenantsFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &tenants); err != nil {
		t.Fatal(err)
	}
	alpha := tenants["t_alpha"]
	alpha.Upstreams = append(alpha.Upstreams, "weather")
	tenants["t_alpha"] = alpha
	if b, err = json.Marshal(tenants); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tenantsFile, b, 0o600); err != nil {
		t.Fatal(err)
	}

	st := stack.Start(t, stack.Options{PolicyDir: policies, ExtraUpstreams: map[string]string{"weather": weather.URL}})

	toolNames := func(tenant string, scopes ...string) []string {
		t.Helper()
		sess, err := demo.Connect(t.Context(), st.MCPURL, st.Token(t, tenant, "agent-support", scopes...))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sess.Close() }()
		res, err := sess.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	if got := toolNames("t_alpha", "weather:read"); !slices.Contains(got, "weather.echo") {
		t.Fatalf("t_alpha with weather:read sees %v, want weather.echo", got)
	}
	if got := toolNames("t_alpha", "crm:read"); slices.Contains(got, "weather.echo") {
		t.Fatal("weather.echo visible without the weather:read scope")
	}
	if got := toolNames("t_gamma", "weather:read"); slices.Contains(got, "weather.echo") {
		t.Fatal("weather.echo visible to t_gamma, which policy does not allow")
	}

	sess, err := demo.Connect(t.Context(), st.MCPURL, st.Token(t, "t_alpha", "agent-support", "weather:read"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{Name: "weather.echo", Arguments: map[string]any{"text": "sunny in Lisbon"}})
	if err != nil || res.IsError {
		t.Fatalf("weather.echo: %v %+v", err, res)
	}
	// The new upstream's output goes through output marking like every other upstream's.
	if text := res.Content[0].(*mcp.TextContent).Text; text != "<<untrusted tool output>>\nsunny in Lisbon" {
		t.Fatalf("weather.echo returned %q, want the marked echo", text)
	}

	var n int
	for deadline := time.Now().Add(10 * time.Second); n != 1 && time.Now().Before(deadline); {
		// The audit writer flushes asynchronously (every 50 ms).
		if err := st.DB.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE server = 'weather' AND tool = 'echo' AND status = 'ok'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond) //nolint:forbidigo // polling with a deadline
	}
	if n != 1 {
		t.Fatalf("audit events for weather.echo = %d, want 1", n)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, st.AdminURL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer "+st.Token(t, "t_alpha", "ops-admin", "mcpgw:admin"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	type upstream struct {
		Name  string   `json:"name"`
		Up    bool     `json:"up"`
		Tools []string `json:"tools"`
	}
	var list struct {
		Upstreams []upstream `json:"upstreams"`
	}
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/upstreams = %d %s", resp.StatusCode, body)
	}
	i := slices.IndexFunc(list.Upstreams, func(u upstream) bool { return u.Name == "weather" })
	if i < 0 || !list.Upstreams[i].Up || !slices.Equal(list.Upstreams[i].Tools, []string{"weather.echo"}) {
		t.Fatalf("admin upstreams %s: want weather up with weather.echo", body)
	}
}
