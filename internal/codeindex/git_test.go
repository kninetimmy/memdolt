package codeindex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func nativeGit(t *testing.T, root string, input []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "core.autocrlf=false"}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git %v: %v %s", args, err, out)
	}
	return out
}

func commitGitFixture(t *testing.T, root, author, date, subject string, parents ...string) string {
	t.Helper()
	when, err := time.Parse(time.RFC3339, date)
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(nativeGit(t, root, nil, "write-tree")))
	var raw strings.Builder
	fmt.Fprintf(&raw, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&raw, "parent %s\n", parent)
	}
	fmt.Fprintf(&raw, "author %s <fixture@example.invalid> %d %s\ncommitter Fixture <fixture@example.invalid> %d +0000\n\n%s\n", author, when.Unix(), when.Format("-0700"), when.Unix(), subject)
	hash := strings.TrimSpace(string(nativeGit(t, root, []byte(raw.String()), "hash-object", "-t", "commit", "-w", "--stdin")))
	nativeGit(t, root, nil, "update-ref", "HEAD", hash)
	return hash
}

func gitTableRows(t *testing.T, root, table string) [][]any {
	t.Helper()
	var result [][]any
	indexSQL(t, root, func(db *sql.DB) {
		// The table is a test-owned literal, never a production query input.
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			result = append(result, values)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	})
	return result
}

func TestGitIngestRangeRepeatPreservesVectorsAndDeletedFileHistory(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"src/旧 name.go": "package a\nfunc Before() {}\n", "delete.txt": "remove later"})
	first := commitGitFixture(t, root, "A\tB\x1fC", "2026-09-01T12:00:00+05:00", "Framed\t\x1f subject \"quote\"")
	writeTest(t, root, ".memdolt/config.toml", "[retrieval]\nmode='hybrid'\n")
	if _, err := Refresh(ctx, root, fakeInference{}); err != nil {
		t.Fatal(err)
	}
	// A real v1 schema contains no history objects. Upgrade must preserve every
	// existing source/chunk/vector value, including row IDs and vector bytes.
	execIndex(t, root, "DROP TABLE git_commit_files; DROP TABLE git_files; DROP TABLE git_commits; DROP TABLE git_ingestions; UPDATE index_meta SET value='1' WHERE key='schema_version'")
	source := map[string][][]any{}
	for _, table := range []string{"indexed_files", "code_chunks", "code_embeddings"} {
		source[table] = gitTableRows(t, root, table)
	}
	if history, err := ReadFileHistory(ctx, root, "src/旧 name.go", 10); err != nil || !history.Indexed || len(history.Results) != 0 {
		t.Fatalf("v1 cache search=%+v %v", history, err)
	}
	writeTest(t, root, "src/旧 name.go", "package a\nfunc After() {}\n")
	gitTest(t, root, "add", "--", "src/旧 name.go")
	second := commitGitFixture(t, root, "Editor", "2026-09-01T11:00:00-05:00", "Edit", first)
	gitTest(t, root, "mv", "--", "src/旧 name.go", "src/new name.go")
	gitTest(t, root, "rm", "--", "delete.txt")
	third := commitGitFixture(t, root, "Renamer", "2026-09-01T18:00:00+02:00", "Rename and delete", second)
	refs := nativeGit(t, root, nil, "show-ref", "--head")
	ranged, err := IngestGit(ctx, root, &first)
	if err != nil || !ranged.Committed || ranged.Head != third || ranged.Since == nil || *ranged.Since != first || ranged.CommitsSeen != 2 || ranged.UniqueFilesSeen != 3 || ranged.CommitFileLinksSeen != 3 {
		t.Fatalf("ranged ingest=%+v %v", ranged, err)
	}
	for table, before := range source {
		if after := gitTableRows(t, root, table); !reflect.DeepEqual(before, after) {
			t.Fatalf("history migration changed %s", table)
		}
	}
	for range 2 {
		full, err := IngestGit(ctx, root, nil)
		if err != nil || !full.Committed || full.CommitsSeen != 3 || full.CommitFileLinksSeen != 5 || full.UniqueFilesSeen != 3 {
			t.Fatalf("full/repeated ingest=%+v %v", full, err)
		}
	}
	for table, count := range map[string]int{"git_commits": 3, "git_files": 3, "git_commit_files": 5, "git_ingestions": 2} {
		if got := len(gitTableRows(t, root, table)); got != count {
			t.Fatalf("%s rows=%d, want %d", table, got, count)
		}
	}
	old, err := ReadFileHistory(ctx, root, "src/旧 name.go", 10)
	if err != nil || len(old.Results) != 2 || old.Results[0].CommitSHA != second || old.Results[1].CommitSHA != first || old.Results[1].Author != "A\tB\x1fC" || old.Results[1].Subject != "Framed\t\x1f subject \"quote\"" {
		t.Fatalf("offset/framed history=%+v %v", old, err)
	}
	for file, want := range map[string]string{"src/new name.go": "R", "delete.txt": "D"} {
		history, err := ReadFileHistory(ctx, root, file, 1)
		if err != nil || len(history.Results) != 1 || history.Results[0].ChangeType != want || history.Results[0].CommitSHA != third {
			t.Fatalf("%s history=%+v %v", file, history, err)
		}
	}
	if _, err := Refresh(ctx, root, fakeInference{}); err != nil {
		t.Fatal(err)
	}
	// A source refresh and even absent source bodies cannot erase history.
	t.Run("no Git or source body", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		got, err := ReadFileHistory(ctx, root, "src/旧 name.go", 10)
		if err != nil || !reflect.DeepEqual(old, got) {
			t.Fatalf("cached history after refresh=%+v %v", got, err)
		}
	})
	if !bytes.Equal(refs, nativeGit(t, root, nil, "show-ref", "--head")) {
		t.Fatal("code history changed native Git refs")
	}
	if result, err := Remove(ctx, root); err != nil || !result.Removed {
		t.Fatalf("remove=%+v %v", result, err)
	}
	if got, err := ReadFileHistory(ctx, root, "delete.txt", 10); err != nil || len(got.Results) != 0 || got.Coverage.Ranges != 0 {
		t.Fatalf("whole-index removal retained history=%+v %v", got, err)
	}
}

