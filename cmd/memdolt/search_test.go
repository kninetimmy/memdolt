package main

import (
	"context"
	"strings"
	"testing"
	"time"

	searchpkg "github.com/kninetimmy/memdolt/internal/search"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestSearchCLIProvidesStableDecisionJSONAndClearRefusals(t *testing.T) {
	base := initStore(t)
	actor := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	decided := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	_, err = st.Commit(context.Background(), store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "INSERT INTO decisions (id, title, rationale, status, source, decided_at) VALUES (?, ?, ?, 'active', 'user', ?)", Args: []any{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "Bundle local SQLite", "Avoid Windows onboarding friction.", decided}},
			{SQL: "INSERT INTO decisions (id, title, rationale, status, source, decided_at) VALUES (?, ?, ?, 'active', 'user', ?)", Args: []any{"01ARZ3NDEKTSV4RRFFQ69G5FAW", "Document Windows onboarding", "Keep local setup repeatable.", decided.Add(time.Hour)}},
		},
		Text:    []string{"Bundle local SQLite", "Avoid Windows onboarding friction.", "Document Windows onboarding", "Keep local setup repeatable."},
		Message: "seed decision search", Author: actor,
	})
	if closeErr := st.Close(); err != nil || closeErr != nil {
		t.Fatalf("seed decisions: commit=%v close=%v", err, closeErr)
	}

	first := runMemdolt(t, "search", "Windows onboarding", "--dir", base, "--json")
	second := runMemdolt(t, "search", "Windows onboarding", "--dir", base, "--json")
	if first != second {
		t.Fatalf("search JSON order is unstable:\nfirst:  %s\nsecond: %s", first, second)
	}
	response := decodeJSON[searchpkg.Response](t, first)
	if response.Matcher != "fts:decision-fallback" || response.Query != "Windows onboarding" || len(response.Results) != 2 {
		t.Fatalf("plain search response = %+v", response)
	}
	for _, hit := range response.Results {
		if hit.Type != "decision" || hit.DecisionID == "" || hit.Title == "" || hit.Rationale == "" || hit.Score <= 0 || hit.DecidedAt.IsZero() {
			t.Errorf("incomplete decision search hit = %+v", hit)
		}
	}

	prefixed := decodeJSON[searchpkg.Response](t, runMemdolt(t,
		"search", "decisions about Windows onboarding", "--limit", "1", "--dir", base, "--json"))
	if prefixed.Matcher != "fts:decision" || prefixed.Query != "Windows onboarding" || len(prefixed.Results) != 1 {
		t.Fatalf("prefixed search response = %+v", prefixed)
	}
	empty := decodeJSON[searchpkg.Response](t, runMemdolt(t,
		"search", "noresulttoken", "--dir", base, "--json"))
	if empty.Results == nil || len(empty.Results) != 0 {
		t.Fatalf("empty search results = %#v, want non-null empty array", empty.Results)
	}
	human := runMemdolt(t, "search", "Windows onboarding", "--dir", base)
	for _, want := range []string{"Matcher: fts:decision-fallback", "Bundle local SQLite", "Avoid Windows onboarding friction."} {
		if !strings.Contains(human, want) {
			t.Errorf("human search output %q does not contain %q", human, want)
		}
	}

	missing := scratchDir(t)
	for _, args := range [][]string{
		{"search", "", "--dir", missing},
		{"search", "decision: !!!", "--dir", missing},
		{"search", "file:src/main.go", "--dir", missing},
	} {
		if err := runMemdoltErr(t, args...); strings.Contains(err, "memdolt init") {
			t.Errorf("invalid search %q reached store SQL before refusal: %s", args[1], err)
		}
	}
	if err := runMemdoltErr(t, "search", "file:src/main.go", "--dir", missing); !strings.Contains(err, "M5 code-index/git-ingest") {
		t.Fatalf("file-history refusal = %q", err)
	}
}
