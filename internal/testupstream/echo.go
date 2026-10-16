// Package testupstream provides small in-process MCP upstreams for tests and benchmarks.
package testupstream

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EchoInput is the input of the echo tool.
type EchoInput struct {
	Text string `json:"text" jsonschema:"text to echo back"`
}

// EchoServer returns an MCP server named name with one read-only tool, "echo".
func EchoServer(name string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "v0.1.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "Echo the text back.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in EchoInput) (*mcp.CallToolResult, EchoInput, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, in, nil
	})
	return s
}

// EchoHandler serves EchoServer over Streamable HTTP.
func EchoHandler(name string) http.Handler {
	s := EchoServer(name)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
