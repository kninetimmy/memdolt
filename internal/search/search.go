// Package search implements PRD §8's text-search surface over durable memory.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kninetimmy/memdolt/internal/store"
)

const DefaultLimit = 10

var decisionPrefixes = []string{
	"find decisions about ",
	"decisions about ",
	"decision about ",
	"decision:",
	"decisions:",
	"decision ",
	"decisions ",
}

type Query struct {
	Text    string
	Matcher string
	Limit   int
}

type DecisionHit struct {
	Type       string    `json:"type"`
	DecisionID string    `json:"decisionId"`
	Title      string    `json:"title"`
	Rationale  string    `json:"rationale"`
	DecidedAt  time.Time `json:"decidedAt"`
	Score      float64   `json:"score"`
}

type Response struct {
	Matcher string        `json:"matcher"`
	Query   string        `json:"query"`
	Results []DecisionHit `json:"results"`
}

// Parse validates and normalizes a search request before any search SQL runs.
func Parse(raw string, limit int) (Query, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Query{}, errors.New("search query cannot be empty")
	}
	if strings.HasPrefix(raw, "file:") {
		return Query{}, errors.New("file-history search is unavailable until the M5 code-index/git-ingest milestone; use a decision text query for M2")
	}
	if limit < 1 {
		return Query{}, errors.New("search limit must be greater than zero")
	}

	text := raw
	matcher := "fts:decision-fallback"
	for _, prefix := range decisionPrefixes {
		if rest, ok := strings.CutPrefix(raw, prefix); ok {
			text = strings.TrimSpace(rest)
			matcher = "fts:decision"
			break
		}
	}
	if !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
		return Query{}, errors.New("search query must include at least one searchable token")
	}
	return Query{Text: text, Matcher: matcher, Limit: limit}, nil
}

// Store is the committed decision-search surface used by local and owner-routed
// executions.
type Store interface {
	SearchDecisions(context.Context, string, int) ([]store.DecisionSearchHit, error)
}

func Run(ctx context.Context, st Store, query Query) (Response, error) {
	hits, err := st.SearchDecisions(ctx, query.Text, query.Limit)
	if err != nil {
		return Response{}, err
	}
	results := make([]DecisionHit, len(hits))
	for i, hit := range hits {
		results[i] = DecisionHit{
			Type: "decision", DecisionID: hit.DecisionID, Title: hit.Title,
			Rationale: hit.Rationale, DecidedAt: hit.DecidedAt, Score: hit.Score,
		}
	}
	return Response{Matcher: query.Matcher, Query: query.Text, Results: results}, nil
}

func Lines(response Response) []string {
	lines := []string{"Matcher: " + response.Matcher}
	if len(response.Results) == 0 {
		return append(lines, fmt.Sprintf("No matches for %q.", response.Query))
	}
	for _, hit := range response.Results {
		lines = append(lines, fmt.Sprintf("[%s] %s (score: %.3f, decided: %s)",
			hit.DecisionID, hit.Title, hit.Score, hit.DecidedAt.Format(time.RFC3339)))
		if rationale := strings.TrimSpace(hit.Rationale); rationale != "" {
			lines = append(lines, "  "+strings.Join(strings.Fields(rationale), " "))
		}
	}
	return lines
}
