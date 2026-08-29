//go:build golden

package golden

import (
	"context"
	"os"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
)

// setupPipelineHarness opens the production inference engine (and therefore
// provisions models/manifest.json's SHA-256-pinned artifacts), seeds a
// disposable embedded Dolt store with the hermetic fixture corpus plus any
// extraRows appended after it (the
// scale rig passes its synthetic hard negatives — see scale_test.go), and
// builds an embedding for every seeded row through that production path.
// Returns a harness whose lexical field is left unset; callers plug
// in fulltextGatherer or bm25Gatherer.
func setupPipelineHarness(t *testing.T, extraRows ...corpusRow) *pipelineHarness {
	t.Helper()
	ctx := context.Background()

	engine, err := embedding.Open(ctx, embedding.Options{
		CacheDir:    os.Getenv("MEMDOLT_PARITY_MODEL_DIR"),
		RuntimePath: os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH"),
		Offline:     os.Getenv("MEMDOLT_INFERENCE_OFFLINE") != "",
	})
	if err != nil {
		t.Fatalf("opening production inference engine: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("closing production inference engine: %v", err)
		}
	})

	db, err := openDisposableStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("opening disposable store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := createSchema(ctx, db); err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	seeded := append(seedRows(), extraRows...)
	if err := insertRows(ctx, db, seeded); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}

	rows := make(map[rowKey]corpusRow, len(seeded))
	embeddings := make(map[rowKey][]float32, len(seeded))
	for _, r := range seeded {
		key := rowKey{r.sourceType, r.id}
		rows[key] = r
		vec, err := engine.Embed(r.embedText())
		if err != nil {
			t.Fatalf("embedding %s: %v", key, err)
		}
		embeddings[key] = vec
	}

	return &pipelineHarness{
		db:         db,
		rows:       rows,
		embeddings: embeddings,
		engine:     engine,
	}
}

// runGoldenEval runs every golden query through h's pipeline (h.lexical
// selects FULLTEXT or the BM25 contingency) and returns the summary.
func runGoldenEval(t *testing.T, h *pipelineHarness, golden *goldenFile) evalSummary {
	t.Helper()
	ctx := context.Background()
	outcomes := make([]queryOutcome, 0, len(golden.Queries))
	for _, q := range golden.Queries {
		hits, err := runQuery(ctx, h, q.Query)
		if err != nil {
			t.Fatalf("query %s (%s): %v", q.ID, h.lexical.Name(), err)
		}
		outcomes = append(outcomes, evaluateQuery(q, hits, 3))
	}
	return summarize(h.lexical.Name(), outcomes)
}

func logSummary(t *testing.T, s evalSummary) {
	t.Helper()
	t.Logf("[%s] queries=%d match=%d/%d empty=%d/%d recall@3=%.4f safety_failures=%d",
		s.gathererName, s.matchQueries+s.emptyQueries, s.matchPasses, s.matchQueries,
		s.emptyPasses, s.emptyQueries, s.recallAtK, s.safetyFailures)
	for _, o := range s.outcomes {
		if !o.passed {
			t.Logf("[%s] FAIL %s (%s): %s", s.gathererName, o.id, o.kind, o.failureReason)
		}
	}
}

// TestRetrievalGolden is PRD §16's rig-3 gate: memdolt's retrieval pipeline
// must reproduce memhub's recorded baseline (Recall@3 = 100%, 21/21, 0
// safety failures) on the same 22-query golden set and hermetic fixture
// shape. MD5's M0 before-state (§19) chose Dolt FULLTEXT, so this test keeps
// that original pass/fail assertion: a run where FULLTEXT stops carrying the
// gate must fail loudly, not pass silently on the BM25 contingency. BM25 and
// the vector-only configuration (no lexical gather at all) are still measured
// and logged on every run, both
// because §8.1 R2's contingency needs to stay proven working and because
// the FULLTEXT-vs-BM25-vs-vector-only comparison is itself a recorded
// finding — see docs/spikes/m0-rig3.md §1/§4/§7 for what it showed.
func TestRetrievalGolden(t *testing.T) {
	golden, err := loadGoldenFile("retrieval_golden.json")
	if err != nil {
		t.Fatalf("%v", err)
	}
	// Drift guard, mirroring memhub's own hermetic test: this golden file
	// is 22 queries (21 match + 1 empty) as of the ported v0.2.0 set.
	if len(golden.Queries) != 22 {
		t.Fatalf("golden query count drifted: got %d, want 22", len(golden.Queries))
	}

	h := setupPipelineHarness(t)

	h.lexical = newFulltextGatherer(h.db)
	ftSummary := runGoldenEval(t, h, golden)
	logSummary(t, ftSummary)

	h.lexical = newBM25Gatherer(h.rows)
	bmSummary := runGoldenEval(t, h, golden)
	logSummary(t, bmSummary)

	h.lexical = noLexicalGatherer{}
	vecSummary := runGoldenEval(t, h, golden)
	logSummary(t, vecSummary)
	if !meetsGate(bmSummary) {
		t.Logf("note: BM25 contingency did not clear the gate on this run (recall@3=%.4f, safety_failures=%d)",
			bmSummary.recallAtK, bmSummary.safetyFailures)
	}
	if meetsGate(vecSummary) {
		t.Logf("note: the vector-only control also cleared the gate — the pool cap (rerankPoolSize=%d) "+
			"evicts one of %d seeded rows on every query regardless of the lexical step, but never a top-3 "+
			"target on this golden set (recall@3=%.4f); see docs/spikes/m0-rig3.md §1/§8", rerankPoolSize, len(h.rows), vecSummary.recallAtK)
	}
	if vecSummary.matchPasses != 21 || vecSummary.safetyFailures != 0 {
		t.Errorf("selected M2 vector-only pool=%d strategy: recall@3=%.4f (%d/%d), safety_failures=%d",
			rerankPoolSize, vecSummary.recallAtK, vecSummary.matchPasses, vecSummary.matchQueries, vecSummary.safetyFailures)
	}

	// Preserve the original FULLTEXT gate as well as the selected M2 check
	// above: the scale decision is additive to the M0 evidence, not a reason
	// to weaken its regression coverage.
	if ftSummary.matchQueries != 21 || ftSummary.emptyQueries != 1 {
		t.Fatalf("golden query mix drifted: match=%d empty=%d, want 21/1",
			ftSummary.matchQueries, ftSummary.emptyQueries)
	}
	var misses []string
	for _, o := range ftSummary.outcomes {
		if !o.passed {
			misses = append(misses, o.id+": "+o.failureReason)
		}
	}
	if len(misses) > 0 {
		t.Errorf("FULLTEXT: recall@3=%.4f, misses:\n%s", ftSummary.recallAtK, joinLines(misses))
	}
	if ftSummary.safetyFailures != 0 {
		t.Errorf("FULLTEXT: %d safety failure(s) (gibberish probe leaked a hit)", ftSummary.safetyFailures)
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
