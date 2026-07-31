//go:build golden

package golden

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/kninetimmy/memdolt/tests/inference"
)

func TestMain(m *testing.M) {
	ortLibPath := os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH")
	if ortLibPath == "" {
		println("ONNXRUNTIME_SHARED_LIBRARY_PATH is not set; see the golden package doc comment (tests/golden/pipeline.go) for how to point it at an onnxruntime 1.26.0 shared library.")
		os.Exit(1)
	}
	ort.SetSharedLibraryPath(ortLibPath)
	if err := ort.InitializeEnvironment(); err != nil {
		println("ort.InitializeEnvironment failed: " + err.Error())
		os.Exit(1)
	}
	code := m.Run()
	_ = ort.DestroyEnvironment()
	os.Exit(code)
}

// setupPipelineHarness stages the ONNX models (reusing tests/parity's
// SHA-256-pinned model files — model_pins.json is not duplicated, see
// pipeline.go's doc comment), seeds a disposable embedded Dolt store with
// the hermetic fixture corpus, and builds an embedding for every seeded row
// through the rig-2 inference path. Returns a harness whose lexical field
// is left unset; callers plug in fulltextGatherer or bm25Gatherer.
func setupPipelineHarness(t *testing.T) *pipelineHarness {
	t.Helper()
	ctx := context.Background()

	modelDir := inference.RequireEnv(t, "MEMDOLT_PARITY_MODEL_DIR",
		"Point it at a directory containing bge-small-en-v1.5/ and ms-marco-MiniLM-L-6-v2/ "+
			"subdirectories (memhub build.rs's OUT_DIR staging layout). If not staged locally, "+
			"fetch from the pinned URLs in tests/parity/testdata/model_pins.json and verify "+
			"each SHA-256 before use.")

	pins, err := inference.LoadModelPins(filepath.Join("..", "parity", "testdata", "model_pins.json"))
	if err != nil {
		t.Fatalf("%v", err)
	}
	bundles := make(map[string]map[string][]byte, len(pins.Bundles))
	for _, b := range pins.Bundles {
		files, err := inference.StagedModelFiles(modelDir, b)
		if err != nil {
			t.Fatalf("%v", err)
		}
		bundles[b.Name] = files
	}
	bgeFiles := bundles["bge-small-en-v1.5"]
	rrFiles := bundles["ms-marco-MiniLM-L-6-v2"]

	bgeTok, err := inference.BuildBertWordPieceTokenizer(bgeFiles["tokenizer.json"])
	if err != nil {
		t.Fatalf("building BGE tokenizer: %v", err)
	}
	rrTok, err := inference.BuildBertWordPieceTokenizer(rrFiles["tokenizer.json"])
	if err != nil {
		t.Fatalf("building reranker tokenizer: %v", err)
	}
	embed, err := inference.NewEmbedRunner(bgeFiles["model.onnx"])
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() { _ = embed.Destroy() })
	rerank, err := inference.NewRerankRunner(rrFiles["model.onnx"])
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() { _ = rerank.Destroy() })

	db, err := openDisposableStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("opening disposable store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := createSchema(ctx, db); err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	seeded := seedRows()
	if err := insertRows(ctx, db, seeded); err != nil {
		t.Fatalf("seeding rows: %v", err)
	}

	rows := make(map[rowKey]corpusRow, len(seeded))
	embeddings := make(map[rowKey][]float32, len(seeded))
	for _, r := range seeded {
		key := rowKey{r.sourceType, r.id}
		rows[key] = r
		enc, err := inference.EncodeSingle(bgeTok, r.embedText())
		if err != nil {
			t.Fatalf("encoding %s: %v", key, err)
		}
		vec, err := embed.Embed(enc)
		if err != nil {
			t.Fatalf("embedding %s: %v", key, err)
		}
		embeddings[key] = vec
	}

	return &pipelineHarness{
		db:         db,
		rows:       rows,
		embeddings: embeddings,
		bgeTok:     bgeTok,
		rrTok:      rrTok,
		embed:      embed,
		rerank:     rerank,
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
// shape. FULLTEXT (PRD §8.1 step 1's primary path) is measured first; the
// in-process BM25 contingency (§8.1 R2) is always measured too so both
// numbers are on record regardless of which one carries the gate — see
// docs/spikes/m0-rig3.md for the recorded verdict and MD5 recommendation.
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

	// PRD §16's gate: Recall@3 >= memhub's baseline, zero safety failures.
	// FULLTEXT is the primary §8.1 path; the BM25 contingency (§8.1 R2)
	// only needs to carry the gate if FULLTEXT falls short. Both numbers
	// are always measured and logged above regardless of which one wins.
	winning, label := ftSummary, "FULLTEXT (primary)"
	if !meetsGate(ftSummary) && meetsGate(bmSummary) {
		winning, label = bmSummary, "BM25 (§8.1 R2 contingency)"
	}
	t.Logf("gate config: %s", label)

	if winning.matchQueries != 21 || winning.emptyQueries != 1 {
		t.Fatalf("golden query mix drifted: match=%d empty=%d, want 21/1",
			winning.matchQueries, winning.emptyQueries)
	}
	var misses []string
	for _, o := range winning.outcomes {
		if !o.passed {
			misses = append(misses, o.id+": "+o.failureReason)
		}
	}
	if len(misses) > 0 {
		t.Errorf("%s: recall@3=%.4f, misses:\n%s", label, winning.recallAtK, joinLines(misses))
	}
	if winning.safetyFailures != 0 {
		t.Errorf("%s: %d safety failure(s) (gibberish probe leaked a hit)", label, winning.safetyFailures)
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
