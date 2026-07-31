//go:build golden

// Package golden is PRD §16's rig 3: the retrieval golden gate. It seeds a
// disposable embedded Dolt store with PRD §6.1-shaped tables, builds
// embeddings for every seeded row through the rig-2 inference path
// (tests/inference, shared with tests/parity), and runs PRD §8.1's
// retrieval pipeline against memhub's shipped golden query set
// (retrieval_golden.json, ported verbatim from memhub v0.2.0) to measure
// Recall@3 and safety failures against memhub's recorded baseline.
//
// It is behind the `golden` build tag for the same reason tests/parity is
// behind `parity` (docs/spikes/m0-rig2.md): it needs the same staged model
// files and onnxruntime shared library, plus a disposable Dolt data
// directory, none of which belong in the default `go test ./...` run.
//
//	go test -tags golden,gms_pure_go ./tests/golden/... -timeout 30m
//
// See docs/spikes/m0-rig3.md for the measured verdict.
package golden

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"

	sgtokenizer "github.com/sugarme/tokenizer"

	"github.com/kninetimmy/memdolt/tests/inference"
)

// §8.1 / config defaults (PRD §8.1, memhub's src/config/mod.rs — the exact
// numbers this rig's acceptance criteria name).
const (
	perSourceFTSLimit = 50
	ftsWeight         = 0.5
	vectorWeight      = 0.5
	stalePenalty      = 0.3 // unused: the fixture seeds no stale rows
	supersededPenalty = 0.4 // unused: the fixture seeds no superseded rows
	rerankPoolSize    = 20
	minRerankScore    = float32(2.0)
	docMinRerankScore = float32(0.0)
	maxResults        = 6
)

// allSourceTypes is the default recall source-type set this rig always
// requests — mirrors memhub's default (fact, decision, task) plus doc_chunk
// via `include_docs_in_default` (decision 90), which the hermetic fixture's
// one `doc::add` call auto-enables. This rig never exercises an explicit
// `--source-type` scope narrower than the default bundle, so it is not
// modeled at all.
var allSourceTypes = []string{"fact", "decision", "task", "doc_chunk"}

// rowKey identifies one seeded row.
type rowKey struct {
	sourceType string
	id         string
}

func (k rowKey) String() string { return k.sourceType + "/" + k.id }

// corpusRow is one seeded fact/decision/task/doc_chunk row, held in memory
// so the pipeline can hydrate candidates without a second round-trip to
// Dolt (the fields below are exactly what recall.rs's `hydrate_sources`
// fetches back out of the source tables in memhub).
type corpusRow struct {
	sourceType string
	id         string
	scope      string // "repo" or "global"
	title      string // fact key / decision title / task title / doc chunk title
	body       string // fact value / decision rationale / task notes / doc chunk body
	summary    string // decisions only; "" otherwise
}

// embedText mirrors memhub's retrieval/persist.rs embed-text builders
// exactly (fact_embed_text, decision_embed_text, task_embed_text,
// doc_chunk_embed_text).
func (r corpusRow) embedText() string {
	switch r.sourceType {
	case "fact":
		return fmt.Sprintf("%s: %s", r.title, r.body)
	case "decision":
		if r.summary != "" {
			return fmt.Sprintf("%s\n\n%s\n\n%s", r.summary, r.title, r.body)
		}
		return fmt.Sprintf("%s\n\n%s", r.title, r.body)
	case "task":
		if r.body != "" {
			return fmt.Sprintf("%s\n\n%s", r.title, r.body)
		}
		return r.title
	case "doc_chunk":
		// r.title already carries "doc title — heading_path" (set at seed
		// time); the embed text mirrors doc_chunk_embed_text's
		// heading_path + body shape using the heading_path portion, which
		// for this rig's single-chunk fixture is the same breadcrumb.
		return fmt.Sprintf("%s\n\n%s", r.title, r.body)
	default:
		return r.title + "\n\n" + r.body
	}
}

