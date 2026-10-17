package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
)

// NFR-9 limits (the request body limit, 1 MiB, is enforced by the HTTP handler).
const (
	DefaultMaxArgsDepth   = 32
	DefaultMaxResultBytes = 4 << 20
)

// argsDepth returns the nesting depth of a JSON document (a scalar is 0, {} or [] is 1).
func argsDepth(raw json.RawMessage) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, b := range raw {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case b == '"':
			inString = true
		case b == '{' || b == '[':
			depth++
			deepest = max(deepest, depth)
		case b == '}' || b == ']':
			depth--
		}
	}
	return deepest
}

// schemas caches each tool's resolved input schema (tools are re-fetched on catalog refresh, so
// the cache is keyed by the definition pointer).
type schemas struct {
	mu sync.Mutex
	m  map[*mcp.Tool]*jsonschema.Resolved
}

func (s *schemas) get(t catalog.Tool) (*jsonschema.Resolved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.m[t.Def]; ok {
		return r, nil
	}
	var r *jsonschema.Resolved
	if t.Def.InputSchema != nil {
		raw, err := json.Marshal(t.Def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("encode input schema of %s: %w", t.FullName(), err)
		}
		var sch jsonschema.Schema
		if err := json.Unmarshal(raw, &sch); err != nil {
			return nil, fmt.Errorf("decode input schema of %s: %w", t.FullName(), err)
		}
		if r, err = sch.Resolve(nil); err != nil {
			return nil, fmt.Errorf("resolve input schema of %s: %w", t.FullName(), err)
		}
	}
	if s.m == nil {
		s.m = map[*mcp.Tool]*jsonschema.Resolved{}
	}
	if len(s.m) > 4096 { // definitions churn only on catalog refresh; bound the cache anyway
		clear(s.m)
	}
	s.m[t.Def] = r
	return r, nil
}

// checkArgs enforces the depth limit and the tool's input schema before any other stage.
func (p *Pipeline) checkArgs(c *call) error {
	if d := argsDepth(c.args); d > p.opts.MaxArgsDepth {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("arguments are nested %d levels deep (limit %d)", d, p.opts.MaxArgsDepth)}
	}
	r, err := p.schemas.get(c.tool)
	if err != nil || r == nil {
		return nil //nolint:nilerr // an unusable upstream schema is the upstream's to enforce
	}
	var instance any
	dec := json.NewDecoder(bytes.NewReader(c.args))
	if err := dec.Decode(&instance); err != nil {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments are not valid JSON"}
	}
	if err := r.Validate(instance); err != nil {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments do not match the input schema of " +
			c.tool.FullName() + ": " + strings.TrimSpace(msg)}
	}
	return nil
}

// checkResult refuses upstream results above MaxResultBytes (NFR-9).
func (p *Pipeline) checkResult(c *call, res *mcp.CallToolResult) error {
	raw, err := json.Marshal(res)
	if err != nil || len(raw) <= p.opts.MaxResultBytes {
		return nil //nolint:nilerr // an unencodable result fails later, when the SDK encodes it
	}
	return mcperr.New(mcperr.CodeUpstreamUnavailable, map[string]any{"server": c.tool.Server, "reason": "result_too_large",
		"limit_bytes": p.opts.MaxResultBytes})
}
