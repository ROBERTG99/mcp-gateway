package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// cmdHealthcheck probes a health URL and exits 0 on HTTP 200 (used by the container HEALTHCHECK,
// since the distroless image has no shell or curl).
func cmdHealthcheck(ctx context.Context, args []string, _, stderr io.Writer) int {
	target := "http://127.0.0.1:8200/healthz"
	if len(args) > 0 {
		target = args[0]
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		_, _ = fmt.Fprintf(stderr, "healthcheck: only http://127.0.0.1 or http://localhost URLs are allowed, got %q\n", target)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil) //nolint:gosec // G704: loopback-only URL, validated above
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is validated above to be loopback-only
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "healthcheck: %s returned %d\n", target, resp.StatusCode)
		return 1
	}
	return 0
}
