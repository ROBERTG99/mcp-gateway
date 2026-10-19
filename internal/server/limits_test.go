package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBodyLimit (NFR-9): MCP request bodies above 1 MiB are refused with 413, whether the length
// is declared or streamed.
func TestBodyLimit(t *testing.T) {
	var got int
	h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		got = len(b)
	}), 1<<20)
	for _, tc := range []struct {
		name    string
		size    int
		chunked bool
		want    int
	}{{"at limit", 1 << 20, false, 200}, {"declared over", 1<<20 + 1, false, 413}, {"streamed over", 1<<20 + 1, true, 413}} {
		var body io.Reader = bytes.NewReader([]byte(strings.Repeat("a", tc.size)))
		if tc.chunked {
			body = io.MultiReader(body) // hides the length: the request is sent chunked
		}
		req := httptest.NewRequest(http.MethodPost, "/mcp", body)
		if tc.chunked {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	if got != 1<<20 {
		t.Fatalf("body at the limit read %d bytes", got)
	}
}
