package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/retrieval"
)

func TestEvalRetrievalCLIReportsOutcomesAndFailsBelowBaseline(t *testing.T) {
	base := initStore(t)
	runMemdolt(t, "task", "add", "Ship retrieval evaluator", "--notes", "exercise golden outcomes", "--dir", base)
	passing := retrieval.GoldenFile{Version: 1, Queries: []retrieval.GoldenQuery{
		{ID: "task-match", Query: "retrieval evaluator golden outcomes", Kind: retrieval.GoldenMatch, SourceType: "task", TitleContains: []string{"retrieval evaluator"}, BodyContains: []string{"golden"}},
		{ID: "nonsense-empty", Query: "zxqvunmatchedtoken", Kind: retrieval.GoldenEmpty},
	}}
	writeGolden(t, base, "passing.json", passing)

	out, err := runMemdoltResult(t, "eval", "retrieval", "--golden", "passing.json", "--mode", "fts", "--dir", base, "--json")
	if err != nil {
		t.Fatalf("passing retrieval eval: %v (output %q)", err, out)
	}
	summary := decodeJSON[retrieval.EvalSummary](t, out)
	if !summary.Passed || summary.Mode != retrieval.ModeFTS || summary.K != 3 ||
		summary.Totals.MatchPasses != 1 || summary.Totals.EmptyPasses != 1 ||
		summary.Totals.SafetyFailures != 0 || summary.RecallAtK != 1 || len(summary.Outcomes) != 2 {
		t.Fatalf("passing eval summary = %+v", summary)
	}
	human := runMemdolt(t, "eval", "retrieval", "--golden", "passing.json", "--mode", "fts", "--dir", base)
	for _, want := range []string{"Recall@3: 1/1 = 100.0%", "Safety: 1/1", "[PASS] task-match (match)", "[PASS] nonsense-empty (empty)"} {
		if !strings.Contains(human, want) {
			t.Errorf("human eval output %q does not contain %q", human, want)
		}
	}

	failing := retrieval.GoldenFile{Version: 1, Queries: []retrieval.GoldenQuery{
		{ID: "match-regression", Query: "retrieval evaluator", Kind: retrieval.GoldenMatch, SourceType: "decision", TitleContains: []string{"not present"}},
		{ID: "safety-regression", Query: "retrieval evaluator", Kind: retrieval.GoldenEmpty},
	}}
	writeGolden(t, base, "failing.json", failing)
	out, err = runMemdoltResult(t, "eval", "retrieval", "--golden", "failing.json", "--mode", "fts", "--dir", base, "--json")
	if !errors.Is(err, retrieval.ErrBelowBaseline) {
		t.Fatalf("failing retrieval eval error = %v, want ErrBelowBaseline (output %q)", err, out)
	}
	summary = decodeJSON[retrieval.EvalSummary](t, out)
	if summary.Passed || summary.Totals.MatchPasses != 0 || summary.Totals.SafetyFailures != 1 || len(summary.Outcomes) != 2 {
		t.Fatalf("failing eval summary = %+v", summary)
	}
	for _, outcome := range summary.Outcomes {
		if outcome.Passed || outcome.FailureReason == nil {
			t.Errorf("failing outcome omitted failure detail: %+v", outcome)
		}
	}
}

func writeGolden(t *testing.T, base, name string, golden retrieval.GoldenFile) {
	t.Helper()
	raw, err := json.Marshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
