//go:build parity

package parity

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	sgtokenizer "github.com/sugarme/tokenizer"
	ort "github.com/yalue/onnxruntime_go"

	"github.com/kninetimmy/memdolt/tests/inference"
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

func TestMain(m *testing.M) {
	ortLibPath := os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH")
	if ortLibPath == "" {
		println("ONNXRUNTIME_SHARED_LIBRARY_PATH is not set; see the parity package doc comment (tests/parity/parity.go) for how to point it at an onnxruntime 1.26.0 shared library.")
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
	bgeTokenizer      *sgtokenizer.Tokenizer
	rerankerTokenizer *sgtokenizer.Tokenizer
	embed             *inference.EmbedRunner
	rerank            *inference.RerankRunner

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

	modelDir := inference.RequireEnv(t, "MEMDOLT_PARITY_MODEL_DIR",
		"Point it at a directory containing bge-small-en-v1.5/ and ms-marco-MiniLM-L-6-v2/ "+
			"subdirectories (memhub build.rs's OUT_DIR staging layout: model.onnx, tokenizer.json, "+
			"config.json, special_tokens_map.json, tokenizer_config.json in each). If not staged "+
			"locally, fetch from the pinned URLs in tests/parity/testdata/model_pins.json and verify "+
			"each SHA-256 before use.")

	pins, err := inference.LoadModelPins("testdata/model_pins.json")
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

	corpus := readJSON[corpusFile](t, "testdata/corpus.json")
	textByID := make(map[string]string, len(corpus.Texts))
	for _, c := range corpus.Texts {
		textByID[c.ID] = c.Text
	}

	return &harness{
		bgeTokenizer:      bgeTok,
		rerankerTokenizer: rrTok,
		embed:             embed,
		rerank:            rerank,
		corpus:            corpus,
		textByID:          textByID,
		tokensBGE:         readJSON[map[string][]int](t, "testdata/tokens_bge.json"),
		tokensRR:          readJSON[map[string][]int](t, "testdata/tokens_reranker.json"),
		embeddings:        readJSON[map[string][]float32](t, "testdata/embeddings.json"),
		rerankPairs:       readJSON[rerankPairsFile](t, "testdata/rerank_pairs.json"),
		rerankGold:        readJSON[[]rerankFixtureEntry](t, "testdata/rerank.json"),
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
		gotBGEEnc, err := inference.EncodeSingle(h.bgeTokenizer, c.Text)
		if err != nil {
			t.Fatalf("%s: BGE tokenize: %v", c.ID, err)
		}
		if !intsEqual(gotBGEEnc.IDs, wantBGE) {
			t.Errorf("%s: BGE token ids differ\n  got:  %v\n  want: %v", c.ID, gotBGEEnc.IDs, wantBGE)
		}

		wantRR, ok := h.tokensRR[c.ID]
		if !ok {
			t.Fatalf("no reranker token fixture for %s", c.ID)
		}
		gotRREnc, err := inference.EncodeSingle(h.rerankerTokenizer, c.Text)
		if err != nil {
			t.Fatalf("%s: reranker tokenize: %v", c.ID, err)
		}
		if !intsEqual(gotRREnc.IDs, wantRR) {
			t.Errorf("%s: reranker token ids differ\n  got:  %v\n  want: %v", c.ID, gotRREnc.IDs, wantRR)
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
		enc, err := inference.EncodeSingle(h.bgeTokenizer, c.Text)
		if err != nil {
			t.Fatalf("%s: tokenize: %v", c.ID, err)
		}
		got, err := h.embed.Embed(enc)
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
		enc, err := inference.EncodePair(h.rerankerTokenizer, p.Query, passage)
		if err != nil {
			t.Fatalf("%s: tokenize pair: %v", p.ID, err)
		}
		got, err := h.rerank.Score(enc)
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
