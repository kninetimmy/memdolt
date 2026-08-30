package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

var m3ToolNames = []string{
	"get_command", "list_decisions", "list_facts", "list_proposals", "list_tasks",
	"log_session_note", "propose_decision", "propose_fact", "propose_supersede",
	"recall", "record_command", "search", "status", "task_add", "task_done",
}

func TestM3ToolsSchemasSuccessAndRefusals(t *testing.T) {
	ctx := context.Background()
	base, st := initializedToolStore(t)
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server, &mcp.Implementation{Name: "Claude Code", Version: "1"}, false)

	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	var recallSourceTypesDescription string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %s has input=%v output=%v; every M3 tool needs both schemas", tool.Name, tool.InputSchema, tool.OutputSchema)
		}
		if tool.Name == "recall" {
			schema, ok := tool.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("recall input schema type = %T, want map", tool.InputSchema)
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("recall input schema properties = %T, want map", schema["properties"])
			}
			sourceTypes, ok := properties["source_types"].(map[string]any)
			if !ok {
				t.Fatalf("recall source_types schema = %T, want map", properties["source_types"])
			}
			recallSourceTypesDescription, _ = sourceTypes["description"].(string)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, m3ToolNames) {
		t.Fatalf("registered tools = %v, want %v", names, m3ToolNames)
	}
	for _, deferred := range []string{"locate", "doc_add", "render", "repo_status", "repo_pull", "repo_push", "history", "archive_transcript"} {
		if slices.Contains(names, deferred) {
			t.Errorf("deferred tool %q was advertised", deferred)
		}
	}
	const advertisedDocumentSource = "doc_chunk"
	if want := "optional fact, decision, task, or doc_chunk filters"; recallSourceTypesDescription != want {
		t.Fatalf("recall source_types description = %q, want %q", recallSourceTypesDescription, want)
	}

	callOK(t, client, "status", map[string]any{})
	callOK(t, client, "recall", map[string]any{
		"query": "advertised document source", "mode": "fts", "source_types": []string{advertisedDocumentSource},
	})
	taskAdded := callAs[taskWriteOutput](t, client, "task_add", map[string]any{
		"title": "Exercise every M3 tool", "notes": "in memory",
	})
	if got := testText(t, st, "SELECT committer FROM dolt_log WHERE commit_hash = ?", taskAdded.Commit); got != "agent:claude-code" {
		t.Fatalf("MCP task commit author = %q, want agent:claude-code", got)
	}
	callOK(t, client, "list_tasks", map[string]any{"status": "open"})
	callAs[taskWriteOutput](t, client, "task_done", map[string]any{"id": taskAdded.Task.ID})
	callError(t, client, "task_done", map[string]any{"id": "missing"}, "not found")

	callAs[commandWriteOutput](t, client, "record_command", map[string]any{
		"kind": "test", "cmdline": "go test ./...", "exit_code": 0,
	})
	callOK(t, client, "get_command", map[string]any{"kind": "test"})
	callError(t, client, "get_command", map[string]any{"kind": "build"}, "not found")

	callOK(t, client, "log_session_note", map[string]any{"text": "first accumulated note"})
	callOK(t, client, "log_session_note", map[string]any{"text": "second accumulated note"})
	if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 0 {
		t.Fatalf("queued notes reached main before flush: %d", got)
	}

	stagedFact := callAs[stagedProposalOutput](t, client, "propose_fact", map[string]any{
		"key": "build.command", "value": "go test ./...", "rationale": "the repository instructions require it", "kind": "command",
	})
	if got := testCount(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 0 {
		t.Fatalf("fact proposal moved main: %d durable facts", got)
	}
	callOK(t, client, "list_proposals", map[string]any{"target": "repo"})
	acceptProposal(t, st, stagedFact.ID)
	callError(t, client, "propose_fact", map[string]any{
		"key": "build.command", "value": "go test -race ./...", "rationale": "duplicate live key",
	}, "live fact already exists")

	stagedDecision := callAs[stagedProposalOutput](t, client, "propose_decision", map[string]any{
		"title": "Use Dolt-backed MCP tools", "rationale": "the CLI and MCP must share store behavior",
		"alternatives_rejected": "duplicate handlers", "evidence": "docs/prd/memdolt-prd.md",
	})
	if got := testCount(t, st, "SELECT COUNT(*) FROM decisions AS OF 'main'"); got != 0 {
		t.Fatalf("decision proposal moved main: %d durable decisions", got)
	}
	acceptProposal(t, st, stagedDecision.ID)
	callOK(t, client, "list_decisions", map[string]any{"status": "active"})
	callOK(t, client, "search", map[string]any{"query": "Dolt-backed", "limit": 5})
	callError(t, client, "search", map[string]any{"query": "file:README.md"}, "M5")

	stagedSupersede := callAs[stagedProposalOutput](t, client, "propose_supersede", map[string]any{
		"superseded_id": stagedFact.RowID, "key": "build.command", "value": "go test -race ./... is the race lane",
		"rationale": "the race lane is now required",
	})
	callOK(t, client, "list_proposals", map[string]any{})
	acceptProposal(t, st, stagedSupersede.ID)
	facts := callAs[listFactsOutput](t, client, "list_facts", map[string]any{"prefix": "build."})
	if len(facts.Facts) != 2 || facts.Facts[0].SupersededBy == "" {
		t.Fatalf("list_facts lost the superseded chain: %+v", facts.Facts)
	}
	callError(t, client, "list_facts", map[string]any{"prefix": "build"}, "must end in '.'")

	recalled := callAs[retrievalOutput](t, client, "recall", map[string]any{
		"query": "race lane", "mode": "fts", "source_types": []string{"fact"}, "provenance": true,
	})
	if recalled.ReturnedCount == 0 || !recalled.HasProvenance() {
		t.Fatalf("recall did not preserve results and provenance: %+v", recalled)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
	if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 2 {
		t.Fatalf("shutdown flush committed %d notes, want 2", got)
	}
	if got := testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (2)'"); got != 1 {
		t.Fatalf("shutdown flush made %d note-batch commits, want 1", got)
	}
	if got := testText(t, st, "SELECT committer FROM dolt_log WHERE message = 'note batch (2)'"); got != "agent:claude-code" {
		t.Fatalf("note-batch commit author = %q, want agent:claude-code", got)
	}
	if got := testText(t, st, "SELECT actor_raw FROM session_notes AS OF 'main' ORDER BY id LIMIT 1"); got != "Claude Code" {
		t.Fatalf("note raw actor = %q, want Claude Code", got)
	}
}

