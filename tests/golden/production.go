//go:build golden

package golden

import (
	"context"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// productionHarness is the unchanged hermetic corpus stored through the real
// migration schema, production side-store, and production retrieval package.
// The older in-memory pipeline remains beside it only for the historical
// FULLTEXT/BM25/scale comparisons.
type productionHarness struct {
	ctx    context.Context
	store  *localdolt.Store
	path   string
	engine *embedding.Engine
	config retrieval.Config
}

func setupProductionHarness(t *testing.T, engine *embedding.Engine) *productionHarness {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	actor := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: actor})
	if err != nil {
		t.Fatalf("new production golden store: %v", err)
	}
	if err := st.Open(ctx); err != nil {
		t.Fatalf("open production golden store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate production golden store: %v", err)
	}

	statements := []store.Statement{}
	texts := []string{}
	documentID := "doc-code-style-guide"
	documentAdded := false
	for _, row := range seedRows() {
		switch row.sourceType {
		case "fact":
			statements = append(statements, store.Statement{
				SQL:  "INSERT INTO facts (id, `key`, value, source, verified_at, created_at) VALUES (?, ?, ?, 'user', NOW(), NOW())",
				Args: []any{row.id, row.title, row.body},
			})
		case "decision":
			statements = append(statements, store.Statement{
				SQL:  "INSERT INTO decisions (id, title, rationale, summary, status, source, decided_at) VALUES (?, ?, ?, NULLIF(?, ''), 'active', 'user+agent:claude-code', NOW())",
				Args: []any{row.id, row.title, row.body, row.summary},
			})
		case "task":
			statements = append(statements, store.Statement{
				SQL:  "INSERT INTO tasks (id, title, status, notes, created_at, updated_at) VALUES (?, ?, 'open', ?, NOW(), NOW())",
				Args: []any{row.id, row.title, row.body},
			})
		case "doc_chunk":
			if !documentAdded {
				documentAdded = true
				statements = append(statements, store.Statement{
					SQL:  "INSERT INTO documents (id, path, title, content_hash, byte_len, source, ingested_at) VALUES (?, 'code-style-guide.md', 'Rust Code Style Guide', ?, 0, 'user', NOW())",
					Args: []any{documentID, "0000000000000000000000000000000000000000000000000000000000000000"},
				})
			}
			statements = append(statements, store.Statement{
				SQL:  "INSERT INTO doc_chunks (id, doc_id, ord, heading_path, body) VALUES (?, ?, ?, ?, ?)",
				Args: []any{row.id, documentID, row.ord, row.headingPath, row.body},
			})
		default:
			t.Fatalf("unknown production golden source type %q", row.sourceType)
		}
		texts = append(texts, row.title, row.body, row.summary, row.headingPath)
	}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: statements, Text: texts, Message: "seed production retrieval golden fixture", Author: actor,
	}); err != nil {
		t.Fatalf("seed production golden store: %v", err)
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("production golden layout: %v", err)
	}
	sources, err := st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatalf("read production golden embedding sources: %v", err)
	}
	if _, err := embedding.Rebuild(ctx, paths.EmbeddingsFile(), sources, engine.Embed); err != nil {
		t.Fatalf("build production golden embedding index: %v", err)
	}
	cfg := retrieval.DefaultConfig()
	cfg.Mode = retrieval.ModeHybrid
	cfg.IncludeDocsInDefault = true
	return &productionHarness{ctx: ctx, store: st, path: paths.EmbeddingsFile(), engine: engine, config: cfg}
}

func runProductionGoldenEval(t *testing.T, h *productionHarness, golden *goldenFile) evalSummary {
	t.Helper()
	outcomes := make([]queryOutcome, 0, len(golden.Queries))
	for _, query := range golden.Queries {
		response, err := retrieval.Recall(h.ctx, h.store, h.path, h.engine, h.config, retrieval.Options{Query: query.Query})
		if err != nil {
			t.Fatalf("production query %s: %v", query.ID, err)
		}
		hits := make([]resultHit, len(response.Results))
		for i, hit := range response.Results {
			hits[i] = resultHit{
				rank: hit.Rank, sourceType: hit.SourceType, scope: "repo", id: hit.SourceID,
				title: hit.Title, body: hit.Body, score: hit.Score,
			}
		}
		outcomes = append(outcomes, evaluateQuery(query, hits, 3))
	}
	return summarize("PRODUCTION vector-only hybrid", outcomes)
}
