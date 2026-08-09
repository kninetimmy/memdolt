package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// TestInitCreatesTheStoreThenReportsItAlreadyInitialized covers the
// acceptance criterion that `memdolt init` creates the store and applies
// the schema, and that a second run reports the store as already
// initialized without adding a commit to the Dolt history.
func TestInitCreatesTheStoreThenReportsItAlreadyInitialized(t *testing.T) {
	base := scratchDir(t)
	latest := store.LatestSchemaVersion()

	first := runMemdolt(t, "init", "--dir", base)
	if !strings.HasPrefix(first, "initialized memdolt store at ") {
		t.Fatalf("first init said %q, want it to report the store it created", first)
	}
	for _, want := range []string{
		"(schema v" + strconv.Itoa(latest) + ")",
		"tagged " + store.MigrationTag(1),
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("first init said %q, want it to mention %q", first, want)
		}
	}

	// The store the command created is the one PRD §5.3 puts under
	// .memdolt/dolt, at the schema version this binary ships, with one
	// commit per migration on top of Dolt's own first commit.
	commitsAfterInit := 1 + latest
	if got := storeSchemaVersion(t, base); got != latest {
		t.Fatalf("schema version after init = %d, want %d", got, latest)
	}
	if got := storeCommitCount(t, base); got != commitsAfterInit {
		t.Fatalf("history after init has %d commits, want %d", got, commitsAfterInit)
	}

	second := runMemdolt(t, "init", "--dir", base)
	if !strings.Contains(second, "is already initialized") {
		t.Fatalf("second init said %q, want it to report the store as already initialized", second)
	}
	if got := storeCommitCount(t, base); got != commitsAfterInit {
		t.Fatalf("the second init changed the history to %d commits, want %d", got, commitsAfterInit)
	}
}

func TestInitJSON(t *testing.T) {
	base := scratchDir(t)

	out := runMemdolt(t, "init", "--dir", base, "--json")

	var payload struct {
		Store         string `json:"store"`
		SchemaVersion int    `json:"schemaVersion"`
		Applied       []struct {
			Version int    `json:"version"`
			Name    string `json:"name"`
			Commit  string `json:"commit"`
			Tag     string `json:"tag"`
		} `json:"applied"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		t.Fatalf("expected a single valid JSON object, got %q: %v", out, err)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("expected stdout to contain a single line, got %q", out)
	}

	latest := store.LatestSchemaVersion()
	if payload.SchemaVersion != latest {
		t.Fatalf("schemaVersion = %d, want %d", payload.SchemaVersion, latest)
	}
	if len(payload.Applied) != latest {
		t.Fatalf("applied %d migrations, want %d", len(payload.Applied), latest)
	}
	if payload.Applied[0].Tag != store.MigrationTag(1) {
		t.Fatalf("applied[0].tag = %q, want %q", payload.Applied[0].Tag, store.MigrationTag(1))
	}
	if payload.Applied[0].Commit == "" {
		t.Fatal("applied[0].commit is empty")
	}
	if payload.Store == "" {
		t.Fatal("store path is empty")
	}
}

func runMemdolt(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute `memdolt %s`: %v (output %q)", strings.Join(args, " "), err, out)
	}
	return out.String()
}

// scratchDir returns a repository root to initialize a store beneath. It
// does not use t.TempDir because that fails the test if the directory
// cannot be removed, and the embedded engine memory-maps files inside the
// Dolt data directory that Windows may still hold briefly after the store
// is closed.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-init")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// openInitializedStore opens the store `memdolt init` left behind. The
// command releases the single-owner lock when it finishes, so this is what
// a later memdolt invocation would see.
func openInitializedStore(t *testing.T, base string) *localdolt.Store {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: initActor})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open the store init created: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func storeSchemaVersion(t *testing.T, base string) int {
	t.Helper()
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()
	version, err := st.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

func storeCommitCount(t *testing.T, base string) int {
	t.Helper()
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()

	rows, err := st.Query(context.Background(), "SELECT COUNT(*) FROM dolt_log")
	if err != nil {
		t.Fatalf("count commits: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("count commits: no rows (err %v)", rows.Err())
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("count commits: scan: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count commits: %v", err)
	}
	return count
}
