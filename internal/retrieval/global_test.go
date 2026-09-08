package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/store"
)

type scopeInference struct {
	embeds, reranks int
}

func (f *scopeInference) Embed(string) ([]float32, error) {
	f.embeds++
	values := make([]float32, embedding.EmbeddingDim)
	values[0] = 1
	return values, nil
}

func (f *scopeInference) Rerank(string, string) (float32, error) {
	f.reranks++
	return 3, nil
}

func TestGlobalCombinedPoolFiltersDocsFreshnessAndOneInferencePass(t *testing.T) {
	ctx := context.Background()
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	oldID := "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	now := time.Now().UTC()
	old := now.Add(-400 * 24 * time.Hour)
	makeScope := func(value string) *recallFixture {
		return newRecallFixture(t, []store.Statement{
			{SQL: "INSERT INTO facts(id, `key`, value, source, verified_at) VALUES (?, 'same.key', ?, 'user', ?)", Args: []any{id, value, now}},
			{SQL: "INSERT INTO facts(id, `key`, value, source, verified_at, superseded_by) VALUES (?, 'same.old', ?, 'agent:codex', ?, ?)", Args: []any{oldID, value, old, id}},
			{SQL: "INSERT INTO doc_chunks(id, heading_path, body) VALUES (?, 'doc', ?)", Args: []any{id, value}},
			{SQL: "INSERT INTO tasks(id, title, status) VALUES (?, ?, 'open')", Args: []any{id, value}},
		}, []string{value})
	}
	repo, global := makeScope("scopebeacon repository"), makeScope("scopebeacon global")
	sources, err := repo.st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedding.Rebuild(ctx, repo.path, sources, (&scopeInference{}).Embed); err != nil {
		t.Fatal(err)
	}
	// Global vectors intentionally start missing: only that scope gets fallback.
	cfg := DefaultConfig()
	cfg.Mode, cfg.IncludeDocsInDefault = ModeHybrid, false
	options := Options{Query: "scopebeacon", MaxResults: 20, SkipObservability: true}
	run := func(options Options, docs bool) (Response, *scopeInference) {
		t.Helper()
		inference := &scopeInference{}
		result, err := recallCombined(ctx, repo.st, repo.path, inference, cfg, options, GlobalScope{Store: global.st, EmbeddingsPath: global.path, IncludeDocsInDefault: docs})
		if err != nil {
			t.Fatal(err)
		}
		return result, inference
	}
	result, inference := run(options, true)
	if result.CandidateCount != 6 || result.ReturnedCount != 6 || inference.embeds != 1 || inference.reranks != 6 {
		t.Fatalf("not one merged inference pass: %+v, %+v", result, inference)
	}
	scoped := map[string]int{}
	for _, hit := range result.Results {
		scoped[hit.Scope+"/"+hit.SourceType]++
		if hit.Scope == "repo" && hit.SourceType == "doc_chunk" || hit.Scope == "global" && hit.SourceType == "task" {
			t.Fatal("scope-specific corpus policy leaked")
		}
		if hit.SourceID != id && hit.SourceID != oldID || hit.SnapshotCommit == "" {
			t.Fatal("scope prefixes escaped or provenance missing")
		}
	}
	if scoped["repo/fact"] != 2 || scoped["global/fact"] != 2 || scoped["global/doc_chunk"] != 1 {
		t.Fatal(scoped)
	}
	for _, warning := range result.Warnings {
		if warning.Kind == "stale_embeddings" && (warning.Scope != "global" || warning.Fix != "run `memdolt index rebuild --global --dir <repository>`") {
			t.Fatal(warning)
		}
	}
	if !warningPresent(result.Warnings, "stale_embeddings") || !warningPresent(result.Warnings, "stale_facts_demoted") {
		t.Fatal(result.Warnings)
	}
	accepted, stale := true, false
	options.AcceptedOnly, options.IncludeStale = &accepted, &stale
	filtered, _ := run(options, false)
	if filtered.ReturnedCount != 2 {
		t.Fatalf("accepted/stale policy not applied per scope: %+v", filtered)
	}
	options.SourceTypes = []string{"doc_chunk"}
	explicit, _ := run(options, false)
	// NULL source docs are filtered by accepted-only in both scopes.
	if explicit.ReturnedCount != 0 {
		t.Fatal(explicit)
	}
	options.AcceptedOnly = nil
	explicit, _ = run(options, false)
	if explicit.ReturnedCount != 2 || explicit.AvailableDocs != 0 {
		t.Fatalf("explicit docs did not include both scopes: %+v", explicit)
	}
	options.SourceTypes = nil
	noRerank := false
	options.UseReranker = &noRerank
	without, inference := run(options, true)
	if inference.reranks != 0 || without.ReturnedCount != 3 {
		t.Fatalf("default docs bypassed required rerank: %+v %+v", without, inference)
	}
}