// rerankText mirrors recall.rs's finalize(): the cross-encoder input is
// `summary\n\ntitle\n\nbody` when a summary is present, else `title\n\nbody`.
func (r corpusRow) rerankText() string {
	if r.summary != "" {
		return fmt.Sprintf("%s\n\n%s\n\n%s", r.summary, r.title, r.body)
	}
	return fmt.Sprintf("%s\n\n%s", r.title, r.body)
}

// lexicalHit is one per-source-type lexical match: a row id and its raw
// relevance, higher-is-better (Dolt FULLTEXT's natural-language relevancy
// and this rig's BM25 contingency both already return higher-is-better
// scores, unlike memhub's SQLite bm25() which is lower-is-better and gets
// negated before use — see recall.rs's `score()`).
type lexicalHit struct {
	id  string
	raw float64
}

// lexicalGatherer is the swappable §8.1 step 1. fulltextGatherer wraps the
// disposable Dolt store's `MATCH ... AGAINST (? IN NATURAL LANGUAGE MODE)`
// queries; bm25Gatherer (the §8.1 R2 contingency) scores the same in-memory
// corpus with an in-process BM25 implementation. Everything downstream
// (vector gather, fusion, rerank, floors, truncation) is identical for
// both, so a recall difference between them isolates the lexical step.
type lexicalGatherer interface {
	Gather(ctx context.Context, sourceType, query string, limit int) ([]lexicalHit, error)
	Name() string
}

// pipelineHarness bundles everything one query run needs: the seeded
// corpus, its embeddings, and the loaded ONNX models.
type pipelineHarness struct {
	db         *sql.DB
	rows       map[rowKey]corpusRow
	embeddings map[rowKey][]float32

	bgeTok  *sgtokenizer.Tokenizer
	rrTok   *sgtokenizer.Tokenizer
	embed   *inference.EmbedRunner
	rerank  *inference.RerankRunner
	lexical lexicalGatherer
}

// scoredHit is one fused-and-scored candidate, pre-rerank.
type scoredHit struct {
	row         corpusRow
	score       float64
	rerankScore float32
	hasRerank   bool
}

// resultHit is one ranked pipeline result, PRD §8.1 step 8's shape.
type resultHit struct {
	rank       int
	sourceType string
	scope      string
	id         string
	title      string
	body       string
	score      float64
}

