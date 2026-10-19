package redact_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

func TestHandlerRedactsLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(redact.NewHandler(slog.NewJSONHandler(&buf, nil), redact.New()))
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJhZ2VudCJ9.c2lnbmF0dXJlLXZhbHVl"
	logger.With("authorization", "Bearer "+jwt).Info("user jo@example.com paid with 4111 1111 1111 1111",
		"token", "anything", "note", "call +1 202 555 0143", "err", errors.New("bad header Bearer "+jwt),
		slog.Group("req", "email", "x@example.org"))
	out := buf.String()
	for _, leaked := range []string{jwt, "jo@example.com", "4111 1111 1111 1111", "202 555 0143", "x@example.org", "anything"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("log leaks %q: %s", leaked, out)
		}
	}
	for _, marker := range []string{"[REDACTED:EMAIL]", "[REDACTED:CARD]", "[REDACTED:PHONE]", "[REDACTED:SECRET]", "[REDACTED:TOKEN]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %s in %s", marker, out)
		}
	}
}