func TestGitHistoryTiesCopiesMergesEmptyRangesAndCacheCoverage(t *testing.T) {
	ctx := context.Background()
	body := strings.Repeat("unique original line\n", 40)
	root := testRepo(t, map[string]string{"source.txt": body})
	first := commitGitFixture(t, root, "Fixture", "2026-09-01T16:00:00Z", "First")
	writeTest(t, root, "copy.txt", body)
	writeTest(t, root, "source.txt", body+"extra\n")
	gitTest(t, root, "add", "--", "copy.txt", "source.txt")
	second := commitGitFixture(t, root, "Fixture", "2026-09-01T11:00:00-05:00", "Copy", first)
	third := commitGitFixture(t, root, "Fixture", "2026-09-01T18:00:00+02:00", "Empty merge", second, first)
	summary, err := IngestGit(ctx, root, nil)
	if err != nil || summary.CommitsSeen != 3 || !summary.Committed {
		t.Fatalf("copy/merge ingest=%+v %v", summary, err)
	}
	history, err := ReadFileHistory(ctx, root, "source.txt", 10)
	want := []string{first, second}
	slices.Sort(want)
	if err != nil || len(history.Results) != 2 || history.Results[0].CommitSHA != want[0] || history.Results[1].CommitSHA != want[1] {
		t.Fatalf("author instant ties=%+v %v", history, err)
	}
	copy, err := ReadFileHistory(ctx, root, "copy.txt", 10)
	if err != nil || len(copy.Results) != 1 || copy.Results[0].ChangeType != "C" {
		t.Fatalf("copy destination=%+v %v", copy, err)
	}
	empty, err := IngestGit(ctx, root, &third)
	if err != nil || !empty.Committed || empty.CommitsSeen != 0 || empty.Head != third {
		t.Fatalf("valid empty range=%+v %v", empty, err)
	}
	got, err := ReadFileHistory(ctx, root, "source.txt", 1)
	if err != nil || !got.Coverage.Cached || !got.Coverage.Truncated || got.Coverage.Ranges != 2 || got.Coverage.LastIngest.Since == nil || *got.Coverage.LastIngest.Since != third {
		t.Fatalf("cached ranges/limit=%+v %v", got, err)
	}
	commitGitFixture(t, root, "Fixture", "2026-09-02T00:00:00Z", "Not ingested", third)
	still, err := ReadFileHistory(ctx, root, "source.txt", 1)
	if err != nil || !reflect.DeepEqual(got, still) {
		t.Fatalf("search implicitly ingested or observed HEAD=%+v %v", still, err)
	}
}

