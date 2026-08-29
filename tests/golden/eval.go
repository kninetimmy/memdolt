//go:build golden

package golden

import (
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

// The rig and the user-facing evaluator share one golden parser and matcher.
type goldenQuery = retrieval.GoldenQuery
type goldenFile = retrieval.GoldenFile

func loadGoldenFile(path string) (*goldenFile, error) {
	golden, err := retrieval.LoadGolden(path)
	return &golden, err
}

type queryOutcome struct {
	id            string
	kind          string
	passed        bool
	returnedCount int
	failureReason string
}

func evaluateQuery(query goldenQuery, hits []resultHit, k int) queryOutcome {
	productionHits := make([]retrieval.Hit, len(hits))
	for i, hit := range hits {
		productionHits[i] = retrieval.Hit{
			Rank: hit.rank, SourceType: hit.sourceType, SourceID: hit.id,
			Title: hit.title, Body: hit.body, Score: hit.score,
		}
	}
	outcome := retrieval.EvaluateQuery(query, productionHits, k)
	failure := ""
	if outcome.FailureReason != nil {
		failure = *outcome.FailureReason
	}
	return queryOutcome{
		id: outcome.ID, kind: string(outcome.Kind), passed: outcome.Passed,
		returnedCount: outcome.ReturnedCount, failureReason: failure,
	}
}

func hitMatches(query goldenQuery, hit resultHit) bool {
	return retrieval.HitMatches(query, retrieval.Hit{
		SourceType: hit.sourceType, SourceID: hit.id, Title: hit.title, Body: hit.body,
	})
}

type evalSummary struct {
	gathererName   string
	matchQueries   int
	emptyQueries   int
	matchPasses    int
	emptyPasses    int
	recallAtK      float64
	safetyFailures int
	outcomes       []queryOutcome
}

func summarize(name string, outcomes []queryOutcome) evalSummary {
	summary := evalSummary{gathererName: name, outcomes: outcomes}
	for _, outcome := range outcomes {
		switch outcome.kind {
		case "match":
			summary.matchQueries++
			if outcome.passed {
				summary.matchPasses++
			}
		case "empty":
			summary.emptyQueries++
			if outcome.passed {
				summary.emptyPasses++
			} else {
				summary.safetyFailures++
			}
		}
	}
	if summary.matchQueries > 0 {
		summary.recallAtK = float64(summary.matchPasses) / float64(summary.matchQueries)
	}
	return summary
}

func meetsGate(summary evalSummary) bool {
	return summary.recallAtK >= retrieval.BaselineRecallAt3 && summary.safetyFailures == 0
}