func TestListFactsTreatsPrefixCharactersLiterallyAndLargeHorizonDoesNotOverflow(t *testing.T) {
	ctx := context.Background()
	base, st := initializedToolStore(t)
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)

	tests := []struct {
		name, prefix, match, decoy string
	}{
		{name: "percent", prefix: "percent%.", match: "percent%.match", decoy: "percentX.match"},
		{name: "underscore", prefix: "under_.", match: "under_.match", decoy: "underX.match"},
		{name: "backslash", prefix: `slash\.`, match: `slash\.match`, decoy: "slash.match"},
		{name: "escape", prefix: "bang!.", match: "bang!.match", decoy: "bang.match"},
	}
	var verifiedID string
	for _, test := range tests {
		for _, key := range []string{test.match, test.decoy} {
			staged := callAs[stagedProposalOutput](t, client, "propose_fact", map[string]any{
				"key": key, "value": "literal prefix " + test.name, "rationale": "exercise literal prefix binding",
			})
			acceptProposal(t, st, staged.ID)
			if key == test.match && verifiedID == "" {
				verifiedID = staged.RowID
			}
		}
	}

	verifiedAt := time.Now().UTC().Add(-365 * 24 * time.Hour)
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{SQL: "UPDATE facts SET verified_at = ? WHERE id = ?", Args: []any{verifiedAt, verifiedID}}},
		Text:       []string{"verify literal-prefix boundary fact"},
		Message:    "verify literal-prefix boundary fact",
		Author:     memory.UserActor.CommitAuthor(),
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("[retrieval]\nfact_stale_after_days = 9223372036854775807\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := callAs[listFactsOutput](t, client, "list_facts", map[string]any{"prefix": test.prefix})
			if len(facts.Facts) != 1 || facts.Facts[0].Key != test.match {
				t.Fatalf("list_facts(%q) = %+v, want only %q", test.prefix, facts.Facts, test.match)
			}
			if facts.Facts[0].ID == verifiedID && facts.Facts[0].Stale {
				t.Fatalf("365-day-old fact is stale under max-int64 day horizon: %+v", facts.Facts[0])
			}
		})
	}
	callError(t, client, "list_facts", map[string]any{"prefix": "percent%"}, "must end in '.'")
	callError(t, client, "list_facts", map[string]any{"limit": -1}, "must not be negative")

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

