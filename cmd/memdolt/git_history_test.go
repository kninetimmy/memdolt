package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/kninetimmy/memdolt/internal/search"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func gitHistoryCLIFixture(t *testing.T, base string) {
	t.Helper()
	writeTestFile(t, filepath.Join(base, "雪 source.go"), "package a\nfunc HistoryFixture() {}\n")
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", "雪 source.go"}, {"-c", "user.name=History Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "History subject"}} {
		if out, err := exec.Command("git", append([]string{"-C", base}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("fixture Git=%s %v", out, err)
		}
	}
}

func TestGitIngestCLIHumanJSONWithoutDoltOrOwner(t *testing.T) {
	base := scratchDir(t)
	gitHistoryCLIFixture(t, base)
	for _, name := range []string{"server.pid", "LOCK", "embeddings.sqlite"} {
		writeTestFile(t, filepath.Join(base, ".memdolt", name), "synthetic unrelated sentinel")
	}
	config := "[retrieval]\nmode='hybrid'\n"
	writeTestFile(t, filepath.Join(base, ".memdolt", "config.toml"), config)
	first := decodeJSON[codeindex.GitIngestSummary](t, runMemdolt(t, "ingest-git", "--dir", base, "--json"))
	if !first.Committed || first.Head == "" || first.Since != nil || first.CommitsSeen != 1 || first.UniqueFilesSeen != 1 || first.CommitFileLinksSeen != 1 {
		t.Fatalf("ingest JSON=%+v", first)
	}
	if human := runMemdolt(t, "ingest-git", "--dir", base); !strings.Contains(human, "1 commits seen, 1 indexed; 1 unique files, 1 links") || !strings.Contains(human, first.Head) {
		t.Fatalf("ingest human=%q", human)
	}
	explicit := decodeJSON[search.Response](t, runMemdolt(t, "search", "file:雪 source.go", "--dir", base, "--json"))
	fallback := decodeJSON[search.Response](t, runMemdolt(t, "search", "雪 source.go", "--dir", base, "--json"))
	if !reflect.DeepEqual(explicit, fallback) || len(explicit.Results) != 1 || explicit.Results[0].CommitSHA != first.Head || explicit.Results[0].Author != "History Fixture" || explicit.Results[0].Subject != "History subject" || explicit.Results[0].ChangeType != "A" {
		t.Fatalf("file search=%+v fallback=%+v", explicit, fallback)
	}
	if human := runMemdolt(t, "search", "file:雪 source.go", "--dir", base); !strings.Contains(human, "Cached Git history only") || !strings.Contains(human, "History subject") || !strings.Contains(human, first.Head) {
		t.Fatalf("file human=%q", human)
	}
	empty := decodeJSON[codeindex.GitIngestSummary](t, runMemdolt(t, "ingest-git", "--since", first.Head, "--dir", base, "--json"))
	if !empty.Committed || empty.CommitsSeen != 0 || empty.Since == nil || *empty.Since != first.Head {
		t.Fatalf("CLI empty range=%+v", empty)
	}
	for _, revision := range []string{"", "--all", "unavailable"} {
		if text := runMemdoltErr(t, "ingest-git", "--since", revision, "--dir", base, "--json"); strings.Contains(text, "committed\":true") {
			t.Fatalf("invalid revision reported success=%s", text)
		}
	}
	for _, name := range []string{"server.pid", "LOCK", "embeddings.sqlite"} {
		data, err := os.ReadFile(filepath.Join(base, ".memdolt", name))
		if err != nil || string(data) != "synthetic unrelated sentinel" {
			t.Fatalf("changed %s: %v", name, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(base, ".memdolt", "config.toml")); err != nil || string(data) != config {
		t.Fatal("Git operation changed configuration")
	}
	if _, err := os.Stat(filepath.Join(base, ".memdolt", "dolt")); !os.IsNotExist(err) {
		t.Fatal("Git operation opened Dolt")
	}
}

func TestFileHistoryCLIAndActualMCPLiveOwnerPreserveMemoryAndQueuedNotes(t *testing.T) {
	base := initStore(t)
	gitHistoryCLIFixture(t, base)
	runMemdolt(t, "decision", "add", "History source.go choice", "--rationale", "Decision search remains available", "--dir", base)
	decisionBefore := runMemdolt(t, "search", "decision:source.go", "--dir", base, "--json")
	plainBefore := runMemdolt(t, "search", "unindexed.choice", "--dir", base, "--json")
	before := interopRepoSnapshot(t, base)
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelperProcess$")
	cmd.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+base)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "history-live-owner", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect owner: %v %s", err, &stderr)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	call := func(query string) search.Response {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": query}})
		if err != nil || result.IsError {
			t.Fatalf("MCP search=%+v %v", result, err)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return decodeJSON[search.Response](t, string(raw))
	}
	queued, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "log_session_note", Arguments: map[string]any{"text": "Keep this note queued during Git history operations"}})
	if err != nil || queued.IsError {
		t.Fatalf("queue=%+v %v", queued, err)
	}
	ownerFile, err := os.ReadFile(filepath.Join(base, ".memdolt", "server.pid"))
	if err != nil {
		t.Fatal(err)
	}
	runMemdolt(t, "ingest-git", "--dir", base, "--json")
	cli := decodeJSON[search.Response](t, runMemdolt(t, "search", "file:雪 source.go", "--dir", base, "--json"))
	if got := call("file:雪 source.go"); !reflect.DeepEqual(got, cli) {
		t.Fatalf("actual MCP and CLI disagree: MCP=%+v CLI=%+v", got, cli)
	}
	if got := call("雪 source.go"); !reflect.DeepEqual(got, cli) {
		t.Fatalf("MCP indexed path fallback=%+v", got)
	}
	if got := runMemdolt(t, "search", "decision:source.go", "--dir", base, "--json"); got != decisionBefore {
		t.Fatal("owner-routed explicit decision output changed")
	}
	if got := runMemdolt(t, "search", "unindexed.choice", "--dir", base, "--json"); got != plainBefore {
		t.Fatal("owner-routed decision fallback changed")
	}
	if got := call("decision:source.go"); !reflect.DeepEqual(got, decodeJSON[search.Response](t, decisionBefore)) {
		t.Fatal("MCP decision ranking/output changed")
	}
	if got := interopRepoSnapshot(t, base); got != before {
		t.Fatal("Git history changed durable main, native refs or working/staged roots")
	}
	if notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")); len(notes.Notes) != 0 {
		t.Fatal("Git history flushed queued notes")
	}
	if data, err := os.ReadFile(filepath.Join(base, ".memdolt", "server.pid")); err != nil || !bytes.Equal(data, ownerFile) {
		t.Fatal("Git history changed the live owner credential")
	}
}
