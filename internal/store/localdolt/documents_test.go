package localdolt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/oklog/ulid/v2"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
)

func TestDocumentMarkdownMemhubV020Cases(t *testing.T) {
	// The first three inputs are verbatim tagged-baseline tests from
	// memhub v0.2.0 src/commands/doc.rs, not newly invented Markdown rules.
	for _, tc := range []struct {
		name, input, title string
		want               []documentSection
	}{
		{"headings", "# Top\n\nintro\n\n## Alpha\n\nbody a\n\n### Nested\n\ndeep\n\n## Beta\n\nbody b\n", "Top", []documentSection{
			{"Top", "# Top\n\nintro"}, {"Top > Alpha", "## Alpha\n\nbody a"},
			{"Top > Alpha > Nested", "### Nested\n\ndeep"}, {"Top > Beta", "## Beta\n\nbody b"},
		}},
		{"fence", "## Code\n\n```yaml\n# this is not a heading\nkey: value\n```\n\nafter\n", "Code", []documentSection{
			{"Code", "## Code\n\n```yaml\n# this is not a heading\nkey: value\n```\n\nafter"},
		}},
		{"preamble", "---\nname: spec\n---\n\nlead prose\n\n# First\n\nbody\n", "First", []documentSection{
			{"", "---\nname: spec\n---\n\nlead prose"}, {"First", "# First\n\nbody"},
		}},
		{"empty", " \n\r\n\u2003", "fixture.md", nil},
		{"unicode paragraph target", "# Ω\n\n" + strings.Repeat("界", 1000) + "\n\n" + strings.Repeat("語", 1000), "Ω", []documentSection{
			{"Ω", "# Ω\n\n" + strings.Repeat("界", 1000)}, {"Ω", strings.Repeat("語", 1000)},
		}},
		{"single paragraph exceeds soft target", strings.Repeat("a", 2200), "fixture.md", []documentSection{{"", strings.Repeat("a", 2200)}}},
		{"oversized fence", "# Code\n\n```\n" + strings.Repeat("a", 2200) + "\n\n# hidden\n```\n\nafter", "Code", []documentSection{
			{"Code", "# Code"}, {"Code", "```\n" + strings.Repeat("a", 2200) + "\n\n# hidden\n```"}, {"Code", "after"},
		}},
		{"family closes shorter fence", "~~~lang\n# hidden\n```\n## still hidden\n~~~~\n# Visible ###\n", "Visible", []documentSection{
			{"", "~~~lang\n# hidden\n```\n## still hidden\n~~~~"}, {"Visible", "# Visible ###"},
		}},
		{"ATX quirks and CRLF", "#no-space\r\n####### too many\r\n #\tLeaf###\r\nbody\rline\r\n", "Leaf", []documentSection{
			{"", "#no-space\n####### too many"}, {"Leaf", "#\tLeaf###\nbody\rline"},
		}},
		{"heading leaf", "# Parent > Leaf\n", "Leaf", []documentSection{{"Parent > Leaf", "# Parent > Leaf"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkMarkdown(tc.input)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("chunks = %#v, want %#v", got, tc.want)
			}
			if title := documentTitle(got, "fixture.md"); title != tc.title {
				t.Fatalf("title = %q, want %q", title, tc.title)
			}
		})
	}
}