// retrievalOutput keeps this test coupled only to the fields that prove MCP
// preserved ranking/provenance, while the tool's inferred schema still comes
// from the complete retrieval.Response type.
type retrievalOutput struct {
	ReturnedCount int `json:"returnedCount"`
	Results       []struct {
		LastChanged any `json:"lastChanged"`
	} `json:"results"`
}

func (r retrievalOutput) HasProvenance() bool {
	for _, hit := range r.Results {
		if hit.LastChanged != nil {
			return true
		}
	}
	return false
}

func TestSessionNoteDeadlineFlushesAndDenyListRefusesVisibly(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		base, st := initializedToolStore(t)
		server := New("test")
		tools := registerTools(server, base, st, 20*time.Millisecond)
		client, serverSession := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
		callOK(t, client, "log_session_note", map[string]any{"text": "flush me on the timer"})

		deadline := time.Now().Add(10 * time.Second)
		for testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'") != 1 {
			if time.Now().After(deadline) {
				t.Fatal("note deadline did not flush")
			}
			time.Sleep(10 * time.Millisecond)
		}
		closeSessions(t, client, serverSession)
		if err := tools.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("deny list", func(t *testing.T) {
		base, st := initializedToolStore(t)
		paths, err := layout.New(base)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.ConfigFile(), []byte("[deny_list]\npatterns = ['BLOCKED']\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		server := New("test")
		tools := RegisterTools(server, base, st)
		client, serverSession := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
		callError(t, client, "log_session_note", map[string]any{"text": "BLOCKED secret"}, "deny-list")
		if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 0 {
			t.Fatalf("denied note wrote %d rows", got)
		}
		closeSessions(t, client, serverSession)
		if err := tools.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("mixed failure retries only failed group on shutdown", func(t *testing.T) {
		base, st := initializedToolStore(t)
		paths, err := layout.New(base)
		if err != nil {
			t.Fatal(err)
		}
		server := New("test")
		tools := RegisterTools(server, base, st)
		client, serverSession := connect(t, server, &mcp.Implementation{Name: "session-client", Version: "1"}, false)
		for _, note := range []struct {
			actor string
			text  string
		}{
			{actor: "Codex", text: "becomes forbidden before shutdown"},
			{actor: "Claude Code", text: "allowed note survives another actor's failure"},
		} {
			result, callErr := client.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "log_session_note",
				Meta:      mcp.Meta{mcp.MetaKeyClientInfo: &mcp.Implementation{Name: note.actor, Version: "1"}},
				Arguments: map[string]any{"text": note.text},
			})
			if callErr != nil || result.IsError {
				t.Fatalf("queue %s note: result=%+v err=%v", note.actor, result, callErr)
			}
		}
		if err := os.WriteFile(paths.ConfigFile(), []byte("[deny_list]\npatterns = ['forbidden']\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		tools.mu.Lock()
		if tools.timer != nil {
			tools.timer.Stop()
			tools.timer = nil
		}
		tools.flushErr = tools.flushLocked(context.Background())
		flushErr := tools.flushErr
		pending := append([]noteGroup(nil), tools.groups...)
		tools.mu.Unlock()
		if flushErr == nil || !strings.Contains(flushErr.Error(), "deny-list") {
			t.Fatalf("deadline-style flush error = %v, want reported deny-list failure", flushErr)
		}
		if len(pending) != 1 || pending[0].actor.Name != "agent:codex" {
			t.Fatalf("pending groups after mixed flush = %+v, want only agent:codex", pending)
		}
		if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main' WHERE actor = 'agent:claude-code'"); got != 1 {
			t.Fatalf("allowed actor rows after mixed flush = %d, want 1", got)
		}
		if got := testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (1)' AND committer = 'agent:claude-code'"); got != 1 {
			t.Fatalf("allowed actor commits after mixed flush = %d, want 1", got)
		}

		closeSessions(t, client, serverSession)
		if err := tools.Close(); err == nil || !strings.Contains(err.Error(), "deny-list") {
			t.Fatalf("shutdown error = %v, want reported deny-list failure", err)
		}
		tools.mu.Lock()
		pendingAfterClose := len(tools.groups)
		tools.mu.Unlock()
		if pendingAfterClose != 0 {
			t.Fatalf("closed toolset retained %d failed groups, want explicit discard", pendingAfterClose)
		}
		if err := tools.Close(); err == nil || !strings.Contains(err.Error(), "deny-list") {
			t.Fatalf("repeated close error = %v, want retained shutdown failure", err)
		}
		if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'"); got != 1 {
			t.Fatalf("mixed failure left %d durable notes, want only the allowed note", got)
		}
		if got := testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (1)' AND committer = 'agent:claude-code'"); got != 1 {
			t.Fatalf("shutdown recommitted the successful group: %d commits", got)
		}
	})
}

