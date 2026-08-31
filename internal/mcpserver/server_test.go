package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
)

func TestInstructionsCarryVersionedPolicy(t *testing.T) {
	for _, phrase := range []string{
		"turn one",
		"Recall relevant memory before reading `PROJECT_LEDGER.md`",
		"use `locate` before grep",
		"Never write durable facts or decisions directly",
		"existing dotted namespace",
		"Facts state what is true",
		"because,” file it as a decision",
	} {
		if !strings.Contains(Instructions, phrase) {
			t.Errorf("instructions do not contain %q", phrase)
		}
	}
}

func TestModernDiscoveryInstructionsAndToolsCache(t *testing.T) {
	server := New("test-version")
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "test-client", Version: "1"}, false)

	initialized := clientSession.InitializeResult()
	if initialized.ProtocolVersion != modernProtocolVersion {
		t.Fatalf("protocol version = %q, want %q", initialized.ProtocolVersion, modernProtocolVersion)
	}
	if initialized.Instructions != Instructions {
		t.Fatal("server/discover did not return the checked-in instructions")
	}
	if initialized.ServerInfo == nil || initialized.ServerInfo.Name != "memdolt" || initialized.ServerInfo.Version != "test-version" {
		t.Fatalf("server info = %#v, want memdolt test-version", initialized.ServerInfo)
	}

	tools, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools.TTLMs != toolsListTTLMs {
		t.Fatalf("tools/list ttlMs = %d, want %d", tools.TTLMs, toolsListTTLMs)
	}
	if tools.CacheScope != "public" {
		t.Fatalf("tools/list cacheScope = %q, want public", tools.CacheScope)
	}
	if len(tools.Tools) != 0 {
		t.Fatalf("runtime without an application backend advertised %d tools", len(tools.Tools))
	}

	closeSessions(t, clientSession, serverSession)
}

func TestLegacyInitializeUsesSessionClientInfo(t *testing.T) {
	server, actors := identityServer()
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "Legacy Client", Version: "1"}, true)

	initialized := clientSession.InitializeResult()
	if initialized.ProtocolVersion == modernProtocolVersion {
		t.Fatalf("legacy connection negotiated modern protocol %q", initialized.ProtocolVersion)
	}
	if initialized.Instructions != Instructions {
		t.Fatal("legacy initialize did not return the checked-in instructions")
	}
	callIdentity(t, clientSession, nil)
	if got := <-actors; got != (memory.Actor{Name: "agent:legacy-client", Raw: "Legacy Client"}) {
		t.Fatalf("legacy actor = %#v", got)
	}

	closeSessions(t, clientSession, serverSession)
}

func TestPerRequestAttributionChangesWithinOneConnection(t *testing.T) {
	server, actors := identityServer()
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "session-client", Version: "1"}, false, omitClientInfoMiddleware)

	callIdentity(t, clientSession, &mcp.Implementation{Name: "Claude Code", Version: "1"})
	callIdentity(t, clientSession, &mcp.Implementation{Name: "Codex", Version: "1"})
	callIdentityMeta(t, clientSession, mcp.Meta{omitClientInfoKey: true})

	if got := <-actors; got != (memory.Actor{Name: "agent:claude-code", Raw: "Claude Code"}) {
		t.Fatalf("first request actor = %#v", got)
	}
	if got := <-actors; got != (memory.Actor{Name: "agent:codex", Raw: "Codex"}) {
		t.Fatalf("second request actor = %#v", got)
	}
	if got := <-actors; got != unknownActor {
		t.Fatalf("request without clientInfo actor = %#v, want %#v", got, unknownActor)
	}

	closeSessions(t, clientSession, serverSession)
}

func TestModernUserIdentityCannotClaimHumanActor(t *testing.T) {
	server, actors := identityServer()
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "session-client", Version: "1"}, false)

	for _, info := range []*mcp.Implementation{
		{Name: "user", Version: "1"},
		{Name: "User", Version: "1"},
		{Name: "agent:user", Version: "1"},
		{Name: "agent:User", Version: "1"},
	} {
		callIdentity(t, clientSession, info)
		if got := <-actors; got != (memory.Actor{Name: "agent:user", Raw: info.Name}) {
			t.Fatalf("modern actor for %q = %#v", info.Name, got)
		}
	}

	closeSessions(t, clientSession, serverSession)
}

