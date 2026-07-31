//go:build golden

package golden

import (
	"context"
	"math"
	"regexp"
	"strings"
)

// bm25Gatherer is PRD §8.1 R2's contingency: an in-process Okapi BM25
// lexical gather over the seeded corpus, used in place of fulltextGatherer
// when the Dolt FULLTEXT path lands below memhub's Recall@3 baseline (see
// docs/spikes/m0-rig3.md). Standard parameters (k1=1.2, b=0.75); index is
// built once per source type at construction time, matching the FULLTEXT
// path's own per-table scoping.
type bm25Gatherer struct {
	perType map[string]*bm25Index
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

var bm25Tokenizer = regexp.MustCompile(`[a-z0-9]+`)

func bm25Tokenize(s string) []string {
	return bm25Tokenizer.FindAllString(strings.ToLower(s), -1)
}

// bm25Index is one source type's document set: term frequencies per
// document, document frequency per term, and corpus-level stats.
type bm25Index struct {
	docTermFreq map[string]map[string]int // id -> term -> count
	docLen      map[string]int            // id -> token count
	docFreq     map[string]int            // term -> number of docs containing it
	docCount    int
	avgDocLen   float64
}

func newBM25Gatherer(rows map[rowKey]corpusRow) *bm25Gatherer {
	byType := make(map[string][]corpusRow)
	for _, r := range rows {
		byType[r.sourceType] = append(byType[r.sourceType], r)
	}
	perType := make(map[string]*bm25Index, len(byType))
	for st, rs := range byType {
		idx := &bm25Index{
			docTermFreq: make(map[string]map[string]int),
			docLen:      make(map[string]int),
			docFreq:     make(map[string]int),
		}
		var totalLen int
		for _, r := range rs {
			terms := bm25Tokenize(r.title + " " + r.body)
			tf := make(map[string]int, len(terms))
			for _, term := range terms {
				tf[term]++
			}
			idx.docTermFreq[r.id] = tf
			idx.docLen[r.id] = len(terms)
			totalLen += len(terms)
			for term := range tf {
				idx.docFreq[term]++
			}
		}
		idx.docCount = len(rs)
		if idx.docCount > 0 {
			idx.avgDocLen = float64(totalLen) / float64(idx.docCount)
		}
		perType[st] = idx
	}
	return &bm25Gatherer{perType: perType}
}

func (g *bm25Gatherer) Name() string { return "BM25" }

func (g *bm25Gatherer) Gather(_ context.Context, sourceType, query string, limit int) ([]lexicalHit, error) {
	idx, ok := g.perType[sourceType]
	if !ok || idx.docCount == 0 {
		return nil, nil
	}
	terms := bm25Tokenize(query)
	scores := make(map[string]float64)
	for id, tf := range idx.docTermFreq {
		dl := float64(idx.docLen[id])
		var score float64
		for _, term := range terms {
			count, ok := tf[term]
			if !ok || count == 0 {
				continue
			}
			df := idx.docFreq[term]
			idf := math.Log(1 + (float64(idx.docCount)-float64(df)+0.5)/(float64(df)+0.5))
			num := float64(count) * (bm25K1 + 1)
			den := float64(count) + bm25K1*(1-bm25B+bm25B*dl/idx.avgDocLen)
			score += idf * num / den
		}
		if score > 0 {
			scores[id] = score
		}
	}
	hits := make([]lexicalHit, 0, len(scores))
	for id, s := range scores {
		hits = append(hits, lexicalHit{id: id, raw: s})
	}
	sortLexicalHitsDesc(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}
