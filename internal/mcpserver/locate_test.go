package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocateMCPModernAndLegacyUseOnlyLocalTypedBackend(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "modern", true: "legacy"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package a\n// The bounded canary.\nfunc Canary() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", "source.go"}} {
				if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			server := New("test")
			// nil deliberately panics if locate ever calls the Dolt backend.
			tools := RegisterTools(server, root, nil)
			client, session := connect(t, server, &mcp.Implementation{Name: "Code Client", Version: "1"}, legacy)
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
				if tool.Name != "locate" {
					continue
				}
				if tool.InputSchema == nil || tool.OutputSchema == nil {
					t.Fatal("locate schemas are missing")
				}
				schema := tool.InputSchema.(map[string]any)
				properties := schema["properties"].(map[string]any)
				for _, forbidden := range []string{"dir", "path", "sql", "no_refresh", "noRefresh", "min_rerank_score"} {
					if _, ok := properties[forbidden]; ok {
						t.Errorf("locate exposes %s", forbidden)
					}
				}
			}
			response := callAs[codeindex.Response](t, client, "locate", map[string]any{"query": "Canary", "limit": 1})
			if response.NoRefresh || response.Refresh == nil || len(response.Results) != 1 || response.Results[0].Kind != "function" || *response.Results[0].Symbol != "Canary" || !strings.Contains(response.Results[0].Snippet, "bounded canary") {
				t.Fatalf("typed locate=%+v", response)
			}
			bad, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "locate", Arguments: map[string]any{"query": "Canary", "limit": -1}})
			if err == nil && !bad.IsError {
				t.Fatal("negative limit was accepted")
			}
			for _, name := range []string{"dolt", "embeddings.sqlite", "LOCK", "server.pid"} {
				if _, err := os.Stat(filepath.Join(root, ".memdolt", name)); !os.IsNotExist(err) {
					t.Fatalf("locate touched %s: %v", name, err)
				}
			}
		})
	}
}