func TestGitHistoryDenialsInvalidRevisionsAndAtomicFailures(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"allowed.txt": "body", ".env": "synthetic", "private/BLOCKED.txt": "synthetic", ".memdolt/hidden.txt": "synthetic"})
	first := commitGitFixture(t, root, "Fixture", "2026-09-01T00:00:00Z", "Allowed")
	writeTest(t, root, "allowed.txt", "changed")
	gitTest(t, root, "add", "--", "allowed.txt")
	second := commitGitFixture(t, root, "Fixture", "2026-09-02T00:00:00Z", "BLOCKED subject", first)
	writeTest(t, root, ".memdolt/config.toml", "[deny_list]\npatterns=['BLOCKED']\n")
	summary, err := IngestGit(ctx, root, nil)
	if err != nil || summary.DeniedCommits != 1 || summary.DeniedFilesSkipped != 4 || summary.CommitsIndexed != 1 || summary.CommitFileLinksSeen != 1 {
		t.Fatalf("denied ingest=%+v %v", summary, err)
	}
	for _, table := range []string{"git_commits", "git_files", "git_commit_files"} {
		if strings.Contains(fmt.Sprint(gitTableRows(t, root, table)), "BLOCKED") {
			t.Fatal("denied metadata reached SQLite")
		}
	}
	for _, path := range []string{".env", "private/BLOCKED.txt", ".memdolt/hidden.txt"} {
		if _, err := ReadFileHistory(ctx, root, path, 10); err == nil || strings.Contains(err.Error(), path) {
			t.Fatalf("denied path was returned or echoed: %v", err)
		}
	}
	before, err := os.ReadFile(filepath.Join(root, ".memdolt", indexName))
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"", " ", "--all", "-n1", "unavailable", "HEAD..HEAD", "HEAD:allowed.txt"} {
		if result, err := IngestGit(ctx, root, &revision); err == nil || result.Committed || result.OutcomeUnknown {
			t.Fatalf("invalid revision %q=%+v %v", revision, result, err)
		}
	}
	// Native Git tolerates NUL and invalid UTF-8 in raw commit objects; pretty
	// output can hide them. The whole ingestion must refuse before publication.
	tree := strings.TrimSpace(string(nativeGit(t, root, nil, "write-tree")))
	for _, bad := range []string{"NUL\x00hidden", "invalid\xffUTF8"} {
		raw := fmt.Sprintf("tree %s\nparent %s\nauthor Fixture <f@example.invalid> 1700000000 +0000\ncommitter Fixture <f@example.invalid> 1700000000 +0000\n\n%s\n", tree, second, bad)
		hash := strings.TrimSpace(string(nativeGit(t, root, []byte(raw), "hash-object", "--literally", "-t", "commit", "-w", "--stdin")))
		nativeGit(t, root, nil, "update-ref", "HEAD", hash)
		if result, err := IngestGit(ctx, root, nil); err == nil || result.Committed || strings.Contains(err.Error(), "hidden") {
			t.Fatalf("malformed native commit=%+v %v", result, err)
		}
	}
	nativeGit(t, root, nil, "update-ref", "HEAD", second)
	after, err := os.ReadFile(filepath.Join(root, ".memdolt", indexName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed Git observations changed the existing cache")
	}
	// Current rules also apply to previously cached metadata.
	writeTest(t, root, ".memdolt/config.toml", "[deny_list]\npatterns=['Allowed']\n")
	if result, err := ReadFileHistory(ctx, root, "allowed.txt", 10); err != nil || len(result.Results) != 0 || result.Coverage.DeniedResults != 1 {
		t.Fatalf("read-time metadata deny=%+v %v", result, err)
	}
}

