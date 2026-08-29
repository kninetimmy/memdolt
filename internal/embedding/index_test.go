package embedding

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const (
	factID     = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	decisionID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	taskID     = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	documentID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	chunkID    = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
)

type indexFixture struct {
	ctx  context.Context
	base string
	path string
	st   *localdolt.Store
}

type doltSnapshot struct {
	commits []string
	status  []string
	rows    map[string][]string
}

type storedRow struct {
	hash      string
	dimension int
	vector    string
}

func newIndexFixture(t *testing.T) *indexFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "memdolt embedding index")
	if err != nil {
		t.Fatalf("create disposable repository: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	ctx := context.Background()
	actor := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: actor})
	if err != nil {
		t.Fatalf("new disposable Dolt store: %v", err)
	}
	if err := st.Open(ctx); err != nil {
		t.Fatalf("open disposable Dolt store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate disposable Dolt store: %v", err)
	}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "INSERT INTO facts (id, `key`, value, source, verified_at, created_at) VALUES (?, ?, ?, 'user', NOW(), NOW())", Args: []any{factID, "build.command", "go test ./..."}},
			{SQL: "INSERT INTO decisions (id, title, rationale, summary, status, source, decided_at) VALUES (?, ?, ?, ?, 'active', 'user', NOW())", Args: []any{decisionID, "Keep vectors local", "Dolt history stays small", "Derived data is rebuildable"}},
			{SQL: "INSERT INTO tasks (id, title, status, notes, created_at, updated_at) VALUES (?, ?, 'open', ?, NOW(), NOW())", Args: []any{taskID, "Ship retrieval", "Run the golden gate"}},
			{SQL: "INSERT INTO documents (id, path, title, content_hash, byte_len, source, ingested_at) VALUES (?, ?, ?, ?, ?, 'user', NOW())", Args: []any{documentID, "docs/retrieval.md", "Retrieval", strings.Repeat("0", 64), 12}},
			{SQL: "INSERT INTO doc_chunks (id, doc_id, ord, heading_path, body) VALUES (?, ?, 0, ?, ?)", Args: []any{chunkID, documentID, "Retrieval > Index", "Rebuild derived vectors"}},
		},
		Text:    []string{"build.command", "go test ./...", "Keep vectors local", "Dolt history stays small", "Derived data is rebuildable", "Ship retrieval", "Run the golden gate", "Retrieval > Index", "Rebuild derived vectors"},
		Message: "seed embedding index sources",
		Author:  actor,
	}); err != nil {
		t.Fatalf("seed disposable Dolt store: %v", err)
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("resolve disposable layout: %v", err)
	}
	return &indexFixture{ctx: ctx, base: base, path: paths.EmbeddingsFile(), st: st}
}

