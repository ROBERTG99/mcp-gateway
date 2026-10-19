package pipeline_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
)

func nested(depth int) json.RawMessage {
	return json.RawMessage(`{"a":` + strings.Repeat(`[`, depth-1) + strings.Repeat(`]`, depth-1) + `}`)
}

// schemaTool has an input schema like the ones the go-sdk derives for the example upstreams.
var schemaTool = catalog.Tool{Server: "orders", Name: "get_order", Def: &mcp.Tool{Name: "orders.get_order",
	Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	InputSchema: map[string]any{"type": "object", "required": []any{"order_id"}, "additionalProperties": false,
		"properties": map[string]any{"order_id": map[string]any{"type": "string", "maxLength": 32}}}}}

// TestLimits (NFR-9): argument nesting deeper than 32 levels, arguments that violate the tool's
// input schema and upstream results above 4 MiB are refused before (or instead of) reaching the
// agent; results above 256 KiB are not stored for replay but are still returned.
func TestLimits(t *testing.T) {
	big := strings.Repeat("x", 5<<20)
	up := &fakeUpstream{result: func(tool catalog.Tool, args json.RawMessage) (*mcp.CallToolResult, error) {
		switch {
		case strings.Contains(string(args), "huge"):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: big}}}, nil
		case strings.Contains(string(args), "medium"):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("y", 300<<10)}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}}
	idem := idempotency.NewMemory(clock.Real{})
	p := pipeline.New(pipeline.Deps{Upstream: up, Policy: allowAll{}, Idempotency: idem}, pipeline.Options{})

	// Depth: 32 levels pass, 33 are refused before the upstream.
	if _, err := call(p, readTool, nested(32), nil); err != nil {
		t.Fatalf("depth 32: %v", err)
	}
	before := up.calls.Load()
	if _, err := call(p, readTool, nested(33), nil); code(t, err) != jsonrpc.CodeInvalidParams {
		t.Fatalf("depth 33: %v, want -32602", err)
	}
	// Schema: unknown property, wrong type and an over-long value are refused before the upstream.
	for _, args := range []string{`{"order_id":"O-1","extra":1}`, `{"order_id":7}`, `{"order_id":"` + strings.Repeat("9", 40) + `"}`, `{}`} {
		if _, err := call(p, schemaTool, json.RawMessage(args), nil); code(t, err) != jsonrpc.CodeInvalidParams {
			t.Fatalf("schema violation %s: %v, want -32602", args, err)
		}
	}
	if up.calls.Load() != before {
		t.Fatal("an invalid call reached the upstream")
	}
	if _, err := call(p, schemaTool, json.RawMessage(`{"order_id":"O-00042"}`), nil); err != nil {
		t.Fatalf("valid args: %v", err)
	}

	// Result above 4 MiB: refused with upstream_unavailable (reason result_too_large).
	_, err := call(p, refundTool, map[string]any{"order_id": "huge"}, mcp.Meta{"idempotency_key": "limits-key-0001"})
	var je *jsonrpc.Error
	if code(t, err) != mcperr.CodeUpstreamUnavailable || !strings.Contains(errData(err, &je), "result_too_large") {
		t.Fatalf("huge result: %v", err)
	}
	// The write committed upstream: the same key replays via the upstream instead of re-executing
	// at the gateway layer, and is refused the same way.
	_, err = call(p, refundTool, map[string]any{"order_id": "huge"}, mcp.Meta{"idempotency_key": "limits-key-0001"})
	if code(t, err) != mcperr.CodeUpstreamUnavailable {
		t.Fatalf("huge result retry: %v", err)
	}

	// Result between 256 KiB and 4 MiB: returned, but not stored for replay (the retry forwards).
	res, err := call(p, refundTool, map[string]any{"order_id": "medium"}, mcp.Meta{"idempotency_key": "limits-key-0002"})
	if err != nil || res.IsError {
		t.Fatalf("medium result: %v", err)
	}
	n := up.calls.Load()
	res, err = call(p, refundTool, map[string]any{"order_id": "medium"}, mcp.Meta{"idempotency_key": "limits-key-0002"})
	if err != nil || up.calls.Load() != n+1 || gwMeta(res)["idempotent_replay"] == true {
		t.Fatalf("oversized stored result must be forwarded on retry: %v calls %d->%d", err, n, up.calls.Load())
	}
}

func errData(err error, je **jsonrpc.Error) string {
	if !errors.As(err, je) {
		return ""
	}
	return string((*je).Data)
}