func documentFixture(t *testing.T) (*Store, string) {
	t.Helper()
	st := openInternalTestStore(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st, filepath.Join(st.paths.Base(), "spec.md")
}

func writeDocumentFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type documentInference struct{}

func (documentInference) Embed(string) ([]float32, error) {
	vector := make([]float32, embedding.EmbeddingDim)
	vector[0] = 1
	return vector, nil
}

func (documentInference) Rerank(string, string) (float32, error) { return 1, nil }

func TestDocumentsCommittedLifecycleAndRetrieval(t *testing.T) {
	ctx := context.Background()
	st, file := documentFixture(t)
	actor, err := memory.NormalizeActor("Claude Code")
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := st.ProposeFact(ctx, Proposal{Actor: actor.CommitAuthor(), Rationale: "keep this pending", Target: TargetRepo}, Fact{Key: "doc.pending", Value: "not a document"})
	if err != nil {
		t.Fatal(err)
	}
	writeDocumentFixture(t, st.paths.ConfigFile(), "[retrieval]\nmode='fts'\ninclude_docs_in_default=false\n[retrieval.scoring]\ndoc_min_rerank_score=0.25\n[custom]\nkeep=['one', 'two']\n[doc]\nallowed_dirs=[]\n")
	const original = "# Title\n\n## Alpha\n\nquasarwhistle alpha\n\n## Beta\n\nretiredbeacon beta\n"
	writeDocumentFixture(t, file, original)
	commits := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	added, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if added.Status != "created" || added.Commit == "" || !added.EnabledDefaultRecall || added.Document == nil || added.Document.ChunkCount != 3 || added.Document.Source != "user" {
		t.Fatalf("add = %+v", added)
	}
	if added.Document.ContentHash != fmt.Sprintf("%x", sha256.Sum256([]byte(original))) || added.Document.ByteLen != int64(len(original)) {
		t.Fatalf("wrong hash or byte length: %+v", added.Document)
	}
	for i, chunk := range added.Chunks {
		if _, err := ulid.ParseStrict(chunk.ID); err != nil || chunk.DocID != added.Document.ID || chunk.Ord != i {
			t.Fatalf("chunk identity/order = %+v, %v", chunk, err)
		}
	}
	var author string
	if err := st.db.QueryRow("SELECT committer FROM dolt_log WHERE commit_hash = ?", added.Commit).Scan(&author); err != nil || author != actor.Name {
		t.Fatalf("commit attribution = %s, %v", author, err)
	}
	var config map[string]any
	if _, err := toml.DecodeFile(st.paths.ConfigFile(), &config); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config["custom"], map[string]any{"keep": []any{"one", "two"}}) {
		t.Fatalf("custom configuration lost: %#v", config)
	}
	cfg, err := retrieval.LoadConfig(st.paths.ConfigFile())
	if err != nil || !cfg.IncludeDocsInDefault || cfg.Scoring.DocMinRerankScore != 0.25 {
		t.Fatalf("retrieval config = %+v, %v", cfg, err)
	}
	// Existing scoring policy excludes default docs without a reranker. An
	// explicit doc_chunk filter still exposes the production FTS source.
	defaultFTS, err := retrieval.Run(ctx, st, st.paths.Base(), retrieval.Options{Query: "quasarwhistle", Mode: retrieval.ModeFTS})
	if err != nil || defaultFTS.CandidateCount != 1 || defaultFTS.ReturnedCount != 0 || defaultFTS.AvailableDocs != 3 {
		t.Fatalf("default FTS scoring policy changed: %+v, %v", defaultFTS, err)
	}
	fts, err := retrieval.Run(ctx, st, st.paths.Base(), retrieval.Options{Query: "quasarwhistle", Mode: retrieval.ModeFTS, SourceTypes: []string{"doc_chunk"}, Provenance: true})
	if err != nil || fts.ReturnedCount == 0 || fts.Results[0].SourceType != "doc_chunk" || fts.Results[0].LastChanged == nil || fts.Results[0].LastChanged.Author != actor.Name {
		t.Fatalf("production default recall = %+v, %v", fts, err)
	}
	shown, err := st.DocShow(ctx, added.Document.ID)
	if err != nil || !reflect.DeepEqual(shown.Document, added.Document) || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
		t.Fatalf("show = %+v, %v; add = %+v", shown, err, added)
	}
	again, err := st.DocAdd(ctx, DocAddOptions{File: filepath.Join(st.paths.Base(), ".", "spec.md"), Title: "not a metadata-only edit", Actor: actor})
	if err != nil || again.Status != "unchanged" || again.Commit != "" || !reflect.DeepEqual(again.Document, added.Document) {
		t.Fatalf("unchanged = %+v, %v", again, err)
	}
	if got := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log"); got != commits+1 {
		t.Fatalf("unchanged ingestion added a commit: %d -> %d", commits, got)
	}
	sources, err := st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inference := documentInference{}
	if _, err := embedding.Rebuild(ctx, st.paths.EmbeddingsFile(), sources, inference.Embed); err != nil {
		t.Fatal(err)
	}
	cfg.Mode = retrieval.ModeHybrid
	hybrid, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), inference, cfg, retrieval.Options{Query: "quasarwhistle"})
	if err != nil || hybrid.ReturnedCount != 3 || len(hybrid.Warnings) != 0 {
		t.Fatalf("indexed production recall = %+v, %v", hybrid, err)
	}
	// A deliberate opt-out survives both unchanged and changed ingestion.
	const optedOut = "[retrieval]\ninclude_docs_in_default=false\n"
	writeDocumentFixture(t, st.paths.ConfigFile(), optedOut)
	if _, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: actor}); err != nil {
		t.Fatal(err)
	}
	writeDocumentFixture(t, file, "# Replacement\n\nnewbeacononly\n")
	updated, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: actor})
	if err != nil || updated.Status != "updated" || updated.Document.ID != added.Document.ID || updated.Document.ChunkCount != 1 || updated.EnabledDefaultRecall {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	raw, err := os.ReadFile(st.paths.ConfigFile())
	if err != nil || string(raw) != optedOut {
		t.Fatalf("opt-out rewritten: %s, %v", raw, err)
	}
	sources, err = st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := embedding.Status(ctx, st.paths.EmbeddingsFile(), sources)
	if err != nil || status.Missing != 1 || status.Orphaned != 3 || status.Current != 0 || !status.NeedsRebuild {
		t.Fatalf("replacement index status = %+v, %v", status, err)
	}
	retired, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), inference, cfg, retrieval.Options{Query: "retiredbeacon"})
	if err != nil || retired.ReturnedCount != 0 || len(retired.Warnings) == 0 {
		t.Fatalf("retired content/vector survived = %+v, %v", retired, err)
	}
	current, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), inference, cfg, retrieval.Options{Query: "newbeacononly", SourceTypes: []string{"doc_chunk"}})
	if err != nil || current.ReturnedCount != 1 || current.Results[0].SourceID != updated.Chunks[0].ID || len(current.Warnings) == 0 {
		t.Fatalf("replacement fallback = %+v, %v", current, err)
	}
	if _, err := embedding.Rebuild(ctx, st.paths.EmbeddingsFile(), sources, inference.Embed); err != nil {
		t.Fatal(err)
	}
	removed, err := st.DocRemove(ctx, file, actor)
	if err != nil || removed.Status != "removed" || removed.Document.ID != added.Document.ID {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	sources, err = st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err = embedding.Status(ctx, st.paths.EmbeddingsFile(), sources)
	if err != nil || status.Orphaned != 1 || status.Current != 0 {
		t.Fatalf("removal index status = %+v, %v", status, err)
	}
	for _, mode := range []retrieval.Mode{retrieval.ModeFTS, retrieval.ModeHybrid} {
		cfg.Mode = mode
		gone, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), inference, cfg, retrieval.Options{Query: "newbeacononly"})
		if err != nil || gone.ReturnedCount != 0 {
			t.Fatalf("%s removal recall = %+v, %v", mode, gone, err)
		}
	}
	if _, err := embedding.Rebuild(ctx, st.paths.EmbeddingsFile(), sources, inference.Embed); err != nil {
		t.Fatal(err)
	}
	if got := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log"); got != commits+3 {
		t.Fatalf("changed operations made %d commits, want three", got-commits)
	}
	pending, err := st.PendingProposals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != proposal.ID {
		t.Fatalf("unrelated proposal changed: %+v, %v", pending, err)
	}
	if docs, err := st.DocList(ctx); err != nil || len(docs) != 0 {
		t.Fatalf("list after remove = %+v, %v", docs, err)
	}
	if missing, err := st.DocRemove(ctx, added.Document.ID, actor); err != nil || missing.Status != "not-found" {
		t.Fatalf("second remove = %+v, %v", missing, err)
	}
}