func TestRebuildSynchronizesOnlyChangedDerivedRows(t *testing.T) {
	f := newIndexFixture(t)
	calls := map[string]int{}
	embed := deterministicEmbedder(calls)

	sources := f.sourcesWithoutChangingDolt(t)
	wantTexts := map[string]string{
		"fact/" + factID:         "build.command: go test ./...",
		"decision/" + decisionID: "Derived data is rebuildable\n\nKeep vectors local\n\nDolt history stays small",
		"task/" + taskID:         "Ship retrieval\n\nRun the golden gate",
		"doc_chunk/" + chunkID:   "Retrieval > Index\n\nRebuild derived vectors",
	}
	for _, source := range sources {
		if got, want := source.Text, wantTexts[source.SourceType+"/"+source.SourceID]; got != want {
			t.Errorf("%s/%s text = %q, want %q", source.SourceType, source.SourceID, got, want)
		}
	}

	first := f.rebuildWithoutChangingDolt(t, embed)
	if first.Eligible != 4 || first.Created != 4 || first.Refreshed != 0 || first.Unchanged != 0 || first.Removed != 0 {
		t.Fatalf("first rebuild = %+v, want four created rows", first)
	}
	if len(calls) != 4 {
		t.Fatalf("first rebuild embedded %d distinct texts, want 4", len(calls))
	}
	assertSideStoreSchema(t, f.path)
	firstRows := readStoredRows(t, f.path)

	second := f.rebuildWithoutChangingDolt(t, embed)
	if second.Created != 0 || second.Refreshed != 0 || second.Unchanged != 4 || second.Removed != 0 {
		t.Fatalf("unchanged rebuild = %+v, want four unchanged rows", second)
	}
	if got := totalCalls(calls); got != 4 {
		t.Fatalf("unchanged rebuild made %d total inference calls, want 4", got)
	}
	if got := readStoredRows(t, f.path); !reflect.DeepEqual(got, firstRows) {
		t.Fatalf("unchanged rebuild changed side-store rows\nbefore: %#v\nafter:  %#v", firstRows, got)
	}

	f.commit(t, "change one embedding source", []store.Statement{{
		SQL: "UPDATE decisions SET rationale = ? WHERE id = ?", Args: []any{"Only derived SQLite changes", decisionID},
	}}, []string{"Only derived SQLite changes"})
	changed := f.rebuildWithoutChangingDolt(t, embed)
	if changed.Created != 0 || changed.Refreshed != 1 || changed.Unchanged != 3 || changed.Removed != 0 {
		t.Fatalf("changed-source rebuild = %+v, want one refreshed and three unchanged", changed)
	}
	changedRows := readStoredRows(t, f.path)
	for key, before := range firstRows {
		after := changedRows[key]
		if key == "decision/"+decisionID {
			if after == before {
				t.Errorf("changed decision retained its old derived row: %+v", after)
			}
		} else if after != before {
			t.Errorf("unchanged source %s changed derived row: before %+v, after %+v", key, before, after)
		}
	}
	if got := totalCalls(calls); got != 5 {
		t.Fatalf("one source change made %d total inference calls, want 5", got)
	}

	f.commit(t, "remove one embedding source", []store.Statement{{
		SQL: "DELETE FROM tasks WHERE id = ?", Args: []any{taskID},
	}}, []string{"Ship retrieval", "Run the golden gate"})
	removed := f.rebuildWithoutChangingDolt(t, embed)
	if removed.Created != 0 || removed.Refreshed != 0 || removed.Unchanged != 3 || removed.Removed != 1 {
		t.Fatalf("orphan cleanup rebuild = %+v, want one removed and three unchanged", removed)
	}
	if _, ok := readStoredRows(t, f.path)["task/"+taskID]; ok {
		t.Fatal("orphaned task embedding remains after rebuild")
	}
}

