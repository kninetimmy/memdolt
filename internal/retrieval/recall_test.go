package retrieval

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type recallFixture struct {
	ctx  context.Context
	base string
	path string
	st   *localdolt.Store
}

type fakeInference struct {
	rerank func(string) float32
}

func (f fakeInference) Embed(string) ([]float32, error) {
	vector := make([]float32, embedding.EmbeddingDim)
	vector[0] = 1
	return vector, nil
}

func (f fakeInference) Rerank(_ string, passage string) (float32, error) {
	if f.rerank != nil {
		return f.rerank(passage), nil
	}
	return 3, nil
}

func newRecallFixture(t *testing.T, statements []store.Statement, text []string) *recallFixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	actor := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: statements, Text: text, Message: "seed recall fixture", Author: actor,
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatal(err)
	}
	return &recallFixture{ctx: ctx, base: base, path: paths.EmbeddingsFile(), st: st}
}

func TestFTSRetainsAndDemotesSupersededStaleFactsAndSupportsFilters(t *testing.T) {
	oldID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	newID := "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	now := time.Now().UTC().Truncate(time.Second)
	f := newRecallFixture(t, []store.Statement{
		{SQL: "INSERT INTO facts (id, `key`, value, source, verified_at, created_at, superseded_by) VALUES (?, ?, ?, ?, ?, ?, ?)", Args: []any{oldID, "build.command", "fallback probe alpha beta", "agent:codex", now.Add(-120 * 24 * time.Hour), now.Add(-120 * 24 * time.Hour), newID}},
		{SQL: "INSERT INTO facts (id, `key`, value, source, verified_at, created_at) VALUES (?, ?, ?, ?, ?, ?)", Args: []any{newID, "build.command", "fallback probe alpha beta", "user", now, now}},
	}, []string{"build.command", "fallback probe alpha beta"})

	cfg := DefaultConfig()
	response, err := Recall(f.ctx, f.st, f.path, nil, cfg, Options{
		Query: "fallback probe alpha beta", Mode: ModeFTS, MaxResults: 10,
		SourceTypes: []string{"fact"}, Provenance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.CandidateCount != 2 || response.ReturnedCount != 2 {
		t.Fatalf("FTS response counts = %+v, want two distinct facts", response)
	}
	var old, current *Hit
	for i := range response.Results {
		switch response.Results[i].SourceID {
		case oldID:
			old = &response.Results[i]
		case newID:
			current = &response.Results[i]
		}
	}
	if old == nil || current == nil {
		t.Fatalf("supersession chain was not retained: %+v", response.Results)
	}
	if !old.Stale || old.SupersededBy != newID || old.Score >= current.Score {
		t.Fatalf("old fact was not tagged and demoted: old=%+v current=%+v", old, current)
	}
	if old.LastChanged == nil || old.LastChanged.Hash == "" || old.LastChanged.Author != "user" {
		t.Fatalf("provenance missing from old fact: %+v", old.LastChanged)
	}
	if !warningPresent(response.Warnings, "stale_facts_demoted") {
		t.Fatalf("stale fact warning missing: %+v", response.Warnings)
	}

	accepted := true
	filtered, err := Recall(f.ctx, f.st, f.path, nil, cfg, Options{
		Query: "fallback probe", Mode: ModeFTS, MaxResults: 10,
		SourceTypes: []string{"fact"}, AcceptedOnly: &accepted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.ReturnedCount != 1 || filtered.Results[0].SourceID != newID {
		t.Fatalf("accepted-only result = %+v, want only the user fact", filtered.Results)
	}

	empty, err := Recall(f.ctx, f.st, f.path, nil, cfg, Options{Query: "zzzzunmatchedtoken", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if empty.ReturnedCount != 0 {
		t.Fatalf("unmatched recall returned %+v", empty.Results)
	}
	observability, err := embedding.ReadObservability(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	if observability.RecallCount != 3 || observability.EmptyCount != 1 || observability.EmptyRate != 1.0/3.0 {
		t.Fatalf("observability = %+v, want 1 empty of 3", observability)
	}
}

func TestHybridUsesVectorOnlyWhenCurrentAndLexicalFallbackForEveryStaleShape(t *testing.T) {
	ids := []string{
		"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		"01ARZ3NDEKTSV4RRFFQ69G5FAX", "01ARZ3NDEKTSV4RRFFQ69G5FAY",
	}
	statements := make([]store.Statement, 0, len(ids))
	for i, id := range ids {
		statements = append(statements, store.Statement{
			SQL:  "INSERT INTO facts (id, `key`, value, source, verified_at, created_at) VALUES (?, ?, ?, 'user', NOW(), NOW())",
			Args: []any{id, "fallback.key." + string(rune('a'+i)), "hybrid stale fallback probe"},
		})
	}
	f := newRecallFixture(t, statements, []string{"hybrid stale fallback probe"})
	inference := fakeInference{}
	sources, err := f.st.EmbeddingSources(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedding.Rebuild(f.ctx, f.path, sources, inference.Embed); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Mode = ModeHybrid
	cfg.UseReranker = false
	current, err := Recall(f.ctx, f.st, f.path, inference, cfg, Options{Query: "hybrid stale fallback probe", MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range current.Results {
		if hit.FTSScore != 0 || hit.VectorScore <= 0 {
			t.Fatalf("current hybrid candidate regained lexical weighting: %+v", hit)
		}
	}

	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM embeddings WHERE source_id = ?", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE embeddings SET content_hash = ? WHERE source_id = ?", strings.Repeat("f", 64), ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE embeddings SET vector = x'0001' WHERE source_id = ?", ids[3]); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	response, err := Recall(f.ctx, f.st, f.path, inference, cfg, Options{Query: "hybrid stale fallback probe", MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if response.ReturnedCount != 4 {
		t.Fatalf("stale lexical fallback lost candidates: %+v", response.Results)
	}
	for _, id := range ids[1:] {
		hit := findHit(t, response.Results, id)
		if hit.FTSScore <= 0 || hit.VectorScore != 0 {
			t.Errorf("stale %s was not a lexical-only fallback: %+v", id, hit)
		}
	}
	warning := findWarning(t, response.Warnings, "stale_embeddings")
	if warning.StaleCount != 3 || !strings.Contains(warning.Reason, "1 missing") ||
		!strings.Contains(warning.Reason, "1 content-hash-mismatched") ||
		!strings.Contains(warning.Reason, "1 wrong-byte-length") || warning.Fix != embedding.RebuildRemedy {
		t.Fatalf("stale warning = %+v", warning)
	}
}

func TestHybridRerankAppliesFloorAndMaximum(t *testing.T) {
	ids := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"}
	statements := []store.Statement{}
	for i, id := range ids {
		statements = append(statements, store.Statement{
			SQL:  "INSERT INTO facts (id, `key`, value, source, verified_at, created_at) VALUES (?, ?, ?, 'user', NOW(), NOW())",
			Args: []any{id, "rerank.key." + string(rune('a'+i)), []string{"drop low", "keep best", "keep second"}[i]},
		})
	}
	f := newRecallFixture(t, statements, []string{"rerank"})
	inference := fakeInference{rerank: func(passage string) float32 {
		switch {
		case strings.Contains(passage, "keep best"):
			return 5
		case strings.Contains(passage, "keep second"):
			return 3
		default:
			return 1
		}
	}}
	sources, _ := f.st.EmbeddingSources(f.ctx)
	if _, err := embedding.Rebuild(f.ctx, f.path, sources, inference.Embed); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Mode = ModeHybrid
	cfg.DefaultMaxResults = 1
	response, err := Recall(f.ctx, f.st, f.path, inference, cfg, Options{Query: "rerank query"})
	if err != nil {
		t.Fatal(err)
	}
	if response.CandidateCount != 3 || response.ReturnedCount != 1 || response.Results[0].Body != "keep best" ||
		response.Results[0].RerankScore == nil || *response.Results[0].RerankScore != 5 {
		t.Fatalf("rerank/floor/max response = %+v", response)
	}
}

func warningPresent(warnings []Warning, kind string) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

func findWarning(t *testing.T, warnings []Warning, kind string) Warning {
	t.Helper()
	for _, warning := range warnings {
		if warning.Kind == kind {
			return warning
		}
	}
	t.Fatalf("warning %q missing from %+v", kind, warnings)
	return Warning{}
}

func findHit(t *testing.T, hits []Hit, id string) Hit {
	t.Helper()
	for _, hit := range hits {
		if hit.SourceID == id {
			return hit
		}
	}
	t.Fatalf("hit %s missing from %+v", id, hits)
	return Hit{}
}