func TestDocumentsEmptyConcurrentIdentityAndConfigFinalization(t *testing.T) {
	ctx := context.Background()
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "")
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	var wait sync.WaitGroup
	results := make(chan DocResult, 4)
	for range 4 {
		wait.Go(func() {
			result, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
			if err != nil {
				t.Error(err)
			}
			results <- result
		})
	}
	wait.Wait()
	close(results)
	created, id := 0, ""
	for result := range results {
		if result.Document == nil {
			t.Fatalf("missing document result: %+v", result)
		}
		if id != "" && result.Document.ID != id {
			t.Fatal("concurrent duplicate ingestion created multiple identities")
		}
		id = result.Document.ID
		if result.Status == "created" {
			created++
			if !result.EnabledDefaultRecall {
				t.Fatal("first empty ingestion failed to enable recall")
			}
		}
		if result.Document.ChunkCount != 0 || result.Document.ByteLen != 0 || result.Document.Title != "spec.md" {
			t.Fatalf("empty document = %+v", result.Document)
		}
	}
	if created != 1 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before+1 {
		t.Fatal("concurrent ingestion was not one creation plus no-ops")
	}
	st2, file2 := documentFixture(t)
	writeDocumentFixture(t, file2, "# Durable despite config failure\n")
	result, err := st2.docAdd(ctx, DocAddOptions{File: file2, Actor: memory.UserActor}, func(*os.Root) (bool, error) {
		return false, errors.New("synthetic config finalization failure")
	})
	if err == nil || !strings.Contains(err.Error(), "do not replay ingestion") || result.Status != "created" || result.Commit == "" || result.EnabledDefaultRecall {
		t.Fatalf("post-commit result = %+v, %v", result, err)
	}
	shown, err := st2.DocShow(ctx, result.Document.ID)
	if err != nil || shown.Document.ContentHash != result.Document.ContentHash || len(shown.Chunks) != 1 {
		t.Fatalf("post-failure inspection = %+v, %v", shown, err)
	}
	noReplay, err := st2.DocAdd(ctx, DocAddOptions{File: file2, Actor: memory.UserActor})
	if err != nil || noReplay.Status != "unchanged" || noReplay.EnabledDefaultRecall {
		t.Fatalf("unchanged add repaired config implicitly: %+v, %v", noReplay, err)
	}
}

