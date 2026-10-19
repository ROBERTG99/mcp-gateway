//go:build integration

package server_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/server"
	"github.com/ROBERTG99/mcp-gateway/internal/testutil/stack"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestStartupUnder2s (NFR-5): with Postgres, Redis, the issuer and the upstreams already up, a
// gateway process is ready in under 2 s: /readyz answers 200 and every upstream's tools are in the
// catalog. Measured from server.New (config already parsed) to the first ready probe.
func TestStartupUnder2s(t *testing.T) {
	st := stack.Start(t, stack.Options{}) // dependencies up (and one gateway already using them)
	cfg := st.Config
	cfg.MCPAddr, cfg.AdminAddr, cfg.MetricsAddr = freeAddr(t), freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Log(err)
		}
	})

	start := time.Now()
	app, err := server.New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		done <- nil
		t.Fatal(err)
	}
	go func() { done <- app.Run(ctx) }()
	hc := &http.Client{Timeout: time.Second}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for !ready(hc, "http://"+cfg.AdminAddr+"/readyz") || !allToolsListed(app) {
		select {
		case <-deadline:
			t.Fatalf("not ready after 10 s: %+v", app.Upstreams())
		case <-tick.C:
		}
	}
	elapsed := time.Since(start)
	t.Logf("ready after %v", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("ready after %v, want < 2 s (NFR-5)", elapsed)
	}
}

func ready(hc *http.Client, url string) bool {
	resp, err := hc.Get(url)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func allToolsListed(app *server.App) bool {
	ups := app.Upstreams()
	for _, u := range ups {
		if !u.Up || len(u.Tools) == 0 {
			return false
		}
	}
	return len(ups) > 0
}