func TestGitHistoryBusyForeignSchemaAndFinalizationControls(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"source.go": "package a\nfunc A() {}"})
	head := commitGitFixture(t, root, "Fixture", "2026-09-01T00:00:00Z", "First")
	if _, err := IngestGit(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	r, err := openRepository(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.acquire(); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestGit(ctx, root, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy ingest=%v", err)
	}
	if _, err := ReadFileHistory(ctx, root, "source.go", 10); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy search=%v", err)
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"CREATE TABLE foreign_data(value TEXT)", "UPDATE index_meta SET value='99' WHERE key='schema_version'"} {
		t.Run(query, func(t *testing.T) {
			other := testRepo(t, map[string]string{"a.txt": "A"})
			commitGitFixture(t, other, "Fixture", "2026-09-01T00:00:00Z", "First")
			if _, err := IngestGit(ctx, other, nil); err != nil {
				t.Fatal(err)
			}
			execIndex(t, other, query)
			before, err := os.ReadFile(filepath.Join(other, ".memdolt", indexName))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := IngestGit(ctx, other, nil); err == nil {
				t.Fatal("foreign/unsupported cache was accepted")
			}
			if _, err := ReadFileHistory(ctx, other, "a.txt", 10); err == nil {
				t.Fatal("foreign/unsupported cache was searched")
			}
			after, err := os.ReadFile(filepath.Join(other, ".memdolt", indexName))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("foreign/unsupported cache changed")
			}
		})
	}
	indexSQL(t, root, func(db *sql.DB) {
		for _, commitFirst := range []bool{false, true} {
			summary := GitIngestSummary{GitRange: GitRange{Head: head, ObservedAt: time.Now().UTC()}}
			result, err := publishGitHistory(ctx, db, summary, nil, func(tx *sql.Tx) error {
				if commitFirst {
					if err := tx.Commit(); err != nil {
						return err
					}
				}
				return errors.New("injected unobserved finalization")
			})
			if err == nil || result.Committed || !result.OutcomeUnknown {
				t.Fatalf("unobserved commit=%+v %v", result, err)
			}
		}
	})
}

func TestGitFramingPreservesDelimitersAndRefusesMalformedObservations(t *testing.T) {
	hash := strings.Repeat("a", 40)
	metadata := []byte(hash + "\x00A\tB\x1fC\x002026-09-01T00:00:00Z\x00Subject\t\x1f\ncontinued\x00")
	commits, err := parseGitMetadata(metadata, []string{hash})
	if err != nil || commits[0].Author != "A\tB\x1fC" || commits[0].Subject != "Subject\t\x1f\ncontinued" {
		t.Fatalf("metadata framing=%+v %v", commits, err)
	}
	path := "dir/雪\t\n\x1f name.txt"
	if err := parseGitChanges([]byte(hash+"\x00R100\x00old\tname\x00"+path+"\x00"), commits); err != nil || commits[0].Files[0].Path != path || commits[0].Files[0].Source != "old\tname" {
		t.Fatalf("path framing=%+v %v", commits, err)
	}
	for _, raw := range []string{hash + "\x00R100\x00old\x00", hash + "\x00M\x00missing-final-nul", hash + "\x00A\x00bad\xff\x00", hash + "\x00X\x00path\x00", hash + "\x00M100\x00path\x00"} {
		if err := parseGitChanges([]byte(raw), []gitCommit{{SHA: hash}}); err == nil {
			t.Fatalf("malformed framing accepted: %q", raw)
		}
	}
	for _, raw := range [][]byte{metadata[:len(metadata)-1], bytes.ReplaceAll(metadata, []byte("2026-09-01T00:00:00Z"), []byte("invalid")), append([]byte{0xff}, metadata...)} {
		if _, err := parseGitMetadata(raw, []string{hash}); err == nil {
			t.Fatal("malformed metadata accepted")
		}
	}
}

