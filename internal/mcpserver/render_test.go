package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store"
)

func TestRenderMCPContentProvenanceAndQueuedNoteIsolation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "modern", true: "legacy"}[legacy], func(t *testing.T) {
			ctx := context.Background()
			base, st := initializedToolStore(t)
			raw, err := os.ReadFile(filepath.Join("..", "render", "testdata", "memory.sql"))
			if err != nil {
				t.Fatal(err)
			}
			var statements []store.Statement
			for _, text := range strings.Split(strings.TrimSpace(string(raw)), ";\n") {
				statements = append(statements, store.Statement{SQL: strings.TrimSuffix(text, ";")})
			}
			seed, err := st.Commit(ctx, store.CommitRequest{Statements: statements, Text: []string{string(raw)}, Author: store.Actor{Name: "user", Email: "user@memdolt.invalid"}, Message: "MCP render fixture provenance"})
			if err != nil {
				t.Fatal(err)
			}
			server := New("test")
			tools := RegisterTools(server, base, testElicitationBackend(base, st))
			client, session := connect(t, server, &mcp.Implementation{Name: "cli", Version: "1"}, legacy)
			callOK(t, client, "log_session_note", map[string]any{"text": "QUEUED_NOTE_EXCLUDED"})
			callOK(t, client, "propose_fact", map[string]any{"key": "pending.render", "value": "PROPOSAL_EXCLUDED", "rationale": "pending fixture"})
			first := callAs[render.Result](t, client, "render", map[string]any{})
			if first.Status != "written" || first.SourceCommit != seed.Hash || len(first.WrittenFiles) != 2 {
				t.Fatalf("MCP render=%+v", first)
			}
			project, err := os.ReadFile(first.WrittenFiles[0])
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := os.ReadFile(first.WrittenFiles[1])
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{seed.Hash, "Building durable memory", "Local owner → committed Dolt", "agent:opencode", "cli", "Recorded session\n\nSecond paragraph stays.", "ses_fixture", "fixture-model"} {
				if !strings.Contains(string(project), text) {
					t.Errorf("PROJECT lacks %q", text)
				}
			}
			for _, text := range []string{seed.Hash, "Earlier choice", "Superseded by (decision ULID)", "old | command\nsecond line", "Superseded by (fact ULID)", "**Stale:** true", "Open task", "Blocked task", "Done task", "MCP render fixture provenance"} {
				if !strings.Contains(string(ledger), text) {
					t.Errorf("ledger lacks %q", text)
				}
			}
			for _, excluded := range []string{"QUEUED_NOTE_EXCLUDED", "PROPOSAL_EXCLUDED", "Token Accounting"} {
				if strings.Contains(string(project)+string(ledger), excluded) {
					t.Errorf("render exposed %s", excluded)
				}
			}
			second := callAs[render.Result](t, client, "render", map[string]any{})
			if len(second.BackupFiles) != 2 || second.SourceCommit != first.SourceCommit {
				t.Fatalf("repeated MCP render=%+v", second)
			}
			// No arbitrary tool argument controls the destination, even when a
			// client sends a name that would otherwise look like configuration.
			outside := t.TempDir()
			response, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "render", Arguments: map[string]any{"output_dir": outside}})
			if err != nil || !response.IsError {
				t.Fatalf("arbitrary destination accepted: %+v, %v", response, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("MCP selected an arbitrary output")
			}
			if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 1 {
				t.Fatal("render flushed the pending MCP note")
			}
			closeSessions(t, client, session)
			if err := tools.Close(); err != nil {
				t.Fatal(err)
			}
			if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 2 {
				t.Fatal("existing orderly-shutdown note flush changed")
			}
		})
	}
}

type renderFinalizationBackend struct{ Backend }

func (s renderFinalizationBackend) Render(ctx context.Context) (render.Result, error) {
	result, err := s.Backend.Render(ctx)
	return result, errors.Join(err, errors.New("synthetic finalization failure"))
}

func TestRenderMCPRetainsResultOnError(t *testing.T) {
	base, st := initializedToolStore(t)
	server := New("test")
	tools := RegisterTools(server, base, renderFinalizationBackend{testElicitationBackend(base, st)})
	client, session := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
	response, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "render", Arguments: map[string]any{}})
	if err != nil || !response.IsError || response.StructuredContent == nil {
		t.Fatalf("render failure lost structured effects: %+v, %v", response, err)
	}
	raw, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result render.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "written" || len(result.WrittenFiles) != 2 || !strings.Contains(result.Error, "finalization") {
		t.Fatalf("error lost confirmed outputs: %+v", result)
	}
	closeSessions(t, client, session)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}
