package mcpserver

import (
	"context"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type locateInput struct {
	Query  string `json:"query" jsonschema:"plain-language code intent"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum breadcrumbs; zero uses ten"`
	Rerank bool   `json:"rerank,omitempty" jsonschema:"optional local cross-encoder (hybrid only); no score floor"`
}

func (t *Toolset) locate(ctx context.Context, _ *mcp.CallToolRequest, in locateInput) (*mcp.CallToolResult, codeindex.Response, error) {
	response, err := codeindex.Locate(ctx, t.baseDir, nil, codeindex.Options{Query: in.Query, Limit: in.Limit, UseReranker: in.Rerank})
	return &mcp.CallToolResult{}, response, err
}
