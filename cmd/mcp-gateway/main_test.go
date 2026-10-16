package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(%v) exit code = %d, stderr = %q", args, code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "mcp-gateway ") {
			t.Fatalf("run(%v) stdout = %q, want prefix %q", args, stdout.String(), "mcp-gateway ")
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}