func TestDocumentWritesRefuseDirtyStateAndRollbackWholeReplacement(t *testing.T) {
	ctx := context.Background()
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "# Original\n\nkeep every original chunk\n")
	added, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE documents SET title = 'dirty title' WHERE id = ?", added.Document.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("CALL DOLT_ADD('documents')"); err != nil {
		t.Fatal(err)
	}
	writeDocumentFixture(t, file, "# Changed\n")
	for _, remove := range []bool{false, true} {
		var err error
		if remove {
			_, err = st.DocRemove(ctx, added.Document.ID, memory.UserActor)
		} else {
			_, err = st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
		}
		if err == nil || !strings.Contains(err.Error(), "uncommitted table change") {
			t.Fatalf("dirty write error = %v", err)
		}
	}
	shown, err := st.DocShow(ctx, added.Document.ID)
	if err != nil || shown.Document.Title != "Original" || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
		t.Fatalf("dirty data entered committed show: %+v, %v", shown, err)
	}
	if got := countInternal(t, st, "SELECT COUNT(*) FROM documents WHERE title = 'dirty title'"); got != 1 {
		t.Fatal("dirty write was lost")
	}
	if _, err := st.db.Exec("CALL DOLT_RESET('--hard')"); err != nil {
		t.Fatal(err)
	}
	// An actual constraint fails on a later replacement chunk, after deletion
	// of the old chunks and the metadata update, exercising transaction rollback.
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "install synthetic document failure", Statements: []store.Statement{
		{SQL: "ALTER TABLE doc_chunks ADD CONSTRAINT reject_fixture CHECK (LOCATE('rollbackrefusal', body) = 0)"},
	}}); err != nil {
		t.Fatal(err)
	}
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	writeDocumentFixture(t, file, "# Valid\n\nfirst insert succeeds\n\n## Invalid\n\nrollbackrefusal\n")
	if _, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor}); err == nil {
		t.Fatal("replacement unexpectedly passed the synthetic constraint")
	}
	after, err := st.DocShow(ctx, added.Document.ID)
	if err != nil || !reflect.DeepEqual(shown, after) || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatalf("failed replacement was not atomic: before=%+v after=%+v, %v", shown, after, err)
	}
	// A parent-delete refusal must roll back the preceding chunk deletion too.
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "install synthetic removal failure", Statements: []store.Statement{
		{SQL: "CREATE TABLE document_guard (doc_id CHAR(26) PRIMARY KEY, FOREIGN KEY (doc_id) REFERENCES documents(id))"},
		{SQL: "INSERT INTO document_guard (doc_id) VALUES (?)", Args: []any{added.Document.ID}},
	}}); err != nil {
		t.Fatal(err)
	}
	before = internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	if _, err := st.DocRemove(ctx, added.Document.ID, memory.UserActor); err == nil {
		t.Fatal("removal unexpectedly passed the synthetic foreign key")
	}
	after, err = st.DocShow(ctx, added.Document.ID)
	if err != nil || !reflect.DeepEqual(shown, after) || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatalf("failed removal was not atomic: before=%+v after=%+v, %v", shown, after, err)
	}
}

