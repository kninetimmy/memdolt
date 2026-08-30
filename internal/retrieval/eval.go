package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultGoldenPath = "tests/golden/retrieval_golden.json"
	EvaluationK       = 3
	BaselineRecallAt3 = 1.0
)

var ErrBelowBaseline = errors.New("retrieval evaluation is below the recorded baseline")

type GoldenKind string

const (
	GoldenMatch GoldenKind = "match"
	GoldenEmpty GoldenKind = "empty"
)

// GoldenQuery and GoldenFile are the committed memhub-compatible golden JSON
// format from PRD §8.4.
type GoldenQuery struct {
	ID            string     `json:"id"`
	Query         string     `json:"query"`
	Kind          GoldenKind `json:"kind"`
	SourceType    string     `json:"source_type"`
	TitleContains []string   `json:"title_contains"`
	BodyContains  []string   `json:"body_contains"`
	Notes         string     `json:"notes"`
}

type GoldenFile struct {
	Version     int           `json:"version"`
	Description string        `json:"description"`
	Queries     []GoldenQuery `json:"queries"`
}

type QueryOutcome struct {
	ID            string     `json:"id"`
	Query         string     `json:"query"`
	Kind          GoldenKind `json:"kind"`
	Passed        bool       `json:"passed"`
	MatchedRank   *int       `json:"matchedRank"`
	MatchedScore  *float64   `json:"matchedScore"`
	ReturnedCount int        `json:"returnedCount"`
	FailureReason *string    `json:"failureReason"`
}

type EvalTotals struct {
	Queries        int `json:"queries"`
	MatchQueries   int `json:"matchQueries"`
	EmptyQueries   int `json:"emptyQueries"`
	MatchPasses    int `json:"matchPasses"`
	EmptyPasses    int `json:"emptyPasses"`
	SafetyFailures int `json:"safetyFailures"`
}

type EvalSummary struct {
	GoldenPath        string         `json:"goldenPath"`
	Mode              Mode           `json:"mode"`
	K                 int            `json:"k"`
	Totals            EvalTotals     `json:"totals"`
	RecallAtK         float64        `json:"recallAtK"`
	BaselineRecallAtK float64        `json:"baselineRecallAtK"`
	Passed            bool           `json:"passed"`
	Outcomes          []QueryOutcome `json:"outcomes"`
	ElapsedMS         int64          `json:"elapsedMs"`
}

type EvalOptions struct {
	GoldenPath string
	Mode       Mode
}

func LoadGolden(path string) (GoldenFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return GoldenFile{}, fmt.Errorf("reading golden file %s: %w", path, err)
	}
	var golden GoldenFile
	if err := json.Unmarshal(raw, &golden); err != nil {
		return GoldenFile{}, fmt.Errorf("parsing golden file %s: %w", path, err)
	}
	if err := validateGolden(golden, path); err != nil {
		return GoldenFile{}, err
	}
	return golden, nil
}

func validateGolden(golden GoldenFile, path string) error {
	if golden.Version != 1 {
		return fmt.Errorf("unsupported golden file version %d in %s; expected 1", golden.Version, path)
	}
	if len(golden.Queries) == 0 {
		return fmt.Errorf("golden file %s has zero queries", path)
	}
	for _, query := range golden.Queries {
		if strings.TrimSpace(query.ID) == "" {
			return errors.New("golden query missing `id`")
		}
		if strings.TrimSpace(query.Query) == "" {
			return fmt.Errorf("golden query %q has empty `query`", query.ID)
		}
		if query.Kind != GoldenMatch && query.Kind != GoldenEmpty {
			return fmt.Errorf("golden query %q has unknown kind %q (expected match or empty)", query.ID, query.Kind)
		}
		if query.SourceType != "" && !validSourceTypes[query.SourceType] {
			return fmt.Errorf("golden query %q has unknown source_type %q (expected fact, decision, task, or doc_chunk)", query.ID, query.SourceType)
		}
		if query.Kind == GoldenMatch && query.SourceType == "" && len(query.TitleContains) == 0 && len(query.BodyContains) == 0 {
			return fmt.Errorf("golden query %q is kind=match but has no matchers", query.ID)
		}
	}
	return nil
}

