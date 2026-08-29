//go:build golden

package golden

import (
	"context"
	"testing"
	"time"
)

var scaleCandidatePoolSizes = [...]int{rerankPoolSize, 40, 80}

// TestRetrievalGoldenAtScale runs the original 22-query golden set against
// the hermetic fixture PLUS scaleCorpus()'s deterministic hard negatives —
// an order of magnitude past rerankPoolSize — through all three gatherer
// configurations at the production-default candidate pool and a checked-in
// sweep of larger pools. It measures what the 21-row fixture cannot:
// per-gatherer Recall@3 and how many wanted targets fell outside each rerank
// pool, i.e. were evicted before the cross-encoder ever saw them
// (docs/spikes/m0-rig3.md §11).
//
// Alternatives remain measurements: their retrieval numbers are logged
// whatever they turn out to be. The selected vector-only pool=20 strategy is
// asserted, along with infrastructure integrity (no gather/pipeline error;
// each matcher maps to exactly one fixture row). The original hermetic gate
// stays in TestRetrievalGolden against its own small fixture.
func TestRetrievalGoldenAtScale(t *testing.T) {
	ctx := context.Background()
	gf, err := loadGoldenFile("retrieval_golden.json")
	if err != nil {
		t.Fatalf("%v", err)
	}

	buildStart := time.Now()
	negatives := scaleCorpus(t, gf)
	fixtureCount := len(seedRows())
	h := setupPipelineHarness(t, negatives...)
	t.Logf("scale corpus built: %d fixture rows + %d synthetic hard negatives = %d embedded rows (%s)",
		fixtureCount, len(negatives), len(h.rows), time.Since(buildStart).Round(time.Second))

	// Pin every match-kind query to its one wanted row among the ORIGINAL
	// fixture rows. Negatives are screened by construction never to satisfy
	// any matcher (scaleCorpus), so Recall@3 here genuinely measures whether
	// that one row reached the top 3.
	fixtureRows := seedRows()
	wanted := make(map[string]rowKey, len(gf.Queries))
	for _, q := range gf.Queries {
		if q.Kind != "match" {
			continue
		}
		var found []rowKey
		for _, r := range fixtureRows {
			if hitMatches(q, resultHit{sourceType: r.sourceType, id: r.id, title: r.title, body: r.body}) {
				found = append(found, rowKey{r.sourceType, r.id})
			}
		}
		if len(found) != 1 {
			t.Fatalf("query %s matches %d fixture rows (%v), want exactly 1", q.ID, len(found), found)
		}
		wanted[q.ID] = found[0]
	}

	configs := []struct {
		new      func() lexicalGatherer
		selected bool
	}{
		{new: func() lexicalGatherer { return newFulltextGatherer(h.db) }},
		{new: func() lexicalGatherer { return newBM25Gatherer(h.rows) }},
		{new: func() lexicalGatherer { return noLexicalGatherer{} }, selected: true},
	}
	for _, config := range configs {
		h.lexical = config.new()
		for _, poolSize := range scaleCandidatePoolSizes {
			configStart := time.Now()
			evicted := 0
			outcomes := make([]queryOutcome, 0, len(gf.Queries))
			for _, q := range gf.Queries {
				pool, err := fusedPool(ctx, h, q.Query)
				if err != nil {
					t.Fatalf("candidate pool %s (%s, pool=%d): %v", q.ID, h.lexical.Name(), poolSize, err)
				}
				targetEvicted := false
				if key, ok := wanted[q.ID]; ok {
					targetEvicted = true
					poolTop := pool
					if len(poolTop) > poolSize {
						poolTop = poolTop[:poolSize]
					}
					for _, sh := range poolTop {
						if sh.row.sourceType == key.sourceType && sh.row.id == key.id {
							targetEvicted = false
							break
						}
					}
					if targetEvicted {
						evicted++
						t.Logf("[%s pool=%d] EVICTED %s: wanted target %s fell outside the rerank pool",
							h.lexical.Name(), poolSize, q.ID, key.String())
					}
				}
				hits, err := runQueryWithPoolSize(ctx, h, q.Query, poolSize)
				if err != nil {
					t.Fatalf("query %s (%s, pool=%d): %v", q.ID, h.lexical.Name(), poolSize, err)
				}
				outcomes = append(outcomes, evaluateQuery(q, hits, 3))
			}
			s := summarize(h.lexical.Name(), outcomes)
			logSummary(t, s)
			t.Logf("[SCALE %s pool=%d] recall@3=%.4f (%d/%d match passes) safety_failures=%d wanted targets outside rerank pool=%d/%d query-loop time=%s",
				h.lexical.Name(), poolSize, s.recallAtK, s.matchPasses, s.matchQueries, s.safetyFailures,
				evicted, len(wanted), time.Since(configStart).Round(time.Millisecond))
			if config.selected && poolSize == rerankPoolSize && !meetsGate(s) {
				t.Errorf("selected vector-only pool=%d strategy: recall@3=%.4f (%d/%d), safety_failures=%d",
					poolSize, s.recallAtK, s.matchPasses, s.matchQueries, s.safetyFailures)
			}
		}
	}
}
