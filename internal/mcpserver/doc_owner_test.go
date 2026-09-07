package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDocumentOwnerMetadataRefusedBeforeExposureOrDurability(t *testing.T) {
	for _, configuration := range []string{"absent", "empty"} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", configuration, legacy), func(t *testing.T) {
				base, st := initializedToolStore(t)
				pidfile := filepath.Join(base, ".memdolt", "server.pid")
				if _, err := os.Lstat(pidfile); !os.IsNotExist(err) {
					t.Fatal("disposable fixture unexpectedly contains owner metadata")
				}
				const synthetic = "syntheticownermetadataprobe"
				writeMCPDocument(t, pidfile, `{"pid":1,"host":"synthetic","id":"probe","detail":{"port":1,"token":"`+synthetic+`"}}`)
				var expectedConfig []byte
				if configuration == "empty" {
					expectedConfig = []byte("[deny_list]\npatterns=[]\n")
					writeMCPDocument(t, filepath.Join(base, ".memdolt", "config.toml"), string(expectedConfig))
				}
				server := New("security-regression")
				tools := RegisterTools(server, base, testElicitationBackend(base, st))
				t.Cleanup(func() { _ = tools.Close() })
				client, session := connect(t, server, &mcp.Implementation{Name: "security-regression", Version: "1"}, legacy)
				defer closeSessions(t, client, session)
				before := testCount(t, st, "SELECT COUNT(*) FROM dolt_log")
				refused := func(t *testing.T, path string) {
					t.Helper()
					result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "doc_add", Arguments: map[string]any{"file": path}})
					if err != nil {
						t.Fatal("protected metadata refusal was not a visible tool error")
					}
					encoded, err := json.Marshal(result)
					if err != nil || bytes.Contains(encoded, []byte(synthetic)) {
						t.Fatal("owner metadata reached the tool response")
					}
					if !result.IsError || !strings.Contains(string(encoded), "protected memdolt owner metadata") {
						t.Fatalf("owner metadata was not refused by the dedicated protection: %s", encoded)
					}
					if testCount(t, st, "SELECT COUNT(*) FROM documents") != 0 || testCount(t, st, "SELECT COUNT(*) FROM doc_chunks") != 0 || testCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || testCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
						t.Fatal("protected metadata refusal changed durable or working memory")
					}
					config, err := os.ReadFile(filepath.Join(base, ".memdolt", "config.toml"))
					if expectedConfig == nil {
						if !os.IsNotExist(err) {
							t.Fatal("protected metadata refusal created configuration")
						}
					} else if err != nil || !bytes.Equal(config, expectedConfig) {
						t.Fatal("protected metadata refusal changed configuration")
					}
				}
				refused(t, ".memdolt/server.pid")
				refused(t, pidfile)
				refused(t, ".memdolt/../.memdolt/server.pid")
				for _, alias := range []string{"symlink", "hardlink"} {
					t.Run(alias, func(t *testing.T) {
						path := filepath.Join(base, alias+".md")
						link := os.Symlink
						if alias == "hardlink" {
							link = os.Link
						}
						if err := link(pidfile, path); err != nil {
							t.Skipf("platform cannot create fixture alias: %v", err)
						}
						refused(t, path)
					})
				}
				t.Run("allowed external hardlink", func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "alias.md")
					if err := os.Link(pidfile, path); err != nil {
						t.Skipf("platform cannot create fixture hard link: %v", err)
					}
					expectedConfig = append(expectedConfig, []byte(fmt.Sprintf("\n[doc]\nallowed_dirs=[%q]\n", filepath.ToSlash(dir)))...)
					writeMCPDocument(t, filepath.Join(base, ".memdolt", "config.toml"), string(expectedConfig))
					refused(t, path)
				})
			})
		}
	}
}