// EvaluateGolden runs every query through Recall, the same production path as
// the CLI, and returns ErrBelowBaseline with the complete summary on quality
// or safety regression.
func EvaluateGolden(ctx context.Context, st Store, embeddingsPath string, inference Inference, cfg Config, golden GoldenFile, options EvalOptions) (EvalSummary, error) {
	if err := validateGolden(golden, options.GoldenPath); err != nil {
		return EvalSummary{}, err
	}
	mode := options.Mode
	if mode == "" {
		mode = cfg.Mode
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return EvalSummary{}, err
	}

	started := time.Now()
	outcomes := make([]QueryOutcome, 0, len(golden.Queries))
	for _, query := range golden.Queries {
		response, err := Recall(ctx, st, embeddingsPath, inference, cfg, Options{
			Query: query.Query, Mode: mode, MaxResults: EvaluationK, SkipObservability: true,
		})
		if err != nil {
			return EvalSummary{}, fmt.Errorf("evaluate golden query %s: %w", query.ID, err)
		}
		outcomes = append(outcomes, EvaluateQuery(query, response.Results, EvaluationK))
	}
	summary := SummarizeEvaluation(options.GoldenPath, mode, outcomes, time.Since(started))
	if !summary.Passed {
		return summary, fmt.Errorf("%w: Recall@3 %.4f (%d/%d), safety failures %d",
			ErrBelowBaseline, summary.RecallAtK, summary.Totals.MatchPasses,
			summary.Totals.MatchQueries, summary.Totals.SafetyFailures)
	}
	return summary, nil
}

func EvaluateQuery(query GoldenQuery, hits []Hit, k int) QueryOutcome {
	outcome := QueryOutcome{
		ID: query.ID, Query: query.Query, Kind: query.Kind, ReturnedCount: len(hits),
	}
	if query.Kind == GoldenEmpty {
		outcome.Passed = len(hits) == 0
		if !outcome.Passed {
			leaked := make([]string, 0, min(k, 3))
			for _, hit := range hits[:min(len(hits), min(k, 3))] {
				leaked = append(leaked, fmt.Sprintf("%s#%s", hit.SourceType, hit.SourceID))
			}
			reason := fmt.Sprintf("expected empty bundle but recall returned %d hit(s): %s", len(hits), strings.Join(leaked, ", "))
			outcome.FailureReason = &reason
		}
		return outcome
	}

	limit := min(k, len(hits))
	for _, hit := range hits[:limit] {
		if HitMatches(query, hit) {
			rank, score := hit.Rank, hit.Score
			outcome.Passed, outcome.MatchedRank, outcome.MatchedScore = true, &rank, &score
			return outcome
		}
	}
	reason := "recall returned no results"
	if len(hits) > 0 {
		top := make([]string, 0, limit)
		for _, hit := range hits[:limit] {
			top = append(top, fmt.Sprintf("%s#%s:%s", hit.SourceType, hit.SourceID, truncateEval(hit.Title, 40)))
		}
		reason = fmt.Sprintf("no top-%d hit matched (returned %d): %s", k, len(hits), strings.Join(top, " | "))
	}
	outcome.FailureReason = &reason
	return outcome
}

func HitMatches(query GoldenQuery, hit Hit) bool {
	if query.SourceType != "" && hit.SourceType != query.SourceType {
		return false
	}
	title := strings.ToLower(hit.Title)
	for _, expected := range query.TitleContains {
		if !strings.Contains(title, strings.ToLower(expected)) {
			return false
		}
	}
	body := strings.ToLower(hit.Body)
	for _, expected := range query.BodyContains {
		if !strings.Contains(body, strings.ToLower(expected)) {
			return false
		}
	}
	return true
}

func SummarizeEvaluation(path string, mode Mode, outcomes []QueryOutcome, elapsed time.Duration) EvalSummary {
	totals := EvalTotals{Queries: len(outcomes)}
	for _, outcome := range outcomes {
		switch outcome.Kind {
		case GoldenMatch:
			totals.MatchQueries++
			if outcome.Passed {
				totals.MatchPasses++
			}
		case GoldenEmpty:
			totals.EmptyQueries++
			if outcome.Passed {
				totals.EmptyPasses++
			} else {
				totals.SafetyFailures++
			}
		}
	}
	recall := 0.0
	if totals.MatchQueries > 0 {
		recall = float64(totals.MatchPasses) / float64(totals.MatchQueries)
	}
	return EvalSummary{
		GoldenPath: path, Mode: mode, K: EvaluationK, Totals: totals,
		RecallAtK: recall, BaselineRecallAtK: BaselineRecallAt3,
		Passed:   recall >= BaselineRecallAt3 && totals.SafetyFailures == 0,
		Outcomes: outcomes, ElapsedMS: elapsed.Milliseconds(),
	}
}

func truncateEval(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}
