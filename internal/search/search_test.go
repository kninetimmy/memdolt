package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/codeindex"
)

func TestParseDecisionFallbackPrefixesAndRefusals(t *testing.T) {
	plain, err := Parse("  Windows onboarding  ", DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Text != "Windows onboarding" || plain.Matcher != "fts:decision-fallback" {
		t.Fatalf("plain query = %+v", plain)
	}
	prefixed, err := Parse("decisions about Windows onboarding", 4)
	if err != nil {
		t.Fatal(err)
	}
	if prefixed.Text != "Windows onboarding" || prefixed.Matcher != "fts:decision" || prefixed.Limit != 4 {
		t.Fatalf("prefixed query = %+v", prefixed)
	}
	colon, err := Parse("decision:Windows onboarding", 4)
	if err != nil || colon.Text != "Windows onboarding" || colon.Matcher != "fts:decision" {
		t.Fatalf("colon-prefixed query = %+v, error %v", colon, err)
	}

	for _, test := range []struct {
		query string
		limit int
		want  string
	}{
		{"", 10, "cannot be empty"},
		{"decision:", 10, "searchable token"},
		{"decision: !!!", 10, "searchable token"},
		{"decision:.-", 10, "searchable token"},
		{"decisions about .☃", 10, "searchable token"},
		{"☃", 10, "searchable token"},
		{"!!!", 10, "searchable token"},
		{"file:", 10, "repository-relative path"},
		{"file:a/../b", 10, "repository-relative path"},
		{"\xff", 10, "UTF-8 without NUL"},
		{"file:.\x00-", 10, "UTF-8 without NUL"},
		{"decision: storage", 0, "greater than zero"},
	} {
		if _, err := Parse(test.query, test.limit); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Parse(%q, %d) error = %v, want %q", test.query, test.limit, err, test.want)
		}
	}
}

func TestFileHistorySymbolCandidatesValidateDecisionsAfterSelection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, raw := range []string{".-", ".☃", "☃/⚙", "☃\\⚙"} {
		query, err := Parse(raw, 10)
		if err != nil || query.Path != strings.ReplaceAll(raw, "\\", "/") || query.Matcher != "fts:decision-fallback" {
			t.Fatalf("symbol-only candidate=%+v %v", query, err)
		}
		if _, handled, err := TryFile(ctx, root, query); !handled || err == nil || !strings.Contains(err.Error(), "searchable token") {
			t.Fatalf("unindexed symbol candidate reached store selection: handled=%t %v", handled, err)
		}
		if _, err := Run(ctx, nil, query); err == nil || !strings.Contains(err.Error(), "searchable token") {
			t.Fatalf("misrouted symbol candidate reached decision backend: %v", err)
		}
	}
	query, err := Parse("a/../b", 10)
	if err != nil || query.Path != "" || query.Matcher != "fts:decision-fallback" {
		t.Fatalf("invalid file candidate lost ordinary decision fallback: %+v %v", query, err)
	}
	if err := os.Mkdir(filepath.Join(root, ".memdolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".memdolt", "code_index.lock"), []byte("synthetic busy marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	query, err = Parse(".-", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, handled, err := TryFile(ctx, root, query); !handled || !errors.Is(err, codeindex.ErrBusy) {
		t.Fatalf("symbol candidate hid cache selection failure: handled=%t %v", handled, err)
	}
}

func TestFileHistorySearchParsingPreservesPathsAndDecisionPriority(t *testing.T) {
	for _, raw := range []string{"file:src/雪 name.go", "file: spaced.go ", "file:dir\\name.go"} {
		query, err := Parse(raw, 10)
		if err != nil || query.Matcher != "exact:file-history" || query.Path != strings.ReplaceAll(strings.TrimPrefix(raw, "file:"), "\\", "/") {
			t.Fatalf("file query=%+v %v", query, err)
		}
	}
	for _, raw := range []string{"decision:src/main.go", "decisions about v1.2"} {
		query, err := Parse(raw, 10)
		if err != nil || query.Matcher != "fts:decision" || query.Path != "" {
			t.Fatalf("decision query=%+v %v", query, err)
		}
	}
	query, err := Parse("src/main.go", 10)
	if err != nil || query.Path != "src/main.go" || query.Matcher != "fts:decision-fallback" {
		t.Fatalf("path candidate=%+v %v", query, err)
	}
	query, err = Parse("  file:src/main.go", 10)
	if err != nil || query.Matcher != "exact:file-history" || query.Path != "src/main.go" {
		t.Fatalf("leading prefix whitespace=%+v %v", query, err)
	}
}
