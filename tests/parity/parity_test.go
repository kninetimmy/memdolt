//go:build parity

package parity

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
)

// Tolerances (recorded with their reasoning in docs/spikes/m0-rig2.md).
//
// Token ids must be byte-identical: tokenization is deterministic
// vocabulary lookup with no floating-point arithmetic anywhere in it, so any
// difference at all is a tokenizer bug, not numeric noise.
//
// Embeddings and rerank scores go through two independent onnxruntime
// bindings (Rust's `ort` crate vs. Go's yalue/onnxruntime_go) around the
// same model bytes and the same CPU execution provider; both are floating
// point, so exact equality isn't the bar — small deviations from
// non-associative float summation order across two different runtime
// builds are expected. See docs/spikes/m0-rig2.md for the measured maxima
// this run actually produced and how these thresholds were chosen against
// them.
const (
	embeddingMaxAbsDeviation = 1e-3
	rerankMaxAbsDeviation    = 1e-2
)

type corpusText struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Text     string `json:"text"`
}

type corpusFile struct {
	Texts []corpusText `json:"texts"`
}

type rerankPair struct {
	ID        string `json:"id"`
	Query     string `json:"query"`
	PassageID string `json:"passage_id"`
}

type rerankPairsFile struct {
	Pairs []rerankPair `json:"pairs"`
}

type rerankFixtureEntry struct {
	ID        string  `json:"id"`
	Query     string  `json:"query"`
	PassageID string  `json:"passage_id"`
	Score     float32 `json:"score"`
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return v
}

// harness bundles everything a single test run needs: both tokenizers, both
// onnx sessions, and the fixture data to diff against.
type harness struct {
	engine *embedding.Engine

	corpus      corpusFile
	textByID    map[string]string
	tokensBGE   map[string][]int
	tokensRR    map[string][]int
	embeddings  map[string][]float32
	rerankPairs rerankPairsFile
	rerankGold  []rerankFixtureEntry
}

func setupHarness(t *testing.T) *harness {
	t.Helper()
	engine, err := embedding.Open(context.Background(), embedding.Options{
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

	corpus := readJSON[corpusFile](t, "testdata/corpus.json")
	textByID := make(map[string]string, len(corpus.Texts))
	for _, c := range corpus.Texts {
		textByID[c.ID] = c.Text
	}

	return &harness{
		engine:      engine,
		corpus:      corpus,
		textByID:    textByID,
		tokensBGE:   readJSON[map[string][]int](t, "testdata/tokens_bge.json"),
		tokensRR:    readJSON[map[string][]int](t, "testdata/tokens_reranker.json"),
		embeddings:  readJSON[map[string][]float32](t, "testdata/embeddings.json"),
		rerankPairs: readJSON[rerankPairsFile](t, "testdata/rerank_pairs.json"),
		rerankGold:  readJSON[[]rerankFixtureEntry](t, "testdata/rerank.json"),
	}
}

// TestTokenIDsByteIdentical asserts every probe text tokenizes to the exact
// same ids, on both tokenizers, as memhub's fastembed-driven reference.
func TestTokenIDsByteIdentical(t *testing.T) {
	h := setupHarness(t)

	for _, c := range h.corpus.Texts {
		wantBGE, ok := h.tokensBGE[c.ID]
		if !ok {
			t.Fatalf("no BGE token fixture for %s", c.ID)
		}
		gotBGE, err := h.engine.EmbeddingTokenIDs(c.Text)
		if err != nil {
			t.Fatalf("%s: BGE tokenize: %v", c.ID, err)
		}
		if !intsEqual(gotBGE, wantBGE) {
			t.Errorf("%s: BGE token ids differ\n  got:  %v\n  want: %v", c.ID, gotBGE, wantBGE)
		}

		wantRR, ok := h.tokensRR[c.ID]
		if !ok {
			t.Fatalf("no reranker token fixture for %s", c.ID)
		}
		gotRR, err := h.engine.RerankerTokenIDs(c.Text)
		if err != nil {
			t.Fatalf("%s: reranker tokenize: %v", c.ID, err)
		}
		if !intsEqual(gotRR, wantRR) {
			t.Errorf("%s: reranker token ids differ\n  got:  %v\n  want: %v", c.ID, gotRR, wantRR)
		}
	}
}

// TestEmbeddingParity asserts every probe text's 384-dim embedding matches
// the fastembed reference within embeddingMaxAbsDeviation, and reports the
// measured maximum deviation across the whole corpus.
func TestEmbeddingParity(t *testing.T) {
	h := setupHarness(t)

	maxDev := -1.0
	var maxDevID string
	for _, c := range h.corpus.Texts {
		want, ok := h.embeddings[c.ID]
		if !ok {
			t.Fatalf("no embedding fixture for %s", c.ID)
		}
		got, err := h.engine.Embed(c.Text)
		if err != nil {
			t.Fatalf("%s: embed: %v", c.ID, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: dimension mismatch: got %d, want %d", c.ID, len(got), len(want))
		}
		dev := maxAbsDiff(got, want)
		t.Logf("%s: |deviation| %g", c.ID, dev)
		if dev > maxDev {
			maxDev = dev
			maxDevID = c.ID
		}
		if dev > embeddingMaxAbsDeviation {
			t.Errorf("%s: embedding deviates by %g (tolerance %g)", c.ID, dev, embeddingMaxAbsDeviation)
		}
	}
	t.Logf("embedding parity: measured max |deviation| = %g on %q across %d probe texts (tolerance %g)",
		maxDev, maxDevID, len(h.corpus.Texts), embeddingMaxAbsDeviation)
}

// TestRerankParity asserts every probe (query, passage) pair's cross-encoder
// score matches the fastembed reference within rerankMaxAbsDeviation, and
// reports the measured maximum deviation across all pairs.
func TestRerankParity(t *testing.T) {
	h := setupHarness(t)

	goldByID := make(map[string]rerankFixtureEntry, len(h.rerankGold))
	for _, g := range h.rerankGold {
		goldByID[g.ID] = g
	}

	maxDev := -1.0
	var maxDevID string
	for _, p := range h.rerankPairs.Pairs {
		gold, ok := goldByID[p.ID]
		if !ok {
			t.Fatalf("no rerank fixture for %s", p.ID)
		}
		passage, ok := h.textByID[p.PassageID]
		if !ok {
			t.Fatalf("%s: unknown passage_id %s", p.ID, p.PassageID)
		}
		got, err := h.engine.Rerank(p.Query, passage)
		if err != nil {
			t.Fatalf("%s: score: %v", p.ID, err)
		}
		dev := math.Abs(float64(got) - float64(gold.Score))
		t.Logf("%s: got %g want %g |deviation| %g", p.ID, got, gold.Score, dev)
		if dev > maxDev {
			maxDev = dev
			maxDevID = p.ID
		}
		if dev > rerankMaxAbsDeviation {
			t.Errorf("%s: rerank score deviates by %g (tolerance %g): got %g, want %g",
				p.ID, dev, rerankMaxAbsDeviation, got, gold.Score)
		}
	}
	t.Logf("rerank parity: measured max |deviation| = %g on %q across %d pairs (tolerance %g)",
		maxDev, maxDevID, len(h.rerankPairs.Pairs), rerankMaxAbsDeviation)
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func maxAbsDiff(a, b []float32) float64 {
	var max float64
	for i := range a {
		d := math.Abs(float64(a[i]) - float64(b[i]))
		if d > max {
			max = d
		}
	}
	return max
}
