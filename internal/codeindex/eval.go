package codeindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/retrieval"
)

const DefaultGoldenPath = "tests/golden/code_locate_golden.json"

var ErrBelowBaseline = errors.New("locate evaluation fell below the match or requested safety baseline")

// GoldenFile and GoldenQuery retain memhub v0.2.0's exact version-1 format.
type GoldenFile struct {
	Version     int           `json:"version"`
	Description string        `json:"description"`
	Queries     []GoldenQuery `json:"queries"`
}

type GoldenQuery struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	Kind           string   `json:"kind"`
	PathContains   []string `json:"path_contains"`
	SymbolContains *string  `json:"symbol_contains"`
	Notes          string   `json:"notes"`
}

type EvalOptions struct {
	GoldenPath     string
	K              int
	UseReranker    bool
	MinRerankScore *float32
}

type QueryOutcome struct {
	ID            string   `json:"id"`
	Query         string   `json:"query"`
	Kind          string   `json:"kind"`
	Passed        bool     `json:"passed"`
	PassedAt1     bool     `json:"passedAt1"`
	MatchedRank   *int     `json:"matchedRank"`
	MatchedScore  *float64 `json:"matchedScore"`
	MatchedRerank *float32 `json:"matchedRerank"`
	ReturnedCount int      `json:"returnedCount"`
	FailureReason *string  `json:"failureReason"`
}

type EvalSummary struct {
	GoldenPath     string         `json:"goldenPath"`
	Mode           retrieval.Mode `json:"mode"`
	K              int            `json:"k"`
	Reranked       bool           `json:"reranked"`
	MinRerankScore *float32       `json:"minRerankScore"`
	TotalQueries   int            `json:"totalQueries"`
	MatchQueries   int            `json:"matchQueries"`
	EmptyQueries   int            `json:"emptyQueries"`
	MatchPassesAt1 int            `json:"matchPassesAt1"`
	MatchPassesAtK int            `json:"matchPassesAtK"`
	EmptyPasses    int            `json:"emptyPasses"`
	RecallAt1      float64        `json:"recallAt1"`
	RecallAtK      float64        `json:"recallAtK"`
	SafetyFailures int            `json:"safetyFailures"`
	Outcomes       []QueryOutcome `json:"outcomes"`
	ElapsedMS      int64          `json:"elapsedMs"`
}

func LoadGolden(file string) (GoldenFile, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return GoldenFile{}, fmt.Errorf("read locate golden (use --golden <path>): %w", err)
	}
	var golden GoldenFile
	if err := json.Unmarshal(raw, &golden); err != nil {
		return golden, fmt.Errorf("parse locate golden: %w", err)
	}
	if golden.Version != 1 || len(golden.Queries) == 0 {
		return golden, errors.New("locate golden must be version 1 with at least one query")
	}
	for _, q := range golden.Queries {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Query) == "" || q.Kind != "match" && q.Kind != "empty" {
			return golden, errors.New("each locate golden query needs an id, query and match/empty kind")
		}
		matcher := q.SymbolContains != nil && *q.SymbolContains != ""
		for _, needle := range q.PathContains {
			matcher = matcher || strings.TrimSpace(needle) != ""
		}
		if q.Kind == "match" && !matcher {
			return golden, fmt.Errorf("locate golden query %s needs path_contains or symbol_contains", q.ID)
		}
	}
	return golden, nil
}

