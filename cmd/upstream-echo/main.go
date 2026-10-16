// Command upstream-echo is a minimal MCP upstream with one "echo" tool, used by the walking
// skeleton, benchmarks and the fourth-upstream integration test.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/testupstream"
)

func main() {
	addr := flag.String("addr", ":8216", "listen address")
	name := flag.String("name", "echo", "server name")
	flag.Parse()
	mux := http.NewServeMux()
	mux.Handle("/mcp", testupstream.EchoHandler(*name))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("upstream-echo listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}