func TestLegacyUserIdentityCannotClaimHumanActor(t *testing.T) {
	server, actors := identityServer()
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "user", Version: "1"}, true)

	callIdentity(t, clientSession, nil)
	if got := <-actors; got != (memory.Actor{Name: "agent:user", Raw: "user"}) {
		t.Fatalf("legacy user actor = %#v", got)
	}

	closeSessions(t, clientSession, serverSession)
}

func TestMCPIdentityRetainsNormalizationValidation(t *testing.T) {
	for _, raw := range []string{
		"agent:???",
		strings.Repeat("a", 60),
		strings.Repeat("verbose", 40),
	} {
		if actor, err := actorFor(&mcp.Implementation{Name: raw}); err == nil {
			t.Errorf("actorFor(%q) = %+v, want an error", raw, actor)
		}
	}
}

func TestMissingIdentityFailsClosedAndOpenCodeKeepsRawName(t *testing.T) {
	server, actors := identityServer()
	clientSession, serverSession := connect(t, server,
		&mcp.Implementation{Name: "", Version: "1"}, false)

	callIdentity(t, clientSession, nil)
	callIdentity(t, clientSession, &mcp.Implementation{Name: "cli", Version: "1"})

	if got := <-actors; got != unknownActor {
		t.Fatalf("missing identity actor = %#v, want %#v", got, unknownActor)
	}
	if got := <-actors; got != (memory.Actor{Name: "agent:opencode", Raw: "cli"}) {
		t.Fatalf("OpenCode actor = %#v", got)
	}

	closeSessions(t, clientSession, serverSession)
}

func identityServer() (*mcp.Server, <-chan memory.Actor) {
	server := New("test")
	actors := make(chan memory.Actor, 4)
	mcp.AddTool(server, &mcp.Tool{Name: "identity"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			actors <- ActorFromContext(ctx)
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	return server, actors
}

func callIdentity(t *testing.T, session *mcp.ClientSession, info *mcp.Implementation) {
	t.Helper()
	var meta mcp.Meta
	if info != nil {
		meta = mcp.Meta{mcp.MetaKeyClientInfo: info}
	}
	callIdentityMeta(t, session, meta)
}

func callIdentityMeta(t *testing.T, session *mcp.ClientSession, meta mcp.Meta) {
	t.Helper()
	params := &mcp.CallToolParams{Name: "identity", Meta: meta}
	if _, err := session.CallTool(context.Background(), params); err != nil {
		t.Fatal(err)
	}
}

func connect(
	t *testing.T,
	server *mcp.Server,
	clientInfo *mcp.Implementation,
	legacy bool,
	clientMiddleware ...mcp.Middleware,
) (*mcp.ClientSession, *mcp.ServerSession) {
	return connectWithOptions(t, server, clientInfo, legacy, nil, clientMiddleware...)
}

func connectWithOptions(
	t *testing.T,
	server *mcp.Server,
	clientInfo *mcp.Implementation,
	legacy bool,
	options *mcp.ClientOptions,
	clientMiddleware ...mcp.Middleware,
) (*mcp.ClientSession, *mcp.ServerSession) {
	t.Helper()
	ctx := context.Background()
	clientSide, serverSide := mcp.NewInMemoryTransports()
	if legacy {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "server/discover" {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "legacy client"}
				}
				return next(ctx, method, req)
			}
		})
	}
	serverSession, err := server.Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(clientInfo, options)
	client.AddSendingMiddleware(clientMiddleware...)
	clientSession, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatal(err)
	}
	return clientSession, serverSession
}

const omitClientInfoKey = "test.memdolt/omit-client-info"

func omitClientInfoMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		meta := req.GetParams().GetMeta()
		if meta[omitClientInfoKey] == true {
			delete(meta, omitClientInfoKey)
			delete(meta, mcp.MetaKeyClientInfo)
		}
		return next(ctx, method, req)
	}
}

func closeSessions(t *testing.T, client *mcp.ClientSession, server *mcp.ServerSession) {
	t.Helper()
	if err := client.Close(); err != nil {
		t.Error(err)
	}
	if err := server.Wait(); err != nil {
		t.Error(err)
	}
}
