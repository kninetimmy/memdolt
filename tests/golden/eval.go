//go:build golden

package golden

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// goldenQuery and goldenFile mirror memhub's src/commands/eval.rs
// GoldenQuery/GoldenFile shape exactly (field-for-field), so
// retrieval_golden.json parses unmodified.
type goldenQuery struct {
	ID            string   `json:"id"`
	Query         string   `json:"query"`
	Kind          string   `json:"kind"` // "match" or "empty"
	SourceType    string   `json:"source_type"`
	TitleContains []string `json:"title_contains"`
	BodyContains  []string `json:"body_contains"`
	Notes         string   `json:"notes"`
}

type goldenFile struct {
	Version     int           `json:"version"`
	Description string        `json:"description"`
	Queries     []goldenQuery `json:"queries"`
}

func loadGoldenFile(path string) (*goldenFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if gf.Version != 1 {
		return nil, fmt.Errorf("unsupported golden file version %d in %s; expected 1", gf.Version, path)
	}
	if len(gf.Queries) == 0 {
		return nil, fmt.Errorf("golden file %s has zero queries", path)
	}
	return &gf, nil
}

// queryOutcome mirrors memhub's eval.rs QueryOutcome (the fields this rig
// needs: match/empty pass, and a human-readable failure reason).
type queryOutcome struct {
	id            string
	kind          string
	passed        bool
	returnedCount int
	failureReason string
}

// evaluateQuery mirrors memhub's src/commands/eval.rs evaluate_query and
// hit_matches exactly: kind=empty passes iff the result set is empty;
// kind=match passes iff some hit within the top min(k, len(results)) has
// the expected source_type (when given) and contains every
// title_contains/body_contains substring, case-insensitive.
func evaluateQuery(q goldenQuery, hits []resultHit, k int) queryOutcome {
	returned := len(hits)
	if q.Kind == "empty" {
		if returned == 0 {
			return queryOutcome{id: q.ID, kind: q.Kind, passed: true, returnedCount: returned}
		}
		leaked := make([]string, 0, len(hits))
		for _, h := range hits {
			if len(leaked) >= min(k, 3) {
				break
			}
			leaked = append(leaked, fmt.Sprintf("%s#%s", h.sourceType, h.id))
		}
		return queryOutcome{
			id: q.ID, kind: q.Kind, passed: false, returnedCount: returned,
			failureReason: fmt.Sprintf("expected empty bundle but recall returned %d hit(s): %s",
				returned, strings.Join(leaked, ", ")),
		}
	}

	limit := min(k, returned)
	for _, h := range hits[:limit] {
		if hitMatches(q, h) {
			return queryOutcome{id: q.ID, kind: q.Kind, passed: true, returnedCount: returned}
		}
	}
	reason := "recall returned no results"
	if returned > 0 {
		top := make([]string, 0, limit)
		for _, h := range hits[:limit] {
			top = append(top, fmt.Sprintf("%s#%s:%s", h.sourceType, h.id, truncate(h.title, 40)))
		}
		reason = fmt.Sprintf("no top-%d hit matched (returned %d): %s", k, returned, strings.Join(top, " | "))
	}
	return queryOutcome{id: q.ID, kind: q.Kind, passed: false, returnedCount: returned, failureReason: reason}
}

func hitMatches(q goldenQuery, h resultHit) bool {
	if q.SourceType != "" && h.sourceType != q.SourceType {
		return false
	}
	titleLower := strings.ToLower(h.title)
	for _, needle := range q.TitleContains {
		if !strings.Contains(titleLower, strings.ToLower(needle)) {
			return false
		}
	}
	bodyLower := strings.ToLower(h.body)
	for _, needle := range q.BodyContains {
		if !strings.Contains(bodyLower, strings.ToLower(needle)) {
			return false
		}
	}
	return true
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// evalSummary mirrors memhub's eval.rs EvalSummary (the fields this rig's
// gate needs).
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
	s := evalSummary{gathererName: name, outcomes: outcomes}
	for _, o := range outcomes {
		switch o.kind {
		case "match":
			s.matchQueries++
			if o.passed {
				s.matchPasses++
			}
		case "empty":
			s.emptyQueries++
			if o.passed {
				s.emptyPasses++
			} else {
				s.safetyFailures++
			}
		}
	}
	if s.matchQueries > 0 {
		s.recallAtK = float64(s.matchPasses) / float64(s.matchQueries)
	}
	return s
}

// meetsGate reports whether s clears the PRD §16 gate: Recall@K at least
// memhub's recorded baseline (100%, 21/21 on this 22-query golden set) and
// zero safety failures.
func meetsGate(s evalSummary) bool {
	const baselineRecall = 1.0
	return s.recallAtK >= baselineRecall && s.safetyFailures == 0
}
