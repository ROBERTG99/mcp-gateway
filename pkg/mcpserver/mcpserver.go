// Package mcpserver helps write upstream MCP servers that sit behind mcp-gateway.
//
// ReadTool and WriteTool wrap the official go-sdk's typed tools. Both read the caller context the
// gateway forwards in _meta.mcpgw ({tenant, agent, request_id}); WriteTool additionally requires
// _meta.idempotency_key and executes each (tenant, key) at most once, replaying the stored result
// on retries. This upstream-side dedup is the second idempotency layer: it holds even if the
// gateway crashes after the upstream committed but before the gateway recorded completion.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/pkg/canonicaljson"
)

// Meta keys used by mcp-gateway (contract v0.1.0).
const (
	MetaIdempotencyKey = "idempotency_key"
	MetaGateway        = "mcpgw"
	MetaRunID          = "run_id"
)

// CodeIdempotencyConflict is returned when a key is reused with different arguments.
const CodeIdempotencyConflict int64 = -32040

// ValidKey reports whether key satisfies the idempotency key contract: 8-128 characters of
// [A-Za-z0-9._:-].
func ValidKey(key string) bool {
	if len(key) < 8 || len(key) > 128 {
		return false
	}
	for i := range len(key) {
		switch c := key[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// ErrKeyReused means an idempotency key was reused with different arguments or another tool.
var ErrKeyReused = errors.New("idempotency key reused with different arguments")

// CallInfo is the caller context forwarded by the gateway.
type CallInfo struct {
	Tenant         string
	Agent          string
	RequestID      string
	RunID          string
	IdempotencyKey string
}

// CallInfoFrom extracts the gateway context from a request's _meta.
func CallInfoFrom(req *mcp.CallToolRequest) CallInfo {
	var info CallInfo
	if req == nil || req.Params == nil {
		return info
	}
	m := req.Params.Meta
	if gw, ok := m[MetaGateway].(map[string]any); ok {
		info.Tenant, _ = gw["tenant"].(string)
		info.Agent, _ = gw["agent"].(string)
		info.RequestID, _ = gw["request_id"].(string)
	}
	info.IdempotencyKey, _ = m[MetaIdempotencyKey].(string)
	info.RunID, _ = m[MetaRunID].(string)
	return info
}

// Tx is the transaction a write handler runs in (nil with the memory dedup store).
type Tx = pgx.Tx

// WriteContext is passed to write handlers: the caller context plus the transaction that also
// records the idempotency entry, so the side effect and its dedup record commit atomically.
type WriteContext struct {
	CallInfo
	Tx Tx
}

// DedupStore runs a write at most once per (tenant, key).
type DedupStore interface {
	// Once runs fn unless (tenant, key) already completed, in which case it returns the stored
	// result with replayed=true. A different tool or args hash for an existing key returns
	// ErrKeyReused. When fn fails nothing is stored and the key may be retried.
	Once(ctx context.Context, tenant, key, tool, argsSHA256 string,
		fn func(ctx context.Context, tx Tx) (json.RawMessage, error)) (result json.RawMessage, replayed bool, err error)
}

// Server is an upstream MCP server with a dedup store.
type Server struct {
	mcp   *mcp.Server
	dedup DedupStore
}

// New creates a server.
func New(name, version string, dedup DedupStore) *Server {
	return &Server{mcp: mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil), dedup: dedup}
}

// MCP returns the underlying go-sdk server (to serve it over Streamable HTTP).
func (s *Server) MCP() *mcp.Server { return s.mcp }

func invalidParams(msg string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}

// ReadTool registers a read-only tool (readOnlyHint=true). The tenant is required.
func ReadTool[In, Out any](s *Server, tool *mcp.Tool, h func(ctx context.Context, info CallInfo, in In) (Out, error)) {
	t := *tool
	t.Annotations = withReadOnly(t.Annotations, true)
	mcp.AddTool(s.mcp, &t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		info := CallInfoFrom(req)
		if info.Tenant == "" {
			return nil, zero, invalidParams("missing _meta.mcpgw.tenant")
		}
		out, err := h(ctx, info, in)
		return nil, out, err
	})
}

// WriteTool registers a write tool (readOnlyHint=false) with upstream-side dedup.
func WriteTool[In, Out any](s *Server, tool *mcp.Tool, h func(ctx context.Context, w WriteContext, in In) (Out, error)) {
	t := *tool
	t.Annotations = withReadOnly(t.Annotations, false)
	mcp.AddTool(s.mcp, &t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		info := CallInfoFrom(req)
		if info.Tenant == "" {
			return nil, zero, invalidParams("missing _meta.mcpgw.tenant")
		}
		if !ValidKey(info.IdempotencyKey) {
			return nil, zero, invalidParams("write tools require _meta.idempotency_key (8-128 chars of [A-Za-z0-9._:-])")
		}
		argsHash, err := argsSHA256(req)
		if err != nil {
			return nil, zero, invalidParams("arguments are not valid JSON")
		}
		raw, _, err := s.dedup.Once(ctx, info.Tenant, info.IdempotencyKey, t.Name, argsHash,
			func(ctx context.Context, tx Tx) (json.RawMessage, error) {
				out, err := h(ctx, WriteContext{CallInfo: info, Tx: tx}, in)
				if err != nil {
					return nil, err
				}
				b, err := json.Marshal(out)
				if err != nil {
					return nil, fmt.Errorf("marshal result: %w", err)
				}
				return b, nil
			})
		if errors.Is(err, ErrKeyReused) {
			return nil, zero, &jsonrpc.Error{Code: CodeIdempotencyConflict, Message: "idempotency_conflict"}
		}
		if err != nil {
			return nil, zero, err
		}
		var out Out
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, zero, fmt.Errorf("decode stored result: %w", err)
		}
		return nil, out, nil
	})
}

func argsSHA256(req *mcp.CallToolRequest) (string, error) {
	raw := json.RawMessage("{}")
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		raw = req.Params.Arguments
	}
	h, err := canonicaljson.SHA256Hex(raw)
	if err != nil {
		return "", fmt.Errorf("hash arguments: %w", err)
	}
	return h, nil
}

func withReadOnly(a *mcp.ToolAnnotations, readOnly bool) *mcp.ToolAnnotations {
	out := &mcp.ToolAnnotations{}
	if a != nil {
		c := *a
		out = &c
	}
	out.ReadOnlyHint = readOnly
	return out
}