func TestSessionNoteBatchesPreservePerRequestAuthors(t *testing.T) {
	base, st := initializedToolStore(t)
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server, &mcp.Implementation{Name: "session-client", Version: "1"}, false)

	for _, name := range []string{"Claude Code", "Codex"} {
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "log_session_note",
			Meta:      mcp.Meta{mcp.MetaKeyClientInfo: &mcp.Implementation{Name: name, Version: "1"}},
			Arguments: map[string]any{"text": "note from " + name},
		})
		if err != nil || result.IsError {
			t.Fatalf("queue %s note: result=%+v err=%v", name, result, err)
		}
	}
	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"agent:claude-code", "agent:codex"} {
		if got := testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main' WHERE actor = ?", actor); got != 1 {
			t.Fatalf("%s note rows = %d, want 1", actor, got)
		}
		if got := testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (1)' AND committer = ?", actor); got != 1 {
			t.Fatalf("%s note commits = %d, want 1", actor, got)
		}
	}
}

func initializedToolStore(t *testing.T) (string, *localdolt.Store) {
	t.Helper()
	base := t.TempDir()
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: memory.UserActor.CommitAuthor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return base, st
}

func acceptProposal(t *testing.T, st *localdolt.Store, id string) {
	t.Helper()
	if _, err := st.AcceptProposal(context.Background(), id, memory.UserActor.CommitAuthor(), localdolt.AcceptOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
}

func callOK(t *testing.T, client *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("call %s: %v", name, result.GetError())
	}
	if result.StructuredContent == nil {
		t.Fatalf("call %s returned no structured content", name)
	}
	return result
}

func callAs[T any](t *testing.T, client *mcp.ClientSession, name string, arguments any) T {
	t.Helper()
	result := callOK(t, client, name, arguments)
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output T
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("decode %s output %s: %v", name, encoded, err)
	}
	return output
}

func callError(t *testing.T, client *mcp.ClientSession, name string, arguments any, contains string) {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s returned a protocol error instead of a visible tool error: %v", name, err)
	}
	if !result.IsError {
		t.Fatalf("call %s succeeded, want an error", name)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	visible := fmt.Sprintf("%v %s", result.GetError(), encoded)
	if !strings.Contains(visible, contains) {
		t.Fatalf("call %s error = %s, want it to contain %q", name, visible, contains)
	}
}

func testCount(t *testing.T, st *localdolt.Store, query string, args ...any) int {
	t.Helper()
	value := testText(t, st, query, args...)
	var count int
	if _, err := fmt.Sscan(value, &count); err != nil {
		t.Fatal(err)
	}
	return count
}

func testText(t *testing.T, st *localdolt.Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	if !rows.Next() {
		t.Fatalf("query %q returned no row: %v", query, rows.Err())
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
