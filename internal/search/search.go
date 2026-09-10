// Package search routes cached file history separately from durable decision search.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/codeindex"
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
	Path    string
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
	Matcher  string                     `json:"matcher"`
	Query    string                     `json:"query"`
	Results  []Hit                      `json:"results"`
	Coverage *codeindex.HistoryCoverage `json:"coverage,omitempty"`
}

type Hit struct {
	Type string `json:"type"`
	*DecisionHit
	*codeindex.FileHistoryHit
}

// Parse validates and normalizes a search request before any search SQL runs.
func Parse(raw string, limit int) (Query, error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return Query{}, errors.New("search query must be UTF-8 without NUL bytes")
	}
	if limit < 1 {
		return Query{}, errors.New("search limit must be greater than zero")
	}
	if file, ok := strings.CutPrefix(strings.TrimLeftFunc(raw, unicode.IsSpace), "file:"); ok {
		path, err := codeindex.NormalizeHistoryPath(file)
		if err != nil {
			return Query{}, err
		}
		return Query{Text: path, Path: path, Matcher: "exact:file-history", Limit: limit}, nil
	}
	pathQuery := raw
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Query{}, errors.New("search query cannot be empty")
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
	query := Query{Text: text, Matcher: matcher, Limit: limit}
	if matcher == "fts:decision-fallback" && strings.ContainsAny(pathQuery, "/\\.") {
		query.Path, _ = codeindex.NormalizeHistoryPath(pathQuery)
	}
	return query, nil
}

// TryFile precedes any memory-store selection. Explicit decision queries skip
// the cache entirely; an unindexed path-looking query retains decision fallback.
func TryFile(ctx context.Context, root string, query Query) (Response, bool, error) {
	if query.Path == "" {
		return Response{}, false, nil
	}
	history, err := codeindex.ReadFileHistory(ctx, root, query.Path, query.Limit)
	if err != nil {
		return Response{}, true, err
	}
	if query.Matcher != "exact:file-history" && !history.Indexed {
		return Response{}, false, nil
	}
	results := make([]Hit, len(history.Results))
	for i := range history.Results {
		results[i] = Hit{Type: "file_history", FileHistoryHit: &history.Results[i]}
	}
	return Response{Matcher: "exact:file-history", Query: query.Path, Results: results, Coverage: &history.Coverage}, true, nil
}

// Store is the committed decision-search surface used by local and owner-routed
// executions.
type Store interface {
	SearchDecisions(context.Context, string, int) ([]store.DecisionSearchHit, error)
}

func Run(ctx context.Context, st Store, query Query) (Response, error) {
	if query.Matcher == "exact:file-history" {
		return Response{}, errors.New("file-history search must select the code cache before opening memory")
	}
	hits, err := st.SearchDecisions(ctx, query.Text, query.Limit)
	if err != nil {
		return Response{}, err
	}
	results := make([]Hit, len(hits))
	for i, hit := range hits {
		results[i] = Hit{Type: "decision", DecisionHit: &DecisionHit{
			Type: "decision", DecisionID: hit.DecisionID, Title: hit.Title,
			Rationale: hit.Rationale, DecidedAt: hit.DecidedAt, Score: hit.Score,
		}}
	}
	return Response{Matcher: query.Matcher, Query: query.Text, Results: results}, nil
}

func Lines(response Response) []string {
	lines := []string{"Matcher: " + response.Matcher}
	if c := response.Coverage; c != nil {
		lines = append(lines, fmt.Sprintf("Cached Git history only: %d ingested ranges; limit %d; truncated=%t; %d denied results. No current Git observation.", c.Ranges, c.Limit, c.Truncated, c.DeniedResults))
		if c.LastIngest != nil {
			rangeText := c.LastIngest.Head
			if c.LastIngest.Since != nil {
				rangeText = *c.LastIngest.Since + ".." + rangeText
			}
			lines = append(lines, fmt.Sprintf("Last ingested range: %s (shallow=%t, observed %s). Results combine cached ranges; coverage may be incomplete.", rangeText, c.LastIngest.Shallow, c.LastIngest.ObservedAt.Format(time.RFC3339)))
		}
	}
	if len(response.Results) == 0 {
		return append(lines, fmt.Sprintf("No matches for %q.", response.Query))
	}
	for _, hit := range response.Results {
		if hit.FileHistoryHit != nil {
			lines = append(lines, fmt.Sprintf("[%s] %s %q (author: %q, authored: %s) %q", hit.CommitSHA, hit.ChangeType, hit.Path, hit.Author, hit.AuthoredAt, hit.Subject))
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s (score: %.3f, decided: %s)",
			hit.DecisionID, hit.Title, hit.Score, hit.DecidedAt.Format(time.RFC3339)))
		if rationale := strings.TrimSpace(hit.Rationale); rationale != "" {
			lines = append(lines, "  "+strings.Join(strings.Fields(rationale), " "))
		}
	}
	return lines
}
