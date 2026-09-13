package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/kninetimmy/memdolt/internal/search"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFileHistoryMCPUsesCacheWithoutDoltGitModelsOrSourceBodies(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "modern", true: "legacy"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			path := "雪 file.txt"
			if err := os.WriteFile(filepath.Join(root, path), []byte("body"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", path}, {"-c", "user.name=History Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "File history subject"}} {
				if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("fixture Git: %s %v", out, err)
				}
			}
			if _, err := codeindex.IngestGit(context.Background(), root, nil); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".memdolt", "config.toml"), []byte("[retrieval]\nmode='hybrid'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, path)); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", t.TempDir())
			server := New("test")
			// nil deliberately panics if a file query reaches any memory operation.
			tools := RegisterTools(server, root, nil)
			client, session := connect(t, server, &mcp.Implementation{Name: "History Client", Version: "1"}, legacy)
			defer closeSessions(t, client, session)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			listed, err := client.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range listed.Tools {
				if tool.Name == "search" {
					properties := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
					if len(properties) != 2 || properties["query"] == nil || properties["limit"] == nil {
						t.Fatalf("search input changed: %+v", properties)
					}
				}
			}
			explicit := callAs[search.Response](t, client, "search", map[string]any{"query": "file:" + path})
			fallback := callAs[search.Response](t, client, "search", map[string]any{"query": path})
			if !reflect.DeepEqual(explicit, fallback) || explicit.Matcher != "exact:file-history" || len(explicit.Results) != 1 || explicit.Results[0].FileHistoryHit == nil || explicit.Results[0].Path != path || explicit.Coverage == nil || explicit.Coverage.Ranges != 1 {
				t.Fatalf("MCP file history explicit=%+v fallback=%+v", explicit, fallback)
			}
			raw, err := json.Marshal(explicit.Results[0])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 7 || fields["type"] != "file_history" || fields["commitSha"] == "" || fields["subject"] != "File history subject" || fields["decisionId"] != nil {
				t.Fatalf("file output fields=%+v", fields)
			}
			missing := callAs[search.Response](t, client, "search", map[string]any{"query": "file:missing.txt"})
			if len(missing.Results) != 0 || missing.Coverage == nil || !missing.Coverage.Cached {
				t.Fatalf("missing cached file=%+v", missing)
			}
			for _, name := range []string{"dolt", "embeddings.sqlite", "models", "LOCK", "server.pid"} {
				if _, err := os.Stat(filepath.Join(root, ".memdolt", name)); !os.IsNotExist(err) {
					t.Fatalf("file query touched %s: %v", name, err)
				}
			}
		})
	}
	t.Run("missing configured root", func(t *testing.T) {
		server := New("test")
		tools := RegisterTools(server, "", nil)
		client, session := connect(t, server, &mcp.Implementation{Name: "History Client", Version: "1"}, false)
		defer closeSessions(t, client, session)
		defer func() { _ = tools.Close() }()
		callError(t, client, "search", map[string]any{"query": "file:README.md"}, "configured code repository root")
	})
}
