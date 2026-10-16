package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// cmdHealthcheck probes a health URL and exits 0 on HTTP 200 (used by the container HEALTHCHECK,
// since the distroless image has no shell or curl).
func cmdHealthcheck(ctx context.Context, args []string, _, stderr io.Writer) int {
	url := "http://127.0.0.1:8200/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