func TestDocumentValidationAndConfigurationRefuseBeforeDurability(t *testing.T) {
	st, file := documentFixture(t)
	ctx := context.Background()
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	for _, tc := range []struct {
		name, content, title, config, want string
	}{
		{"invalid UTF-8", string([]byte{0xff}), "", "", "UTF-8"},
		{"wide title", "body", strings.Repeat("界", 513), "", "512"},
		{"wide breadcrumb", "# " + strings.Repeat("a", 500) + "\n## " + strings.Repeat("b", 500) + "\n### " + strings.Repeat("c", 30), "valid", "", "1024"},
		{"wide paragraph", strings.Repeat("界", 22000), "", "", "65535-byte"},
		{"content deny", "# forbidden", "", "[deny_list]\npatterns=['forbidden']\n", "deny-list"},
		{"title deny", "body", "forbidden", "[deny_list]\npatterns=['forbidden']\n", "deny-list"},
		{"path deny", "body", "", "[deny_list]\npatterns=['spec\\.md']\n", "deny-list"},
		{"deny invalid", "body", "", "[deny_list]\npatterns=['[']\n", "pattern"},
		{"invalid TOML", "body", "", "[broken", "configuration"},
		{"wrong allow-list type", "body", "", "[doc]\nallowed_dirs=1\n", "configuration"},
		{"misspelled allow-list", "body", "", "[doc]\nallow_dirs=[]\n", "unknown [doc]"},
		{"wrong include type", "body", "", "[retrieval]\ninclude_docs_in_default='false'\n", "configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeDocumentFixture(t, file, tc.content)
			writeDocumentFixture(t, st.paths.ConfigFile(), tc.config)
			result, err := st.DocAdd(ctx, DocAddOptions{File: file, Title: tc.title, Actor: memory.UserActor, Confined: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) || result.Commit != "" {
				t.Fatalf("result=%+v error=%v, want %s", result, err, tc.want)
			}
			if internalCount(t, st, "SELECT COUNT(*) FROM documents") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatal("refusal left data or dirty state")
			}
		})
	}
	writeDocumentFixture(t, st.paths.ConfigFile(), "")
	for _, path := range []string{"", "\x00", "line\nbreak", strings.Repeat("x", 1025), st.paths.Base(), filepath.Join(st.paths.Base(), "absent.md")} {
		if _, err := st.DocAdd(ctx, DocAddOptions{File: path, Actor: memory.UserActor}); err == nil {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
	for _, ident := range []string{"'; DROP TABLE documents; --", "unknown", strings.Repeat("0", 26)} {
		got, err := st.DocShow(ctx, ident)
		if err != nil || got.Status != "not-found" {
			t.Fatalf("bound identity = %+v, %v", got, err)
		}
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before {
		t.Fatal("validation changed history")
	}
	if err := os.Remove(st.paths.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(st.paths.ConfigFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("unreadable config = %v", err)
	}
}

func TestDocumentMetadataScanAndCollationCollision(t *testing.T) {
	ctx := context.Background()
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "# Allowed\n")
	actor := memory.Actor{Name: "agent:codex", Raw: "raw-provenance"}
	for _, pattern := range []string{"^user$", "^raw-provenance$"} {
		writeDocumentFixture(t, st.paths.ConfigFile(), "[deny_list]\npatterns=['"+pattern+"']\n")
		if _, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: actor}); !errors.Is(err, store.ErrDenied) {
			t.Fatalf("metadata scan %s: %v", pattern, err)
		}
	}
	writeDocumentFixture(t, st.paths.ConfigFile(), "")
	// The shipped index is retained. Exercise its collision guard with a
	// collation that equates two distinct filesystem names, without silently
	// replacing the first document or changing its identity.
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "synthetic document collation", Statements: []store.Statement{
		{SQL: "ALTER TABLE documents MODIFY path VARCHAR(1024) COLLATE utf8mb4_0900_ai_ci"},
	}}); err != nil {
		t.Fatal(err)
	}
	added, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(st.paths.Base(), "spéc.md")
	writeDocumentFixture(t, other, "# Different file\n")
	if _, err := st.DocAdd(ctx, DocAddOptions{File: other, Actor: actor}); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("collation collision = %v", err)
	}
	shown, err := st.DocShow(ctx, added.Document.ID)
	if err != nil || !reflect.DeepEqual(shown.Document, added.Document) || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
		t.Fatalf("colliding source replaced a different file: %+v, %v", shown, err)
	}
}

