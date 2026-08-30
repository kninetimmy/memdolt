// Package mcpserver implements memdolt's MCP protocol runtime (PRD §11.1).
package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
)

const (
	callToolMethod        = "tools/call"
	modernProtocolVersion = "2026-07-28"

	// toolsListTTLMs is one day. The tool set is fixed before a stdio session
	// starts, so clients can avoid polling it during a normal coding session.
	toolsListTTLMs = 24 * 60 * 60 * 1000
)

// Instructions is the versioned policy sent by both server/discover and the
// legacy initialize response.
//
//go:embed instructions.md
var Instructions string

var unknownActor = memory.Actor{Name: memory.AgentPrefix + "unknown"}

// New constructs the protocol server. Tools are added by the application
// before the server is connected; their set is static for the session.
func New(version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "memdolt", Version: version}, &mcp.ServerOptions{
		Instructions: Instructions,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	server.AddReceivingMiddleware(attributionMiddleware, toolsListCacheMiddleware)
	return server
}

// ActorFromContext returns the identity derived for this tool request.
// Requests outside New's tools/call middleware fail closed to agent:unknown.
func ActorFromContext(ctx context.Context) memory.Actor {
	actor, ok := ctx.Value(actorContextKey{}).(memory.Actor)
	if !ok {
		return unknownActor
	}
	return actor
}

type actorContextKey struct{}

func attributionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != callToolMethod {
			return next(ctx, method, req)
		}
		toolReq, ok := req.(*mcp.CallToolRequest)
		if !ok {
			return nil, errors.New("mcpserver: tools/call did not carry CallToolRequest")
		}
		actor, err := actorForRequest(toolReq)
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, actorContextKey{}, actor), method, req)
	}
}

func actorForRequest(req *mcp.CallToolRequest) (memory.Actor, error) {
	if req.ProtocolVersion() < modernProtocolVersion {
		return actorFor(req.ClientInfo())
	}
	if req.Params == nil {
		return unknownActor, nil
	}
	raw, ok := req.Params.Meta[mcp.MetaKeyClientInfo]
	if !ok || raw == nil {
		return unknownActor, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return memory.Actor{}, fmt.Errorf("mcpserver: encode per-request clientInfo: %w", err)
	}
	var info mcp.Implementation
	if err := json.Unmarshal(encoded, &info); err != nil {
		return memory.Actor{}, fmt.Errorf("mcpserver: decode per-request clientInfo: %w", err)
	}
	return actorFor(&info)
}

func actorFor(info *mcp.Implementation) (memory.Actor, error) {
	if info == nil || strings.TrimSpace(info.Name) == "" {
		return unknownActor, nil
	}
	raw := strings.TrimSpace(info.Name)
	if strings.EqualFold(raw, "cli") {
		actor, err := memory.NormalizeActor("opencode")
		if err != nil {
			return memory.Actor{}, err
		}
		actor.Raw = raw
		return actor, nil
	}
	actor, err := memory.NormalizeActor(raw)
	if err != nil {
		return memory.Actor{}, fmt.Errorf("mcpserver: invalid clientInfo name: %w", err)
	}
	return actor, nil
}

func toolsListCacheMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil {
			return nil, err
		}
		if method == "tools/list" {
			list, ok := result.(*mcp.ListToolsResult)
			if !ok {
				return nil, errors.New("mcpserver: tools/list did not return ListToolsResult")
			}
			list.TTLMs = toolsListTTLMs
		}
		return result, nil
	}
}