func TestGitHistoryOwnerAliasesAndV1NameCollisionsRefuse(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"alias.txt": "safe original"})
	commitGitFixture(t, root, "Fixture", "2026-09-01T00:00:00Z", "First")
	if _, err := IngestGit(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, ".memdolt/server.pid", "synthetic owner credential")
	if err := os.Remove(filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, ".memdolt/server.pid"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHistory(ctx, root, "alias.txt", 10); err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("owner alias search=%v", err)
	}
	if summary, err := IngestGit(ctx, root, nil); err != nil || summary.DeniedFilesSkipped != 1 || summary.CommitFileLinksSeen != 0 {
		t.Fatalf("owner alias ingest=%+v %v", summary, err)
	}
	if err := os.Remove(filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, "alias.txt", "safe original")
	if err := os.Remove(filepath.Join(root, ".memdolt/server.pid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".memdolt/server.pid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestGit(ctx, root, nil); err == nil {
		t.Fatal("unverifiable owner protection was ignored")
	}
	other := testRepo(t, map[string]string{"a.go": "package a\nfunc A() {}"})
	commitGitFixture(t, other, "Fixture", "2026-09-01T00:00:00Z", "First")
	if _, err := Refresh(ctx, other, nil); err != nil {
		t.Fatal(err)
	}
	execIndex(t, other, "DROP TABLE git_commit_files; DROP TABLE git_files; DROP TABLE git_commits; DROP TABLE git_ingestions; UPDATE index_meta SET value='1' WHERE key='schema_version'; CREATE TABLE git_commits(keep TEXT)")
	if _, err := IngestGit(ctx, other, nil); err == nil {
		t.Fatal("v1 migration reused a foreign history-name collision")
	}
	if got := gitTableRows(t, other, "code_chunks"); len(got) != 1 {
		t.Fatal("refused migration changed source chunks")
	}
}

func TestGitHistoryRejectsForeignSQLitePrefixLookalikes(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{1, 2} {
		for _, kind := range []string{"table", "trigger"} {
			t.Run(fmt.Sprintf("v%d/%s", version, kind), func(t *testing.T) {
				root := testRepo(t, map[string]string{"source.go": "package a\nfunc KeptSource() {}\n"})
				first := commitGitFixture(t, root, "Fixture", "2026-09-01T00:00:00Z", "First")
				writeTest(t, root, ".memdolt/config.toml", "[retrieval]\nmode='hybrid'\n")
				if _, err := Refresh(ctx, root, fakeInference{}); err != nil {
					t.Fatal(err)
				}
				if _, err := IngestGit(ctx, root, nil); err != nil {
					t.Fatal(err)
				}
				commitGitFixture(t, root, "Fixture", "2026-09-02T00:00:00Z", "Not yet cached", first)
				if version == 1 {
					execIndex(t, root, "DROP TABLE git_commit_files; DROP TABLE git_files; DROP TABLE git_commits; DROP TABLE git_ingestions; UPDATE index_meta SET value='1' WHERE key='schema_version'")
				}
				// Real SQLite internal indexes/statistics and FTS shadow tables
				// remain valid in both supported schemas.
				execIndex(t, root, "ANALYZE")
				indexSQL(t, root, func(db *sql.DB) {
					if err := checkOwnedSchema(ctx, db); err != nil {
						t.Fatalf("refused legitimate SQLite internals: %v", err)
					}
				})
				foreign := "CREATE TABLE sqlitex_unrelated(value TEXT); INSERT INTO sqlitex_unrelated VALUES ('keep foreign data')"
				if kind == "trigger" {
					foreign = "CREATE TRIGGER sqlitex_side_effect AFTER INSERT ON git_commits BEGIN DELETE FROM code_chunks; END"
					if version == 1 {
						foreign = "CREATE TRIGGER sqlitex_side_effect AFTER UPDATE ON index_meta BEGIN DELETE FROM code_chunks; END"
					}
				}
				execIndex(t, root, foreign)
				source := map[string][][]any{}
				for _, table := range []string{"indexed_files", "code_chunks", "code_embeddings"} {
					source[table] = gitTableRows(t, root, table)
					if len(source[table]) != 1 {
						t.Fatalf("fixture %s needs one real row", table)
					}
				}
				cache := filepath.Join(root, ".memdolt", indexName)
				before, err := os.ReadFile(cache)
				if err != nil {
					t.Fatal(err)
				}
				for _, operation := range []struct {
					name string
					run  func() error
				}{
					{"ingest", func() error {
						result, err := IngestGit(ctx, root, nil)
						if result.Committed || result.OutcomeUnknown {
							t.Errorf("unsafe ingest reported publication: %+v", result)
						}
						return err
					}},
					{"file history", func() error { _, err := ReadFileHistory(ctx, root, "source.go", 10); return err }},
					{"refresh", func() error { _, err := Refresh(ctx, root, fakeInference{}); return err }},
					{"locate", func() error { _, err := Locate(ctx, root, fakeInference{}, Options{Query: "KeptSource"}); return err }},
					{"remove", func() error { _, err := Remove(ctx, root); return err }},
				} {
					if err := operation.run(); err == nil || !strings.Contains(err.Error(), "unrelated schema objects") {
						t.Errorf("%s failed to refuse foreign %s before use: %v", operation.name, kind, err)
					}
					for table, rows := range source {
						if after := gitTableRows(t, root, table); !reflect.DeepEqual(rows, after) {
							t.Errorf("%s changed %s: %d rows before, %d after", operation.name, table, len(rows), len(after))
						}
					}
					if after, err := os.ReadFile(cache); err != nil || !bytes.Equal(before, after) {
						t.Fatalf("%s changed the complete cache, including foreign objects: %v", operation.name, err)
					}
				}
			})
		}
	}
}
