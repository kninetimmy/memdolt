package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type documentBackend struct {
	Backend
	seen localdolt.DocAddOptions
	late bool
}

func (s *documentBackend) DocAdd(ctx context.Context, opts localdolt.DocAddOptions) (localdolt.DocResult, error) {
	s.seen = opts
	result, err := s.Backend.DocAdd(ctx, opts)
	if err == nil && s.late {
		err = errors.New("synthetic confirmed document finalization failure")
	}
	return result, err
}

func writeMCPDocument(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentMCPPathsSchemasAndModernLegacyAttribution(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			base, st := initializedToolStore(t)
			backend := &documentBackend{Backend: testElicitationBackend(base, st)}
			server := New("test")
			tools := RegisterTools(server, base, backend)
			t.Cleanup(func() { _ = tools.Close() })
			client, session := connect(t, server, &mcp.Implementation{Name: "cli", Version: "1"}, legacy)
			defer closeSessions(t, client, session)
			writeMCPDocument(t, filepath.Join(base, "inside.md"), "# Inside\n\n## 子\n\nAllowed content\n")
			added := callAs[localdolt.DocResult](t, client, "doc_add", map[string]any{"file": "inside.md"})
			if added.Status != "created" || added.Document.Source != "user" || !added.EnabledDefaultRecall || !backend.seen.Confined || backend.seen.Actor.Name != "agent:opencode" || backend.seen.Actor.Raw != "cli" {
				t.Fatalf("MCP source/caller result=%+v seen=%+v", added, backend.seen)
			}
			if author := testText(t, st, "SELECT committer FROM dolt_log WHERE commit_hash = ?", added.Commit); author != "agent:opencode" {
				t.Fatalf("document commit author = %s", author)
			}
			rendered := callAs[render.Result](t, client, "render", map[string]any{})
			if rendered.Status != "written" || rendered.SourceCommit != added.Commit {
				t.Fatalf("render did not capture the ingested document commit: %+v", rendered)
			}
			for _, local := range []bool{true, false} {
				status := callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{"local": local})
				want := "no-remote"
				if local {
					want = "offline"
				}
				if status.Status != want || status.MainCommit != added.Commit || !status.Clean {
					t.Fatalf("MCP document repository status local=%t: %+v", local, status)
				}
			}
			shown, err := st.DocShow(context.Background(), added.Document.ID)
			if err != nil || !reflect.DeepEqual(shown.Document, added.Document) || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
				t.Fatalf("MCP render changed document metadata/chunks: %+v, %v", shown, err)
			}
			again := callAs[localdolt.DocResult](t, client, "doc_add", map[string]any{"file": "inside.md", "title": "ignored unchanged title"})
			if again.Status != "unchanged" || again.Document.ID != added.Document.ID || again.Document.Title != "Inside" {
				t.Fatalf("unchanged = %+v", again)
			}
			outside := t.TempDir()
			external := filepath.Join(outside, "outside.md")
			writeMCPDocument(t, external, "# Outside\n")
			callError(t, client, "doc_add", map[string]any{"file": external}, "outside")
			traversal, err := filepath.Rel(base, external)
			if err != nil {
				t.Fatal(err)
			}
			callError(t, client, "doc_add", map[string]any{"file": traversal}, "outside")
			configPath := filepath.Join(base, ".memdolt", "config.toml")
			writeMCPDocument(t, configPath, fmt.Sprintf("[doc]\nallowed_dirs=[%q]\n", filepath.ToSlash(outside)))
			allowed := callAs[localdolt.DocResult](t, client, "doc_add", map[string]any{"file": external})
			if allowed.Status != "created" || allowed.EnabledDefaultRecall || allowed.Document.Path != external {
				t.Fatalf("allow-list ingestion = %+v", allowed)
			}
			for _, args := range []map[string]any{
				{"file": ""}, {"file": "\x00"}, {"file": "missing.md"}, {"file": base},
				{"file": "inside.md", "title": strings.Repeat("x", 513)},
			} {
				callError(t, client, "doc_add", args, "document")
			}
			for _, args := range []map[string]any{{"file": 42}, {"file": "inside.md", "global": true}, {"file": "inside.md", "confined": false}, {"file": "inside.md", "actor": "user"}} {
				result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "doc_add", Arguments: args})
				if err == nil && !result.IsError {
					t.Fatalf("invalid schema input accepted: %+v", args)
				}
			}
			writeMCPDocument(t, configPath, "[doc]\nallowed_dirs='not an array'\n")
			callError(t, client, "doc_add", map[string]any{"file": "inside.md"}, "configuration")
			writeMCPDocument(t, configPath, "[deny_list]\npatterns=['forbidden']\n")
			callError(t, client, "doc_add", map[string]any{"file": "inside.md", "title": "forbidden"}, "deny-list")
			writeMCPDocument(t, filepath.Join(base, "forbidden.md"), "body")
			callError(t, client, "doc_add", map[string]any{"file": "forbidden.md"}, "deny-list")
			writeMCPDocument(t, filepath.Join(base, "inside.md"), "forbidden content")
			callError(t, client, "doc_add", map[string]any{"file": "inside.md"}, "deny-list")
			if count := testCount(t, st, "SELECT COUNT(*) FROM documents AS OF 'main'"); count != 2 {
				t.Fatalf("refusals ingested rows: %d", count)
			}
		})
	}
}

func TestDocumentMCPSymlinkEscapeAndConfirmedError(t *testing.T) {
	base, st := initializedToolStore(t)
	backend := &documentBackend{Backend: testElicitationBackend(base, st)}
	server := New("test")
	tools := RegisterTools(server, base, backend)
	t.Cleanup(func() { _ = tools.Close() })
	client, session := connect(t, server, &mcp.Implementation{Name: "user", Version: "1"}, false)
	defer closeSessions(t, client, session)
	file := filepath.Join(base, "durable.md")
	writeMCPDocument(t, file, "# Durable\n")
	backend.late = true
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "doc_add", Arguments: map[string]any{"file": file}})
	if err != nil || !result.IsError || result.StructuredContent == nil {
		t.Fatalf("confirmed tool-error lost its structure: %+v, %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var confirmed localdolt.DocResult
	if err := json.Unmarshal(raw, &confirmed); err != nil || confirmed.Status != "created" || confirmed.Commit == "" || !strings.Contains(confirmed.Error, "confirmed document") {
		t.Fatalf("confirmed result = %s, %v", raw, err)
	}
	if backend.seen.Actor.Name != "agent:user" || testText(t, st, "SELECT committer FROM dolt_log WHERE commit_hash = ?", confirmed.Commit) != "agent:user" {
		t.Fatal("document source granted human provenance")
	}
	backend.late = false
	t.Run("symlinks", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.md")
		writeMCPDocument(t, outside, "# Never ingest this target\n")
		link := filepath.Join(base, "escape.md")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("platform cannot create fixture symlink: %v", err)
		}
		callError(t, client, "doc_add", map[string]any{"file": link}, "outside")
		insideLink := filepath.Join(base, "alias.md")
		if err := os.Symlink(file, insideLink); err != nil {
			t.Fatal(err)
		}
		alias := callAs[localdolt.DocResult](t, client, "doc_add", map[string]any{"file": insideLink})
		if alias.Status != "unchanged" || alias.Document.ID != confirmed.Document.ID {
			t.Fatalf("canonical alias duplicated document identity: %+v", alias)
		}
		config := filepath.Join(base, ".memdolt", "config.toml")
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, config); err != nil {
			t.Fatal(err)
		}
		callError(t, client, "doc_add", map[string]any{"file": file}, "configuration must be a regular file")
	})
}
