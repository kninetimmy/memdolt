package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/render"
)

// Render is the session render boundary shared by the MCP tool and runServe's
// authenticated CLI route. Standalone Store.Render has no session queue.
func (t *Toolset) Render(ctx context.Context) (render.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return render.Result{Status: "notes-failed"}, errors.New("mcpserver: session-note batch is closed; render was not published")
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	flushCtx, cancel := context.WithTimeout(ctx, noteFlushTimeout)
	defer cancel()
	err := t.flushLocked(flushCtx)
	t.flushErr = errors.Join(t.flushErr, err)
	err = errors.Join(t.renderFlushErr, err)
	t.renderFlushErr = nil
	if err != nil {
		return render.Result{Status: "notes-failed"}, fmt.Errorf("session notes failed; render was not published; inspect `memdolt note list` and Dolt history before retrying: %w", err)
	}
	return t.store.Render(ctx)
}

func (t *Toolset) render(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, render.Result, error) {
	result, err := t.Render(ctx)
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
