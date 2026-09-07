package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestPullCLIConflictResolutionDirectAndLiveOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			remote := configureCLITransferRemote(t, a)
			task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", "shared task", "--dir", a, "--json"))
			runMemdolt(t, "push", "--dir", a)
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			for i, base := range []string{a, b} {
				db := openRepoFixtureDB(t, base)
				if _, err := db.Exec("UPDATE tasks SET notes=?, updated_at=? WHERE id=?", []string{"theirs", "ours"}[i], []string{"2026-01-03 00:00:00", "2026-01-02 00:00:00"}[i], task.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("CALL DOLT_COMMIT('-A', '-m', 'synthetic CLI conflict')"); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			runMemdolt(t, "push", "--dir", a)
			stop := func() {}
			if routed {
				stop = serveTransferProcess(t, b)
			}
			before := runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json")
			stdout, err := runMemdoltResult(t, "pull", "--dir", b, "--json")
			if err == nil || !strings.Contains(err.Error(), "--resolve") {
				t.Fatalf("noninteractive conflict = %q, %v", stdout, err)
			}
			shown := decodeJSON[localdolt.TransferResult](t, stdout)
			if shown.Status != "conflicted" || len(shown.Conflicts) != 1 || shown.Conflicts[0].Rows[0].TheirsBlame.Message != "synthetic CLI conflict" {
				t.Fatalf("CLI conflict = %+v", shown)
			}
			if runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json") != before {
				t.Fatal("CLI conflict changed local state")
			}
			resolution := localdolt.PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: []localdolt.PullChoice{{Conflict: shown.Conflicts[0].ID, Take: "manual"}}}
			resolution.Choices[0].Row = shown.Conflicts[0].Rows[0].Ours
			manual := "reviewed manual CLI value: café 😀 �"
			resolution.Choices[0].Row["notes"] = &manual
			data, err := json.Marshal(resolution)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(b, "resolution.json")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			malformed := filepath.Join(b, "malformed.json")
			if err := os.WriteFile(malformed, []byte(`{"localCommit":"first","localCommit":"second"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runMemdoltErr(t, "pull", "--dir", b, "--resolve", malformed); !strings.Contains(err, "duplicate") {
				t.Fatal(err)
			}
			for _, invalid := range []string{"bad\xfftext", `bad\ud800text`, `bad\udc00text`, `bad\ud800\u0041text`} {
				bad := bytes.Replace(data, []byte(manual), []byte(invalid), 1)
				if err := os.WriteFile(malformed, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := runMemdoltErr(t, "pull", "--dir", b, "--resolve", malformed); !strings.Contains(err, "UTF-8") && !strings.Contains(err, "surrogate") {
					t.Fatal(err)
				}
				if runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json") != before {
					t.Fatal("malformed CLI Unicode changed main or its working set")
				}
			}
			resolved := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--resolve", file, "--json"))
			if !resolved.Changed || resolved.LocalCommit != shown.LocalCommit || resolved.RemoteCommit != shown.RemoteCommit {
				t.Fatalf("CLI merge = %+v", resolved)
			}
			stop()
			listed := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", b, "--json"))
			if len(listed.Tasks) != 1 || listed.Tasks[0].ID != task.ID || listed.Tasks[0].Notes != manual {
				t.Fatalf("ordinary reopened CLI read = %+v", listed)
			}
			if string(mustPullFile(t, file)) != string(data) {
				t.Fatal("pull modified resolution source file")
			}
			if err := runMemdoltErr(t, "pull", "--dir", b, "--resolve", file); !strings.Contains(err, "changed since") {
				t.Fatal(err)
			}
		})
	}
}

func mustPullFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPullCLIIndependentMergeAndStdinResolutionParsing(t *testing.T) {
	a := initStore(t)
	remote := configureCLITransferRemote(t, a)
	runMemdolt(t, "push", "--dir", a)
	b := scratchDir(t)
	runMemdolt(t, "clone", remote, "--dir", b)
	runMemdolt(t, "task", "add", "local task", "--dir", b)
	runMemdolt(t, "task", "add", "remote task", "--dir", a)
	runMemdolt(t, "push", "--dir", a)
	result := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--json"))
	if !result.Changed || result.MainCommit == result.RemoteCommit || len(decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", b, "--json")).Tasks) != 2 {
		t.Fatalf("independent CLI merge = %+v", result)
	}
	runMemdolt(t, "push", "--dir", b)
	c := scratchDir(t)
	runMemdolt(t, "clone", remote, "--dir", c)
	if len(decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", c, "--json")).Tasks) != 2 {
		t.Fatal("ordinary fresh clone lost merged rows")
	}
	root := newRootCommand()
	root.SetIn(strings.NewReader(`{"localCommit":"not a hash","remoteCommit":"not a hash","choices":[]}`))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"pull", "--dir", b, "--resolve", "-"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "exact displayed") {
		t.Fatalf("stdin malformed hashes = %v", err)
	}
}

func TestPullCLIIntegratesHumanFactAndDecisionWrites(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			remote := configureCLITransferRemote(t, a)
			shared := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "shared.key", "base value", "--dir", a, "--json"))
			runMemdolt(t, "push", "--dir", a)
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			stop := func() {}
			if routed {
				stop = serveTransferProcess(t, b)
			}
			var facts, decisions []localdolt.HumanMemoryResult
			for i, base := range []string{a, b} {
				side := []string{"remote", "local"}[i]
				updated := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "shared.key", side+" value", "--source", "observed", "--kind", "convention", "--evidence", side+".md", "--dir", base, "--json"))
				if updated.ID != shared.ID {
					t.Fatal("human upsert changed the shared fact identity")
				}
				facts = append(facts, decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "collision.key", side+" fact", "--dir", base, "--json")))
				decisions = append(decisions, decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "decision", "add", side+" decision", "--rationale", side+" rationale", "--summary", side+" summary", "--evidence", side+".md", "--dir", base, "--json")))
			}
			runMemdolt(t, "push", "--dir", a)
			before := runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json")
			stdout, err := runMemdoltResult(t, "pull", "--dir", b, "--json")
			if err == nil {
				t.Fatal("human fact conflicts merged without operator choices")
			}
			shown := decodeJSON[localdolt.TransferResult](t, stdout)
			if shown.Status != "conflicted" || len(shown.Conflicts) != 2 || runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json") != before {
				t.Fatalf("human conflict preview changed main or lost conflicts: %+v", shown)
			}
			resolution := localdolt.PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit}
			for _, conflict := range shown.Conflicts {
				choice := localdolt.PullChoice{Conflict: conflict.ID, Take: "ours"}
				if conflict.Kind == "live-fact-key" {
					choice.Take, choice.Winner = "winner", facts[0].ID
				}
				resolution.Choices = append(resolution.Choices, choice)
			}
			data, err := json.Marshal(resolution)
			if err != nil {
				t.Fatal(err)
			}
			merged := decodeJSON[localdolt.TransferResult](t, runMemdoltIn(t, string(data), "pull", "--resolve", "-", "--dir", b, "--json"))
			if !merged.Changed || merged.LocalCommit != shown.LocalCommit || merged.RemoteCommit != shown.RemoteCommit {
				t.Fatalf("human conflict resolution = %+v", merged)
			}
			runMemdolt(t, "fact", "verify", facts[0].ID, "--dir", b)
			runMemdolt(t, "decision", "set-summary", decisions[0].ID, "reviewed remote summary", "--dir", b)
			runMemdolt(t, "decision", "supersede", decisions[1].ID, "--by", decisions[0].ID, "--dir", b)
			stop()
			listed := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", b, "--json"))
			if len(listed.Facts) != 3 {
				t.Fatalf("reopened human fact list lost rows: %+v", listed)
			}
			for _, fact := range listed.Facts {
				switch fact.ID {
				case shared.ID:
					if fact.Value != "local value" || fact.Source != "observed" || fact.Kind != "convention" || fact.Evidence != "local.md" {
						t.Fatalf("chosen human fact fields changed: %+v", fact)
					}
				case facts[0].ID:
					if fact.Value != "remote fact" || fact.SupersededBy != "" || fact.VerifiedAt == nil {
						t.Fatalf("chosen fact winner changed: %+v", fact)
					}
				case facts[1].ID:
					if fact.Value != "local fact" || fact.SupersededBy != facts[0].ID {
						t.Fatalf("fact loser was removed or overwritten: %+v", fact)
					}
				default:
					t.Fatalf("unexpected merged fact: %+v", fact)
				}
			}
			decisionRows := decodeJSON[decisionListReport](t, runMemdolt(t, "decision", "list", "--dir", b, "--json"))
			if len(decisionRows.Decisions) != 2 {
				t.Fatalf("default human list lost superseded decisions: %+v", decisionRows)
			}
			for _, decision := range decisionRows.Decisions {
				if decision.ID == decisions[0].ID && (decision.Status != "active" || decision.Summary != "reviewed remote summary" || decision.Evidence != "remote.md") ||
					decision.ID == decisions[1].ID && (decision.Status != "superseded" || decision.SupersededBy != decisions[0].ID || decision.Summary != "local summary") {
					t.Fatalf("human decision fields changed after pull: %+v", decision)
				}
			}
		})
	}
}