func TestDocumentOwnerProtectionCheckFailsClosed(t *testing.T) {
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "\xff")
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	metadata, err := st.documentConfigRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	_, data, err := st.readDocumentFile(DocAddOptions{File: file, Actor: memory.UserActor}, documentConfig{}, metadata)
	if !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), "verify protected memdolt owner metadata") || len(data) != 0 {
		t.Fatalf("closed protection root did not refuse before content validation: %v", err)
	}
	if err := os.Mkdir(st.paths.PidFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := st.DocAdd(context.Background(), DocAddOptions{File: file, Actor: memory.UserActor})
	if err == nil || !strings.Contains(err.Error(), "cannot verify protected memdolt owner metadata") || result.Document != nil || len(result.Chunks) != 0 || result.Commit != "" {
		t.Fatal("invalid owner metadata did not refuse before content validation")
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM documents") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM doc_chunks") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatal("failed owner protection changed memory")
	}
	if _, err := os.Stat(st.paths.ConfigFile()); !os.IsNotExist(err) {
		t.Fatal("failed owner protection created configuration")
	}
}

func TestDocumentIdentityErrorsPreserveExactPathsAndIDs(t *testing.T) {
	ctx := context.Background()
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "# Stored identity survives source changes\n")
	added, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(st.paths.Base(), "cycle")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("platform cannot create fixture symlink: %v", err)
	}
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	if _, err := st.DocShow(ctx, loop); err == nil || !strings.Contains(err.Error(), "resolve document identity path") {
		t.Fatalf("show hid a path resolution error: %v", err)
	}
	if _, err := st.DocRemove(ctx, loop, memory.UserActor); err == nil || !strings.Contains(err.Error(), "resolve document identity path") {
		t.Fatalf("remove hid a path resolution error: %v", err)
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatal("path resolution errors changed memory")
	}
	if err := os.Remove(added.Document.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(added.Document.Path, added.Document.Path); err != nil {
		t.Fatal(err)
	}
	for _, ident := range []string{added.Document.ID, added.Document.Path} {
		shown, err := st.DocShow(ctx, ident)
		if err != nil || shown.Status != "found" || !reflect.DeepEqual(shown.Document, added.Document) || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
			t.Fatalf("exact identity depended on source resolution: %v", err)
		}
	}
	removed, err := st.DocRemove(ctx, added.Document.Path, memory.UserActor)
	if err != nil || removed.Status != "removed" || removed.Document.ID != added.Document.ID {
		t.Fatalf("exact stored path could not be removed: %v", err)
	}
}
