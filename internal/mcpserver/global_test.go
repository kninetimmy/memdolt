package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestGlobalMCPRecallUsesCombinedScopesAndRefusesCompetingOwner(t *testing.T) {
	ctx := context.Background()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base, st := initializedToolStore(t)
	if _, err := st.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "scope.same", Value: "scopebeacon repo"}, Source: "user", Actor: memory.UserActor}); err != nil {
		t.Fatal(err)
	}
	if _, err := localdolt.SetGlobalEnabled(base, true); err != nil {
		t.Fatal(err)
	}
	paths, err := localdolt.GlobalPaths()
	if err != nil {
		t.Fatal(err)
	}
	global, err := localdolt.New(localdolt.Config{BaseDir: paths.Base(), Actor: memory.UserActor.CommitAuthor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := global.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := global.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := global.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "scope.same", Value: "scopebeacon global"}, Source: "user", Actor: memory.UserActor}); err != nil {
		t.Fatal(err)
	}
	server := New("test")
	tools := RegisterTools(server, base, testElicitationBackend(base, st))
	client, session := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "test"}, false)
	callError(t, client, "recall", map[string]any{"query": "scopebeacon"}, "current operation/owner")
	if err := global.Close(); err != nil {
		t.Fatal(err)
	}
	result := callAs[retrieval.Response](t, client, "recall", map[string]any{"query": "scopebeacon", "provenance": true})
	if result.ReturnedCount != 2 || result.Results[0].Scope == result.Results[1].Scope {
		t.Fatal(result)
	}
	for _, hit := range result.Results {
		if hit.LastChanged == nil || hit.SnapshotCommit == "" {
			t.Fatal(hit)
		}
	}
	if _, err := localdolt.SetGlobalEnabled(base, false); err != nil {
		t.Fatal(err)
	}
	disabled := callAs[retrieval.Response](t, client, "recall", map[string]any{"query": "scopebeacon"})
	if disabled.ReturnedCount != 1 || disabled.Results[0].Scope != "" {
		t.Fatal(disabled)
	}
	closeSessions(t, client, session)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}
