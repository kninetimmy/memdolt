package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func (t *Toolset) repoStatus(ctx context.Context, _ *mcp.CallToolRequest, in localdolt.RepoStatusOptions) (*mcp.CallToolResult, localdolt.RepoStatusReport, error) {
	report, err := t.store.RepoStatus(ctx, in)
	return &mcp.CallToolResult{}, report, err
}