func TestStatusClassifiesEveryStaleShapeWithoutWritingDolt(t *testing.T) {
	f := newIndexFixture(t)
	f.rebuildWithoutChangingDolt(t, deterministicEmbedder(map[string]int{}))
	db := openSideStore(t, f.path)
	if _, err := db.Exec("DELETE FROM embeddings WHERE source_type = 'fact' AND source_id = ?", factID); err != nil {
		t.Fatalf("remove fact embedding: %v", err)
	}
	if _, err := db.Exec("UPDATE embeddings SET content_hash = ? WHERE source_type = 'task' AND source_id = ?", strings.Repeat("f", 64), taskID); err != nil {
		t.Fatalf("corrupt task hash: %v", err)
	}
	if _, err := db.Exec("UPDATE embeddings SET vector = x'0001' WHERE source_type = 'decision' AND source_id = ?", decisionID); err != nil {
		t.Fatalf("corrupt decision vector length: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO embeddings
  (source_type, source_id, model_name, vector, content_hash, dimension)
VALUES ('task', 'removed-task', ?, zeroblob(?), ?, ?)`,
		EmbeddingModelName, EmbeddingDim*4, strings.Repeat("0", 64), EmbeddingDim); err != nil {
		t.Fatalf("insert orphaned embedding: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close side-store corruption handle: %v", err)
	}

	report := f.statusWithoutChangingDolt(t, f.path)
	if report.Eligible != 4 || report.Current != 1 || report.Missing != 1 || report.ContentHashMismatched != 1 || report.WrongByteLength != 1 || report.Orphaned != 1 {
		t.Fatalf("status = %+v, want one row in each required state plus one orphan", report)
	}
	if !report.NeedsRebuild || report.Remedy != RebuildRemedy {
		t.Fatalf("stale status remedy = %q (needs=%v), want %q", report.Remedy, report.NeedsRebuild, RebuildRemedy)
	}
	states := map[string]StatusState{}
	for _, entry := range report.Entries {
		states[entry.SourceType+"/"+entry.SourceID] = entry.State
	}
	for key, want := range map[string]StatusState{
		"fact/" + factID:         StatusMissing,
		"decision/" + decisionID: StatusWrongByteLength,
		"task/" + taskID:         StatusContentHashMismatched,
		"doc_chunk/" + chunkID:   StatusCurrent,
	} {
		if got := states[key]; got != want {
			t.Errorf("%s status = %q, want %q", key, got, want)
		}
	}
	repaired := f.rebuildWithoutChangingDolt(t, deterministicEmbedder(map[string]int{}))
	if repaired.Created != 1 || repaired.Refreshed != 2 || repaired.Unchanged != 1 || repaired.Removed != 1 {
		t.Fatalf("stale-row rebuild = %+v, want one create, two refreshes, one unchanged, and one orphan removed", repaired)
	}
	current := f.statusWithoutChangingDolt(t, f.path)
	if current.Current != 4 || current.NeedsRebuild || current.Missing+current.ContentHashMismatched+current.WrongByteLength+current.Orphaned != 0 {
		t.Fatalf("repaired index status = %+v, want four current rows", current)
	}

	missingPath := filepath.Join(f.base, ".memdolt", "missing-embeddings.sqlite")
	missing := f.statusWithoutChangingDolt(t, missingPath)
	if missing.Missing != 4 || missing.Current != 0 || !missing.NeedsRebuild {
		t.Fatalf("missing side-store status = %+v, want four missing rows", missing)
	}
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("read-only status created %s or returned an unexpected stat error: %v", missingPath, err)
	}
}

func (f *indexFixture) commit(t *testing.T, message string, statements []store.Statement, text []string) {
	t.Helper()
	if _, err := f.st.Commit(f.ctx, store.CommitRequest{
		Statements: statements,
		Text:       text,
		Message:    message,
		Author:     store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("%s: %v", message, err)
	}
}

func (f *indexFixture) sourcesWithoutChangingDolt(t *testing.T) []store.EmbeddingSource {
	t.Helper()
	before := f.snapshot(t)
	sources, err := f.st.EmbeddingSources(f.ctx)
	if err != nil {
		t.Fatalf("read embedding sources: %v", err)
	}
	f.assertSnapshot(t, before)
	return sources
}

func (f *indexFixture) rebuildWithoutChangingDolt(t *testing.T, embed func(string) ([]float32, error)) RebuildResult {
	t.Helper()
	before := f.snapshot(t)
	sources, err := f.st.EmbeddingSources(f.ctx)
	if err != nil {
		t.Fatalf("read embedding sources: %v", err)
	}
	result, err := Rebuild(f.ctx, f.path, sources, embed)
	if err != nil {
		t.Fatalf("rebuild embedding index: %v", err)
	}
	f.assertSnapshot(t, before)
	return result
}

func (f *indexFixture) statusWithoutChangingDolt(t *testing.T, path string) StatusReport {
	t.Helper()
	before := f.snapshot(t)
	sources, err := f.st.EmbeddingSources(f.ctx)
	if err != nil {
		t.Fatalf("read embedding sources: %v", err)
	}
	report, err := Status(f.ctx, path, sources)
	if err != nil {
		t.Fatalf("read embedding index status: %v", err)
	}
	f.assertSnapshot(t, before)
	return report
}

func (f *indexFixture) snapshot(t *testing.T) doltSnapshot {
	t.Helper()
	return doltSnapshot{
		commits: queryDoltStrings(t, f.st, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash", 1),
		status:  queryDoltStrings(t, f.st, "SELECT table_name, status, staged FROM dolt_status ORDER BY table_name", 3),
		rows: map[string][]string{
			"facts":      queryDoltStrings(t, f.st, "SELECT id, COALESCE(`key`, ''), COALESCE(value, ''), COALESCE(superseded_by, '') FROM facts ORDER BY id", 4),
			"decisions":  queryDoltStrings(t, f.st, "SELECT id, COALESCE(title, ''), COALESCE(rationale, ''), COALESCE(summary, ''), status, COALESCE(superseded_by, '') FROM decisions ORDER BY id", 6),
			"tasks":      queryDoltStrings(t, f.st, "SELECT id, COALESCE(title, ''), status, COALESCE(notes, '') FROM tasks ORDER BY id", 4),
			"documents":  queryDoltStrings(t, f.st, "SELECT id, path, title, content_hash FROM documents ORDER BY id", 4),
			"doc_chunks": queryDoltStrings(t, f.st, "SELECT id, doc_id, ord, COALESCE(heading_path, ''), COALESCE(body, '') FROM doc_chunks ORDER BY id", 5),
		},
	}
}

func (f *indexFixture) assertSnapshot(t *testing.T, before doltSnapshot) {
	t.Helper()
	after := f.snapshot(t)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("embedding lifecycle changed Dolt source rows, working set, or commit graph\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func queryDoltStrings(t *testing.T, st *localdolt.Store, query string, columns int) []string {
	t.Helper()
	rows, err := st.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query Dolt snapshot %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		values := make([]sql.NullString, columns)
		dest := make([]any, columns)
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan Dolt snapshot %q: %v", query, err)
		}
		parts := make([]string, columns)
		for i, value := range values {
			parts[i] = fmt.Sprintf("%t:%s", value.Valid, value.String)
		}
		result = append(result, strings.Join(parts, "\x00"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read Dolt snapshot %q: %v", query, err)
	}
	return result
}

func deterministicEmbedder(calls map[string]int) func(string) ([]float32, error) {
	return func(text string) ([]float32, error) {
		calls[text]++
		vector := make([]float32, EmbeddingDim)
		for i := range vector {
			vector[i] = float32(len(text)+i) / 1000
		}
		return vector, nil
	}
}

func totalCalls(calls map[string]int) int {
	total := 0
	for _, count := range calls {
		total += count
	}
	return total
}

func openSideStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open side-store %s: %v", path, err)
	}
	return db
}

func readStoredRows(t *testing.T, path string) map[string]storedRow {
	t.Helper()
	db := openSideStore(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT source_type, source_id, content_hash, dimension, hex(vector) FROM embeddings WHERE model_name = ? ORDER BY source_type, source_id", EmbeddingModelName)
	if err != nil {
		t.Fatalf("read stored embeddings: %v", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]storedRow{}
	for rows.Next() {
		var sourceType, sourceID string
		var row storedRow
		if err := rows.Scan(&sourceType, &sourceID, &row.hash, &row.dimension, &row.vector); err != nil {
			t.Fatalf("scan stored embedding: %v", err)
		}
		if row.dimension != EmbeddingDim {
			t.Errorf("%s/%s dimension = %d, want %d", sourceType, sourceID, row.dimension, EmbeddingDim)
		}
		result[sourceType+"/"+sourceID] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read stored embeddings: %v", err)
	}
	return result
}

func assertSideStoreSchema(t *testing.T, path string) {
	t.Helper()
	db := openSideStore(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query("PRAGMA table_info(embeddings)")
	if err != nil {
		t.Fatalf("read embeddings schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]int{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan embeddings schema: %v", err)
		}
		columns[name] = pk
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read embeddings schema: %v", err)
	}
	if got, want := columns, map[string]int{
		"source_type":  1,
		"source_id":    2,
		"model_name":   3,
		"vector":       0,
		"content_hash": 0,
		"dimension":    0,
	}; !reflect.DeepEqual(got, want) {
		keys := make([]string, 0, len(got))
		for key := range got {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		t.Fatalf("embeddings columns/pk = %#v (columns %v), want %#v", got, keys, want)
	}
}