// Evaluate runs every query through Locate with lazy refresh. The optional
// floor exists here alone; runtime fusion/reranking never filters by a floor.
// Fusion's documented empty-probe leakage is reported, not a failing gate.
func Evaluate(ctx context.Context, start string, engine retrieval.Inference, golden GoldenFile, opts EvalOptions) (summary EvalSummary, err error) {
	if opts.K < 1 {
		return summary, errors.New("eval locate K must be greater than zero")
	}
	if opts.MinRerankScore != nil && (math.IsNaN(float64(*opts.MinRerankScore)) || math.IsInf(float64(*opts.MinRerankScore), 0)) {
		return summary, errors.New("eval locate rerank floor must be finite")
	}
	started := time.Now()
	lazy := &localInference{ctx: ctx}
	if engine == nil {
		engine = lazy
	}
	defer func() { err = errors.Join(err, lazy.close()) }()
	summary = EvalSummary{GoldenPath: opts.GoldenPath, K: opts.K, MinRerankScore: opts.MinRerankScore, TotalQueries: len(golden.Queries), Outcomes: []QueryOutcome{}}
	for _, q := range golden.Queries {
		response, err := Locate(ctx, start, engine, Options{Query: q.Query, Limit: opts.K, UseReranker: opts.UseReranker})
		if err != nil {
			return summary, err
		}
		summary.Mode, summary.Reranked = response.Mode, response.Reranked
		outcome := EvaluateQuery(q, response, opts.K, opts.MinRerankScore)
		summary.Outcomes = append(summary.Outcomes, outcome)
		if q.Kind == "match" {
			summary.MatchQueries++
			if outcome.Passed {
				summary.MatchPassesAtK++
			}
			if outcome.PassedAt1 {
				summary.MatchPassesAt1++
			}
		} else {
			summary.EmptyQueries++
			if outcome.Passed {
				summary.EmptyPasses++
			} else {
				summary.SafetyFailures++
			}
		}
	}
	if summary.MatchQueries != 0 {
		summary.RecallAt1 = float64(summary.MatchPassesAt1) / float64(summary.MatchQueries)
		summary.RecallAtK = float64(summary.MatchPassesAtK) / float64(summary.MatchQueries)
	}
	summary.ElapsedMS = time.Since(started).Milliseconds()
	if summary.MatchPassesAtK != summary.MatchQueries || summary.Reranked && opts.MinRerankScore != nil && summary.SafetyFailures > 0 {
		return summary, ErrBelowBaseline
	}
	return summary, nil
}

// EvaluateQuery applies the floor to the returned top-K, then uses
// case-insensitive AND substring matching and post-floor positions.
func EvaluateQuery(q GoldenQuery, response Response, k int, floor *float32) QueryOutcome {
	out := QueryOutcome{ID: q.ID, Query: q.Query, Kind: q.Kind}
	var survivors []Hit
	for _, hit := range response.Results {
		if response.Reranked && floor != nil && (hit.RerankScore == nil || *hit.RerankScore < *floor) {
			continue
		}
		survivors = append(survivors, hit)
	}
	out.ReturnedCount = len(survivors)
	if q.Kind == "empty" {
		out.Passed = len(survivors) == 0
		out.PassedAt1 = out.Passed
	} else {
		for i, hit := range survivors[:min(k, len(survivors))] {
			matched := true
			for _, needle := range q.PathContains {
				matched = matched && strings.Contains(strings.ToLower(hit.Path), strings.ToLower(needle))
			}
			if q.SymbolContains != nil && *q.SymbolContains != "" {
				matched = matched && hit.Symbol != nil && strings.Contains(strings.ToLower(*hit.Symbol), strings.ToLower(*q.SymbolContains))
			}
			if matched {
				rank := i + 1
				out.Passed, out.PassedAt1 = true, rank == 1
				out.MatchedRank, out.MatchedScore, out.MatchedRerank = &rank, &hit.Score, hit.RerankScore
				break
			}
		}
	}
	if !out.Passed {
		var top []string
		for _, hit := range survivors[:min(k, len(survivors))] {
			label := fmt.Sprintf("%s:%d", hit.Path, hit.StartLine)
			if hit.Symbol != nil {
				label += " [" + *hit.Symbol + "]"
			}
			top = append(top, label)
		}
		reason := fmt.Sprintf("expected %s at K=%d; returned %d: %s", q.Kind, k, len(survivors), strings.Join(top, " | "))
		out.FailureReason = &reason
	}
	return out
}
