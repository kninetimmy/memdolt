package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// Temporary roots can themselves be OS aliases (macOS /var). Fixtures use
// their canonical directory; production bundle paths still refuse links.
func interopTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInteropCLIThroughDirectAndAuthenticatedOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", owner), func(t *testing.T) {
			source, target := initStore(t), initStore(t)
			if owner {
				serveStore(t, source)
				serveStore(t, target)
			}
			runMemdolt(t, "index", "rebuild", "--dir", target)
			seedRender(t, source)
			// Existing document content and local artifacts must survive a fresh
			// memory import, including a real owner credential in the routed case.
			docFile := filepath.Join(interopTempDir(t), "reference.md")
			writeTestFile(t, docFile, "# Retained\n\nExisting target reference document.\n")
			doc := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", docFile, "--dir", target, "--json"))
			configPath := filepath.Join(target, ".memdolt", "config.toml")
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(target, ".memdolt", "local-sentinel"), "retain local artifacts")
			indexPath := filepath.Join(target, ".memdolt", "embeddings.sqlite")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "render", "--dir", target)
			renderPath := filepath.Join(target, ".memdolt", "rendered", "PROJECT.md")
			rendered, err := os.ReadFile(renderPath)
			if err != nil {
				t.Fatal(err)
			}
			before := interopQueryStrings(t, target, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
			file := filepath.Join(interopTempDir(t), "interop.json")
			exported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "export", file, "--dir", source, "--json"))
			if !exported.Written || exported.SourceCommit == "" {
				t.Fatalf("CLI export=%+v", exported)
			}
			imported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", file, "--dir", target, "--json"))
			if imported.Status != "imported" || imported.MainCommit == "" || imported.RetainedDocuments != 1 || imported.RetainedDocChunks != doc.Document.ChunkCount || len(imported.CreatedProposals) != 1 {
				t.Fatalf("CLI import=%+v", imported)
			}
			if after := interopQueryStrings(t, target, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); len(after) != len(before)+1 {
				t.Fatal("CLI fabricated historical commits")
			}
			shown := runMemdolt(t, "review", "show", imported.CreatedProposals[0].ID, "--dir", target, "--json")
			if !strings.Contains(shown, "NEVER_RENDER_PENDING") {
				t.Fatalf("pending import is not reviewable: %s", shown)
			}
			if facts := interopQueryStrings(t, target, "SELECT `key` FROM facts"); len(facts) != 2 {
				t.Fatalf("import promoted pending payload: %v", facts)
			}
			for _, table := range []string{"facts", "decisions", "tasks", "session_notes", "project_state", "project_arch"} {
				first := interopQueryStrings(t, source, "SELECT id FROM "+table+" ORDER BY id")
				second := interopQueryStrings(t, target, "SELECT id FROM "+table+" ORDER BY id")
				if !reflect.DeepEqual(first, second) {
					t.Fatalf("CLI %s IDs changed", table)
				}
			}
			if actual, err := os.ReadFile(configPath); err != nil || !bytes.Equal(actual, config) {
				t.Fatal("import changed local config")
			}
			for path, before := range map[string][]byte{indexPath: index, renderPath: rendered} {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
					t.Fatalf("import changed existing derived/render artifact %s", path)
				}
			}
			if actual, err := os.ReadFile(filepath.Join(target, ".memdolt", "local-sentinel")); err != nil || string(actual) != "retain local artifacts" {
				t.Fatal("import changed local artifact")
			}
			if text := runMemdolt(t, "doc", "show", doc.Document.ID, "--dir", target); !strings.Contains(text, "Existing target reference") {
				t.Fatal("import lost retained document")
			}
			response := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "Open task", "--source-type", "task", "--mode", "fts", "--provenance", "--dir", target, "--json"))
			if len(response.Results) == 0 || response.Results[0].LastChanged == nil || response.Results[0].LastChanged.Hash != imported.MainCommit {
				t.Fatalf("production recall lost import provenance: %+v", response)
			}
			// A real CLI review can accept the ordinary imported proposal; the
			// explicit synthetic force skips inference but retains all other guards.
			runMemdolt(t, "review", "accept", imported.CreatedProposals[0].ID, "--force", "--dir", target)
			if facts := interopQueryStrings(t, target, "SELECT `key` FROM facts"); len(facts) != 3 {
				t.Fatal("ordinary CLI review did not promote the imported proposal")
			}
		})
	}
}

func interopQueryStrings(t *testing.T, base, query string) []string {
	t.Helper()
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := st.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestInteropCLILegacyAndFailuresEmitOneResult(t *testing.T) {
	base := initStore(t)
	raw, err := os.ReadFile(repoFile("internal", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(interopTempDir(t), "legacy.json")
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", "--from-memhub", fixture, "--dir", base, "--json"))
	if result.MainCommit == "" || result.Counts["session_notes"] != 3 {
		t.Fatalf("CLI legacy import=%+v", result)
	}
	for _, args := range [][]string{
		{"import", "--from-memhub", fixture, "--dir", base, "--json"},
		{"import", fixture, "--dir", initStore(t), "--json"},
		{"import", "--dir", initStore(t), "--json"},
		{"export", filepath.Join(base, ".memdolt", "server.pid"), "--dir", base, "--json"},
		{"export", "--dir", base, "--json"},
		{"import", fixture, "--dir", interopTempDir(t), "--json"},
	} {
		var out bytes.Buffer
		cmd := newRootCommand()
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted invalid CLI operation: %v", args)
		}
		var failed localdolt.InteropResult
		if err := json.Unmarshal(out.Bytes(), &failed); err != nil || failed.Status != "refused" || failed.Error == "" || failed.MainCommit != "" {
			t.Fatalf("failure is not one honest JSON result: %s, %v", out.String(), err)
		}
	}
}