// runQuery executes PRD §8.1's pipeline for one query against h's corpus
// and returns the truncated, ranked result set.
func runQuery(ctx context.Context, h *pipelineHarness, query string) ([]resultHit, error) {
	// Step 1: lexical gather, per source type, limited to
	// perSourceFTSLimit each.
	fts := make(map[rowKey]float64)
	for _, st := range allSourceTypes {
		hits, err := h.lexical.Gather(ctx, st, query, perSourceFTSLimit)
		if err != nil {
			return nil, fmt.Errorf("lexical gather (%s, %s): %w", h.lexical.Name(), st, err)
		}
		for _, hit := range hits {
			fts[rowKey{st, hit.id}] = hit.raw
		}
	}

	// Step 2: vector gather (hybrid mode) — brute-force cosine over every
	// seeded row of a requested source type, query embedding computed once.
	queryEnc, err := inference.EncodeSingle(h.bgeTok, query)
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}
	queryVec, err := h.embed.Embed(queryEnc)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	vector := make(map[rowKey]float64)
	requested := make(map[string]bool, len(allSourceTypes))
	for _, st := range allSourceTypes {
		requested[st] = true
	}
	for key, vec := range h.embeddings {
		if !requested[key.sourceType] {
			continue
		}
		cosine := cosineSimilarity(queryVec, vec)
		if cosine < 0.0 {
			continue
		}
		vector[key] = cosine
	}

	// Union the two candidate sets.
	candidates := make(map[rowKey]bool, len(fts)+len(vector))
	for k := range fts {
		candidates[k] = true
	}
	for k := range vector {
		candidates[k] = true
	}

	// Step 5: fusion (memhub's exact formula and defaults — half-life off,
	// so age_decay is always exactly 1.0; the fixture seeds no stale or
	// superseded rows, so both penalties are always exactly 0.0).
	ftsMin, ftsMax := minMax(fts)
	scored := make([]scoredHit, 0, len(candidates))
	for key := range candidates {
		row, ok := h.rows[key]
		if !ok {
			continue
		}
		ftsScore := 0.0
		if raw, ok := fts[key]; ok {
			ftsScore = normalizeFTS(raw, ftsMin, ftsMax)
		}
		vecScore := clamp01(vector[key])
		relevance := ftsWeight*ftsScore + vectorWeight*vecScore
		scored = append(scored, scoredHit{row: row, score: relevance})
	}

	// Step 6: scope merge is a no-op here — repo and global rows already
	// share one candidate pool (this rig's licensed simplification; see
	// docs/spikes/m0-rig3.md). Sort by blended score before the rerank
	// truncation, same as recall.rs's merged-pool sort.
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].row.sourceType != scored[j].row.sourceType {
			return scored[i].row.sourceType < scored[j].row.sourceType
		}
		return scored[i].row.id < scored[j].row.id
	})

	// Step 7: rerank over the top-20 pool, with floors, then truncate.
	reranked := len(scored) > 1
	if reranked {
		if len(scored) > rerankPoolSize {
			scored = scored[:rerankPoolSize]
		}
		type rerankedPair struct {
			idx   int
			score float32
		}
		pairs := make([]rerankedPair, len(scored))
		for i, sh := range scored {
			enc, err := inference.EncodePair(h.rrTok, query, sh.row.rerankText())
			if err != nil {
				return nil, fmt.Errorf("encode rerank pair: %w", err)
			}
			s, err := h.rerank.Score(enc)
			if err != nil {
				return nil, fmt.Errorf("rerank score: %w", err)
			}
			pairs[i] = rerankedPair{idx: i, score: s}
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })

		reshuffled := make([]scoredHit, 0, len(pairs))
		for _, p := range pairs {
			sh := scored[p.idx]
			floor := minRerankScore
			if sh.row.sourceType == "doc_chunk" {
				floor = docMinRerankScore
			}
			if p.score < floor {
				continue
			}
			sh.rerankScore = p.score
			sh.hasRerank = true
			reshuffled = append(reshuffled, sh)
		}
		scored = reshuffled
	} else {
		// Safety invariant (recall.rs's docs_via_default && !reranked
		// rule): an unscored doc chunk never displaces project memory.
		filtered := scored[:0]
		for _, sh := range scored {
			if sh.row.sourceType != "doc_chunk" {
				filtered = append(filtered, sh)
			}
		}
		scored = filtered
	}

	if len(scored) > maxResults {
		scored = scored[:maxResults]
	}

	results := make([]resultHit, len(scored))
	for i, sh := range scored {
		results[i] = resultHit{
			rank:       i + 1,
			sourceType: sh.row.sourceType,
			scope:      sh.row.scope,
			id:         sh.row.id,
			title:      sh.row.title,
			body:       sh.row.body,
			score:      sh.score,
		}
	}
	return results, nil
}

// sortLexicalHitsDesc sorts hits by raw relevance, descending, with a
// stable id tiebreak — shared by both lexicalGatherer implementations.
func sortLexicalHitsDesc(hits []lexicalHit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].raw != hits[j].raw {
			return hits[i].raw > hits[j].raw
		}
		return hits[i].id < hits[j].id
	})
}

func minMax(fts map[rowKey]float64) (float64, float64) {
	min, max := 0.0, 0.0
	first := true
	for _, v := range fts {
		if first {
			min, max = v, v
			first = false
			continue
		}
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

// normalizeFTS mirrors util.rs's normalize_fts exactly: min-max normalize
// into [0, 1], treating a degenerate range (single hit, or a tie across the
// whole pool) as full strength (1.0) rather than dividing by zero.
func normalizeFTS(value, min, max float64) float64 {
	if max-min < 1e-12 && max-min > -1e-12 {
		return 1.0
	}
	v := (value - min) / (max - min)
	return clamp01(v)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// cosineSimilarity mirrors util.rs's cosine_similarity exactly.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na <= 1e-12 || nb <= 1e-12 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
