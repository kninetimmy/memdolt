package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestHistoryCLIDirectAndLiveOwnerPreserveNativeState(t *testing.T) {
	ctx := context.Background()
	base := initStore(t)
	st := openInitializedStore(t, base)
	added, err := st.Commit(ctx, store.CommitRequest{Author: cliActor, Message: "CLI history fixture", NoText: true, Statements: []store.Statement{
		{SQL: "INSERT INTO facts (id, `key`, value, source) VALUES ('f', 'history.cli', NULL, '')"},
		{SQL: "INSERT INTO decisions (id, title, rationale, status) VALUES ('d', 'CLI decision', NULL, NULL)"},
		{SQL: "INSERT INTO project_state (id, body, actor_raw) VALUES ('s', NULL, '')"},
		{SQL: "INSERT INTO project_arch (id, body, created_at) VALUES ('a', 'old imported architecture', '2000-01-01 00:00:00')"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 26; i++ {
		if _, _, err := memory.New(st, memory.UserActor).SetNarrative(ctx, memory.StateNarrative, fmt.Sprintf("state %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := st.ProposeFact(ctx, localdolt.Proposal{Rationale: "history CLI exclusion", Actor: cliActor, Target: localdolt.TargetRepo}, localdolt.Fact{Key: "history.pending", Value: "UNACCEPTED"})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for _, subject := range []string{"fact", "decision", "state", "arch"} {
		opts := localdolt.HistoryOptions{Subject: subject, Limit: 25}
		if subject == "fact" || subject == "decision" {
			id := subject[:1]
			opts.ID = &id
		}
		for _, past := range []bool{false, true} {
			if past {
				opts.AsOf = &added.Hash
			}
			result, err := st.History(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			expected[fmt.Sprintf("%s/%t", subject, past)] = string(encoded)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRepoFixtureDB(t, base)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE facts SET value = 'DIRTY'", "UPDATE decisions SET rationale = 'DIRTY'",
		"UPDATE project_state SET body = 'STAGED'", "CALL DOLT_ADD('project_state')",
		"ALTER TABLE project_arch ADD COLUMN dirty_only TEXT", "UPDATE project_arch SET body = 'DIRTY'",
	} {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Real local artifacts must survive both direct reopen and live-owner reads.
	artifacts := map[string]string{
		pathsFor(t, base).ConfigFile():                              "[retrieval]\nmode = 'fts'\n",
		filepath.Join(base, ".memdolt", "history-derived-sentinel"): "derived artifact control",
	}
	for path, body := range artifacts {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []bool{false, true} {
		if owner {
			serveStore(t, base)
		}
		var before [][]string
		humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
		for _, subject := range []string{"fact", "decision", "state", "arch"} {
			args := []string{"history", subject, "--dir", base}
			if subject == "fact" || subject == "decision" {
				args = append(args, subject[:1])
			}
			for _, past := range []bool{false, true} {
				selected := append([]string{}, args...)
				if past {
					selected = append(selected, "--as-of", added.Hash)
				}
				got := decodeJSON[localdolt.HistoryResult](t, runMemdolt(t, append(selected, "--json")...))
				encoded, err := json.Marshal(got)
				if err != nil || string(encoded) != expected[fmt.Sprintf("%s/%t", subject, past)] {
					t.Fatalf("CLI owner=%t subject=%s past=%t = %s (%v)", owner, subject, past, encoded, err)
				}
				if subject == "state" && !past && len(got.Changes) != 25 {
					t.Fatal("default CLI limit is not 25")
				}
				human := runMemdolt(t, selected...)
				if !strings.Contains(human, "captured main: "+got.MainCommit) || !strings.Contains(human, "selected revision: "+got.Revision) || !strings.Contains(human, "native blame:") || strings.Contains(human, "DIRTY") || strings.Contains(human, "STAGED") || strings.Contains(human, "UNACCEPTED") {
					t.Fatal(human)
				}
			}
			limited := decodeJSON[localdolt.HistoryResult](t, runMemdolt(t, append(args, "--limit", "1", "--json")...))
			if len(limited.Changes) != 1 {
				t.Fatal("CLI limit ignored")
			}
		}
		for _, selection := range []string{pending.Commit, "main", strings.Repeat("0", 32)} {
			if output, err := runMemdoltResult(t, "history", "state", "--as-of", selection, "--dir", base, "--json"); err == nil || output != "" {
				t.Fatalf("invalid as-of exposed output: %s, %v", output, err)
			}
		}
		missing := runMemdolt(t, "history", "decision", "absent", "--dir", base)
		if !strings.Contains(missing, "current: null") || !strings.Contains(missing, "native blame: null") || !strings.Contains(missing, "no changes") {
			t.Fatal(missing)
		}
		humanInspect(t, base, func(st commandStore) {
			if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
				t.Fatal("CLI history changed native branches/roots")
			}
		})
		for path, want := range artifacts {
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("history changed artifact %s: %v", path, err)
			}
		}
	}
}

func TestHistoryCLIMissingStoreAndInvalidSelectionsCreateNothing(t *testing.T) {
	for _, args := range [][]string{
		{"history", "state"}, {"history", "arch"}, {"history", "fact", "missing"}, {"history", "decision", "missing"},
		{"history", "fact"}, {"history", "state", "id"}, {"history", "task", "id"},
		{"history", "state", "--limit", "0"}, {"history", "state", "--limit", "200001"},
		{"history", "arch", "--limit", "-1"}, {"history", "arch", "--as-of", ""},
	} {
		base := t.TempDir()
		out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...)
		if err == nil || out != "" {
			t.Fatalf("invalid/missing history = %s %v", out, err)
		}
		entries, err := os.ReadDir(base)
		if err != nil || len(entries) != 0 {
			t.Fatalf("history created missing memory: %v, %v", entries, err)
		}
	}
}
