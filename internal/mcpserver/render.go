package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/render"
)

func (t *Toolset) render(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, render.Result, error) {
	result, err := t.store.Render(ctx)
	response := &mcp.CallToolResult{}
	if err != nil {
		result.Error = err.Error()
		response.IsError = true
		response.Content = []mcp.Content{&mcp.TextContent{Text: result.Error}}
	}
	// Keep confirmed file effects in structuredContent on errors too. Returning
	// a Go handler error would let the SDK discard the typed partial result.
	return response, result, nil
}
