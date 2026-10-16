// Package mcperr defines the gateway's JSON-RPC error codes (spec §4.4) and constructors.
package mcperr

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// JSON-RPC error codes of the mcp-gateway v0.1.0 contract.
const (
	CodeIdempotencyConflict    int64 = -32040
	CodeIdempotencyKeyRequired int64 = -32041
	CodeRateLimited            int64 = -32042
	CodeBudgetExhausted        int64 = -32043
	CodePolicyDenied           int64 = -32044
	CodeApprovalInvalid        int64 = -32045
	CodeUpstreamUnavailable    int64 = -32046
	CodeIdempotencyInProgress  int64 = -32047
	CodeAuditBackpressure      int64 = -32048
	CodeDependencyUnavailable  int64 = -32049
	CodeInvalidParams          int64 = jsonrpc.CodeInvalidParams
)

// Messages are the stable snake_case names of each code.
var messages = map[int64]string{
	CodeIdempotencyConflict:    "idempotency_conflict",
	CodeIdempotencyKeyRequired: "idempotency_key_required",
	CodeRateLimited:            "rate_limited",
	CodeBudgetExhausted:        "budget_exhausted",
	CodePolicyDenied:           "policy_denied",
	CodeApprovalInvalid:        "approval_invalid",
	CodeUpstreamUnavailable:    "upstream_unavailable",
	CodeIdempotencyInProgress:  "idempotency_in_progress",
	CodeAuditBackpressure:      "audit_backpressure",
	CodeDependencyUnavailable:  "dependency_unavailable",
}

// Message returns the stable name of a code ("" for unknown codes).
func Message(code int64) string { return messages[code] }

// New builds a JSON-RPC error with the code's stable message and optional data.
func New(code int64, data any) *jsonrpc.Error {
	e := &jsonrpc.Error{Code: code, Message: messages[code]}
	if data != nil {
		if raw, err := json.Marshal(data); err == nil {
			e.Data = raw
		}
	}
	return e
}

// UnknownTool is the error for a tool that does not exist or is hidden from the caller.
// It is byte-for-byte the SDK's own unknown-tool error so hidden tools are indistinguishable.
func UnknownTool(name string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "unknown tool \"" + name + "\""}
}

// Code extracts the JSON-RPC code from err, or 0 when err is not a JSON-RPC error.
func Code(err error) int64 {
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return je.Code
	}
	return 0
}
