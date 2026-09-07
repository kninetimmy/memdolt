package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

func testRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	gitTest(t, root, "init", "--quiet")
	for path, body := range files {
		writeTest(t, root, path, body)
		gitTest(t, root, "add", "--", path)
	}
	return root
}

func gitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	args = append([]string{"-C", root, "-c", "core.autocrlf=false"}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git: %v: %s", err, out)
	}
}

func writeTest(t *testing.T, root, path, body string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func indexSQL(t *testing.T, root string, action func(*sql.DB)) {
	t.Helper()
	r, err := openRepository(root, false)
	if err != nil {
		t.Fatal(err)
	}
	db, _, err := r.openDB(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	action(db)
	if err := errors.Join(db.Close(), r.close()); err != nil {
		t.Fatal(err)
	}
}

func execIndex(t *testing.T, root, query string, args ...any) {
	t.Helper()
	indexSQL(t, root, func(db *sql.DB) {
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIndexLazyRefreshAndExplicitNoRefresh(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"src/a.go": "package a\nfunc Old() {}\n"})
	status, err := Status(ctx, root)
	if err != nil || status.Exists {
		t.Fatalf("missing status=%+v %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".memdolt")); !os.IsNotExist(err) {
		t.Fatalf("status created metadata: %v", err)
	}
	first, err := Locate(ctx, root, nil, Options{Query: "Old"})
	if err != nil || len(first.Results) != 1 || first.Refresh.NewFiles != 1 {
		t.Fatalf("lazy locate=%+v %v", first, err)
	}
	info, err := os.Stat(filepath.Join(root, "src/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, "src/a.go", "package a\nfunc New() {}\n")
	if err := os.Chtimes(filepath.Join(root, "src/a.go"), time.Now(), info.ModTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// No-refresh must require no git executable while retaining all read guards.
	t.Run("no git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		stale, err := Locate(ctx, root, nil, Options{Query: "Old", NoRefresh: true})
		if err != nil || len(stale.Results) != 1 || *stale.Results[0].Symbol != "Old" || !strings.Contains(stale.Results[0].Snippet, "New") || stale.Refresh != nil {
			t.Fatalf("no-refresh=%+v %v", stale, err)
		}
	})
	fresh, err := Locate(ctx, root, nil, Options{Query: "New"})
	if err != nil || len(fresh.Results) != 1 || *fresh.Results[0].Symbol != "New" || fresh.Refresh.ChangedFiles != 1 {
		t.Fatalf("fresh=%+v %v", fresh, err)
	}
	info, err = os.Stat(filepath.Join(root, "src/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, "src/a.go", "package a\nfunc Old() {}\n")
	if err := os.Chtimes(filepath.Join(root, "src/a.go"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	same, err := Refresh(ctx, root, nil)
	if err != nil || same.UnchangedFiles != 1 {
		t.Fatalf("documented metadata fast path=%+v %v", same, err)
	}
	if err := os.Chtimes(filepath.Join(root, "src/a.go"), time.Now(), info.ModTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	changed, err := Refresh(ctx, root, nil)
	if err != nil || changed.ChangedFiles != 1 {
		t.Fatalf("hash refresh=%+v %v", changed, err)
	}
	if err := os.Chtimes(filepath.Join(root, "src/a.go"), time.Now(), info.ModTime().Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	touched, err := Refresh(ctx, root, nil)
	if err != nil || touched.UnchangedFiles != 1 || touched.ChangedFiles != 0 {
		t.Fatalf("hash same=%+v %v", touched, err)
	}
}

func TestRefreshCountsPruneDeletedRenamedBinaryAndDeniedSources(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{
		"src/a.rs": "fn alpha() {}\n", "src/b.rs": "// fixture_marker\nfn beta() {}\n",
		"binary.go": string([]byte{0xff, 0}), "README.md": "text", "dist/x.min.js": "function x() {}",
		"nested/secrets/token.go": "fixture secret", "nested/.env.go": "fixture secret", "vendor/plain.js": "function plain() {}",
		" spaced.txt": "package a\nfunc Spaced() {}",
	})
	writeTest(t, root, "untracked.go", "package a\nfunc Untracked() {}")
	s, err := Refresh(ctx, root, nil)
	if err != nil || s.NewFiles != 4 || s.ExcludedFiles != 3 || s.DeniedFiles != 2 || s.BinarySkipped != 1 || s.FilesTotal != 4 {
		t.Fatalf("initial=%+v %v", s, err)
	}
	if s.NewFiles+s.ChangedFiles+s.UnchangedFiles+s.SkippedFiles+s.ExcludedFiles+s.DeniedFiles != s.TrackedTotal {
		t.Fatal("counts do not reconcile")
	}
	if err := os.Rename(filepath.Join(root, "src/a.rs"), filepath.Join(root, "src/renamed.rs")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "rm", "--cached", "--", "src/a.rs")
	gitTest(t, root, "add", "--", "src/renamed.rs")
	if err := os.Remove(filepath.Join(root, "vendor/plain.js")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, ".memdolt/config.toml", "[deny_list]\npatterns = ['fixture_marker']\n")
	s, err = Refresh(ctx, root, nil)
	if err != nil || s.NewFiles != 1 || s.DeletedFiles != 3 || s.DeniedFiles != 3 || s.SkippedFiles != 1 || s.FilesTotal != 2 {
		t.Fatalf("pruned=%+v %v", s, err)
	}
	indexSQL(t, root, func(db *sql.DB) {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM code_chunks WHERE embed_text LIKE '%fixture_marker%' OR embed_text LIKE '%plain%'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("retained stale chunks=%d %v", count, err)
		}
	})
	writeTest(t, root, "src/renamed.rs", string([]byte{0xff, 0, 1}))
	s, err = Refresh(ctx, root, nil)
	if err != nil || s.ChangedFiles != 1 || s.BinarySkipped != 1 || s.ChunksTotal != 0 {
		t.Fatalf("binary replacement=%+v %v", s, err)
	}
}

func TestOwnerAliasesAndTamperedRowsNeverReachSnippets(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"alias.go": "package a\nfunc Canary() {}\n"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	writeTest(t, root, ".memdolt/server.pid", "synthetic-owner-capability-do-not-index")
	if err := os.Remove(filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, ".memdolt/server.pid"), filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	result, err := Locate(ctx, root, nil, Options{Query: "Canary", NoRefresh: true})
	if !errors.Is(err, layout.ErrOwnerSource) || len(result.Results) != 0 {
		t.Fatalf("owner alias=%+v %v", result, err)
	}
	summary, err := Refresh(ctx, root, nil)
	if err != nil || summary.DeniedFiles != 1 || summary.DeletedFiles != 1 || summary.ChunksTotal != 0 {
		t.Fatalf("owner cleanup=%+v %v", summary, err)
	}
	// Required verification cannot degrade to "ordinary different file".
	if err := os.Remove(filepath.Join(root, ".memdolt/server.pid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".memdolt/server.pid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(ctx, root, nil); err == nil {
		t.Fatal("unverifiable owner metadata was ignored")
	}
	for _, path := range []string{"../outside.go", "/outside.go", "C:/outside.go", "sub/../alias.go", "sub\\alias.go", ".memdolt/server.pid", "alias.go:stream"} {
		t.Run(path, func(t *testing.T) {
			root := testRepo(t, map[string]string{"safe.go": "package a\nfunc Canary() {}"})
			if _, err := Refresh(ctx, root, nil); err != nil {
				t.Fatal(err)
			}
			execIndex(t, root, "UPDATE indexed_files SET path=?", path)
			if response, err := Locate(ctx, root, nil, Options{Query: "Canary", NoRefresh: true}); err == nil || len(response.Results) != 0 {
				t.Fatalf("tampered row=%+v %v", response, err)
			}
		})
	}
}

func TestSymlinkAndUnreadableSourcesArePruned(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"src/a.go": "package a\nfunc Canary() {}\n"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	writeTest(t, out, "outside.go", "package a\nfunc Outside() {}")
	if err := os.Remove(filepath.Join(root, "src/a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(out, "outside.go"), filepath.Join(root, "src/a.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Locate(ctx, root, nil, Options{Query: "Canary", NoRefresh: true}); err == nil {
		t.Fatal("no-refresh read a symlink")
	}
	s, err := Refresh(ctx, root, nil)
	if err != nil || s.SkippedFiles != 1 || s.DeletedFiles != 1 || s.ChunksTotal != 0 {
		t.Fatalf("symlink=%+v %v", s, err)
	}
	// A linked ancestor, including an in-root link, must be rejected too.
	if err := os.Symlink(out, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	r, err := openRepository(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if f, _, err := r.openSource("linked/outside.go"); err == nil {
		_ = f.Close()
		t.Fatal("linked ancestor read")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	// Non-regular replacements exercise the same skip/prune branch on all OSes.
	root = testRepo(t, map[string]string{"a.go": "package a\nfunc A() {}"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "a.go"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err = Refresh(ctx, root, nil)
	if err != nil || s.DeletedFiles != 1 || s.SkippedFiles != 1 {
		t.Fatalf("nonregular=%+v %v", s, err)
	}
}

func TestIndexStatusRebuildRemovalAndForeignFilePreservation(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"a.go": "package a\nfunc A() {}"})
	if _, err := Refresh(ctx, root, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"embeddings.sqlite", "user.sqlite", "server.pid"} {
		writeTest(t, root, ".memdolt/"+name, "synthetic independent data")
	}
	execIndex(t, root, "UPDATE index_meta SET value='incompatible' WHERE key='schema_version'")
	before, err := os.ReadFile(filepath.Join(root, ".memdolt", indexName))
	if err != nil {
		t.Fatal(err)
	}
	status, err := Status(ctx, root)
	if err != nil || !status.Exists || !status.NeedsRebuild {
		t.Fatalf("schema status=%+v %v", status, err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".memdolt", indexName))
	if err != nil || hashBytes(before) != hashBytes(after) {
		t.Fatal("status modified the index")
	}
	if _, err := Locate(ctx, root, nil, Options{Query: "A", NoRefresh: true}); err == nil {
		t.Fatal("no-refresh rebuilt an incompatible schema")
	}
	if s, err := Refresh(ctx, root, nil); err != nil || s.NewFiles != 1 {
		t.Fatalf("rebuild=%+v %v", s, err)
	}
	if result, err := Remove(ctx, root); err != nil || !result.Removed {
		t.Fatalf("remove=%+v %v", result, err)
	}
	if result, err := Remove(ctx, root); err != nil || result.Removed {
		t.Fatalf("second remove=%+v %v", result, err)
	}
	for _, name := range []string{"embeddings.sqlite", "user.sqlite", "server.pid"} {
		raw, err := os.ReadFile(filepath.Join(root, ".memdolt", name))
		if err != nil || string(raw) != "synthetic independent data" {
			t.Fatalf("changed unrelated file %s: %v", name, err)
		}
	}
	writeTest(t, root, ".memdolt/"+indexName, "unrelated occupant")
	if _, err := Refresh(ctx, root, nil); err == nil {
		t.Fatal("overwrote unknown occupant")
	}
	if _, err := Remove(ctx, root); err == nil {
		t.Fatal("removed unknown occupant")
	}
	if err := os.Remove(filepath.Join(root, ".memdolt", indexName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, ".memdolt/embeddings.sqlite"), filepath.Join(root, ".memdolt", indexName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(ctx, root, nil); err == nil {
		t.Fatal("wrote through database hardlink")
	}
}

type fakeInference struct {
	embed func(string) ([]float32, error)
}

func (f fakeInference) Embed(text string) ([]float32, error) {
	if f.embed != nil {
		return f.embed(text)
	}
	v := make([]float32, embedding.EmbeddingDim)
	v[0] = 1
	return v, nil
}
func (f fakeInference) Rerank(string, string) (float32, error) { return -1, nil }

func TestVectorCorruptionRepairAndConcurrentFailurePreservation(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t, map[string]string{"a.go": "package a\nfunc Canary() {}"})
	writeTest(t, root, ".memdolt/config.toml", "[retrieval]\nmode='hybrid'\n")
	if _, err := Refresh(ctx, root, fakeInference{}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE code_embeddings SET model_name='old'",
		"UPDATE code_embeddings SET dimension=3",
		"UPDATE code_embeddings SET content_hash='old'",
		"UPDATE code_embeddings SET vector=x'0000803f'",
		"UPDATE code_embeddings SET vector_hash='corrupt'",
	} {
		execIndex(t, root, query)
		response, err := Locate(ctx, root, fakeInference{}, Options{Query: "Canary", NoRefresh: true})
		if err != nil || response.CorruptEmbeddings != 1 || len(response.Results) != 1 || response.Results[0].VectorScore != 0 {
			t.Fatalf("bad vector used: %+v %v", response, err)
		}
		if summary, err := Refresh(ctx, root, fakeInference{}); err != nil || summary.EmbeddedChunks != 1 || summary.UnchangedFiles != 1 {
			t.Fatalf("vector repair=%+v %v", summary, err)
		}
	}
	writeTest(t, root, "a.go", "package a\nfunc ChangedCanary() {}")
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Refresh(ctx, root, fakeInference{embed: func(string) ([]float32, error) {
			close(entered)
			<-release
			return nil, errors.New("injected embedding failure")
		}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh did not reach embedding")
	}
	if _, err := Remove(ctx, root); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent removal=%v", err)
	}
	if _, err := Locate(ctx, root, fakeInference{}, Options{Query: "ChangedCanary", NoRefresh: true}); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent query=%v", err)
	}
	if _, err := Refresh(ctx, root, fakeInference{}); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent refresh=%v", err)
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "injected embedding failure") {
		t.Fatalf("embed failure=%v", err)
	}
	status, err := Status(ctx, root)
	if err != nil || status.FilesTotal != 1 || status.ChunksTotal != 1 || status.EmbeddingsTotal != 0 {
		t.Fatalf("failed-vector state=%+v %v", status, err)
	}
	writeTest(t, root, ".memdolt/config.toml", "[retrieval]\nmode='fts'\n")
	response, err := Locate(ctx, root, nil, Options{Query: "ChangedCanary", NoRefresh: true})
	if err != nil || len(response.Results) != 1 {
		t.Fatalf("committed FTS not preserved=%+v %v", response, err)
	}
}

func TestIndependentConfigNormalizationAndHarnessFloor(t *testing.T) {
	root := testRepo(t, map[string]string{"a.go": "package a\nfunc Canary() {}"})
	writeTest(t, root, ".memdolt/config.toml", "[retrieval]\nmode='fts'\n[retrieval.scoring]\nfts_weight=0.1\nvector_weight=0.9\n[code_index]\nfts_weight=0.8\nvector_weight=0.2\ntest_path_penalty=0.5\n")
	response, err := Locate(context.Background(), root, nil, Options{Query: "Canary"})
	if err != nil || len(response.Results) != 1 || response.Results[0].Score != 0.8 {
		t.Fatalf("config=%+v %v", response, err)
	}
	recall, err := retrieval.LoadConfig(filepath.Join(root, ".memdolt/config.toml"))
	if err != nil || recall.Scoring.FTSWeight != 0.1 || recall.Scoring.VectorWeight != 0.9 {
		t.Fatalf("recall retuned=%+v %v", recall, err)
	}
	for _, config := range []string{"[code_index]\nfts_weight=nan", "[code_index]\nfts_weight=2", "[code_index]\nunknown=1", "[deny_list]\npattern=['x']", "[deny_list]\npatterns=['[']"} {
		writeTest(t, root, ".memdolt/config.toml", config)
		if _, err := Locate(context.Background(), root, nil, Options{Query: "Canary", NoRefresh: true}); err == nil {
			t.Fatalf("invalid config accepted: %s", config)
		}
	}
	if got := ftsMatch(`'parse' "manifest";`); got != `"parse" AND "manifest"` {
		t.Fatalf("fts expression: %s", got)
	}
	if normalizeFTS(-5, -5, -5) != 1 || normalizeFTS(0, -10, 10) != 0.5 {
		t.Fatal("baseline normalization changed")
	}
	for _, path := range []string{"tests/a.go", "benches/a.go", "examples/a.go"} {
		if !isTestPath(path) {
			t.Fatal(path)
		}
	}
	if isTestPath("src/tests/a.go") {
		t.Fatal("broadened test-path penalty")
	}
	n := float32(-1)
	floor := float32(0)
	q := GoldenQuery{ID: "empty", Query: "nonsense", Kind: "empty"}
	r := Response{Results: []Hit{{Path: "a.go", RerankScore: &n}}}
	if EvaluateQuery(q, r, 3, &floor).Passed {
		t.Fatal("floor applied without actual reranking")
	}
	r.Reranked = true
	if !EvaluateQuery(q, r, 3, &floor).Passed {
		t.Fatal("harness floor did not reject")
	}
}
