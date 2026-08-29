package retrieval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGoldenValidatesFormatAndEvaluateQueryMatchesSubstrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golden.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"queries":[{"id":"decision","query":"why","kind":"match","source_type":"decision","title_contains":["LOCAL"],"body_contains":["windows"]},{"id":"empty","query":"none","kind":"empty"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	golden, err := LoadGolden(path)
	if err != nil {
		t.Fatal(err)
	}
	match := EvaluateQuery(golden.Queries[0], []Hit{{Rank: 1, SourceType: "decision", SourceID: "d1", Title: "Keep setup local", Body: "Avoid WINDOWS friction", Score: 4.2}}, 3)
	if !match.Passed || match.MatchedRank == nil || *match.MatchedRank != 1 || match.MatchedScore == nil || *match.MatchedScore != 4.2 {
		t.Fatalf("match outcome = %+v", match)
	}
	safety := EvaluateQuery(golden.Queries[1], []Hit{{SourceType: "fact", SourceID: "f1"}}, 3)
	if safety.Passed || safety.FailureReason == nil || !strings.Contains(*safety.FailureReason, "fact#f1") {
		t.Fatalf("safety outcome = %+v", safety)
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"queries":[{"id":"bad","query":"x","kind":"match"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGolden(path); err == nil || !strings.Contains(err.Error(), "no matchers") {
		t.Fatalf("matcher-free golden error = %v", err)
	}
}
