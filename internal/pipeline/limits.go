package pipeline

import (
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
// the cache is keyed by the definition pointer). Unusable schemas are cached too, as their error.
type schemas struct {
	mu sync.RWMutex
	m  map[*mcp.Tool]schemaEntry
}

type schemaEntry struct {
	r   *jsonschema.Resolved // nil when the tool has no schema or it is unusable
	err error
}

// get returns the resolved schema; fresh reports whether this call resolved it (so an unusable
// schema is logged once per definition, not once per call).
func (s *schemas) get(t catalog.Tool) (e schemaEntry, fresh bool) {
	s.mu.RLock()
	e, ok := s.m[t.Def]
	s.mu.RUnlock()
	if ok {
		return e, false
	}
	e = resolveSchema(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[*mcp.Tool]schemaEntry{}
	}
	if len(s.m) > 4096 { // definitions churn only on catalog refresh; bound the cache anyway
		clear(s.m)
	}
	s.m[t.Def] = e
	return e, true
}

func resolveSchema(t catalog.Tool) schemaEntry {
	if t.Def.InputSchema == nil {
		return schemaEntry{}
	}
	raw, err := json.Marshal(t.Def.InputSchema)
	if err != nil {
		return schemaEntry{err: fmt.Errorf("encode input schema of %s: %w", t.FullName(), err)}
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal(raw, &sch); err != nil {
		return schemaEntry{err: fmt.Errorf("decode input schema of %s: %w", t.FullName(), err)}
	}
	r, err := sch.Resolve(nil)
	if err != nil {
		return schemaEntry{err: fmt.Errorf("resolve input schema of %s: %w", t.FullName(), err)}
	}
	return schemaEntry{r: r}
}

// checkArgs enforces the depth limit and the tool's input schema before any other stage. A tool
// whose schema cannot be used is not validated here (the upstream enforces its own schema); that
// is logged once per tool definition.
func (p *Pipeline) checkArgs(c *call) error {
	if d := argsDepth(c.args); d > p.opts.MaxArgsDepth {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("arguments are nested %d levels deep (limit %d)", d, p.opts.MaxArgsDepth)}
	}
	e, fresh := p.schemas.get(c.tool)
	if e.err != nil && fresh {
		p.deps.Logger.Warn("input schema unusable; arguments not validated by the gateway", "tool", c.tool.FullName(), "error", e.err)
	}
	if e.r == nil {
		return nil
	}
	if err := e.r.Validate(c.argsVal); err != nil {
		msg := strings.TrimSpace(err.Error())
		if len(msg) > 300 {
			msg = strings.ToValidUTF8(msg[:300], "")
		}
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "arguments do not match the input schema of " +
			c.tool.FullName() + ": " + msg}
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
