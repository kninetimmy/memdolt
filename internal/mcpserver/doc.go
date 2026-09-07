package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type docAddInput struct {
	File  string `json:"file" jsonschema:"UTF-8 Markdown file under the repository root or configured doc.allowed_dirs; relative paths start at the repository root"`
	Title string `json:"title,omitempty" jsonschema:"optional title; blank defaults to the first heading or filename"`
}

func (t *Toolset) docAdd(ctx context.Context, _ *mcp.CallToolRequest, in docAddInput) (*mcp.CallToolResult, localdolt.DocResult, error) {
	result, err := t.store.DocAdd(ctx, localdolt.DocAddOptions{
		File: in.File, Title: in.Title, Actor: ActorFromContext(ctx), Confined: true,
	})
	if err != nil {
		if result.Commit == "" {
			return nil, result, err
		}
		// The SDK discards typed output when the Go error is non-nil. Preserve
		// confirmed data plus the visible error in a protocol tool-error result.
		result.Error = err.Error()
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: result.Error}}}, result, nil
	}
	return &mcp.CallToolResult{}, result, nil
}
