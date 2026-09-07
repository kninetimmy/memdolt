package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func humanInspect(t *testing.T, base string, inspect func(commandStore)) {
	t.Helper()
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	inspect(st)
}

func humanRow(t *testing.T, st store.Store, query string, args []any, dest ...any) {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("missing fixture row: %s (%v)", query, rows.Err())
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestHumanMemoryCLIDirectAndOwnerLifecycle(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			ctx := context.Background()
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			run := func(args ...string) localdolt.HumanMemoryResult {
				t.Helper()
				result := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, append(args, "--dir", base, "--json")...))
				if result.ID == "" || (result.Status != "unchanged" && result.Commit == "") {
					t.Fatalf("missing write evidence: %+v", result)
				}
				return result
			}
			for _, kind := range []string{"fact", "decision"} {
				if out := runMemdolt(t, kind, "list", "--dir", base, "--json"); out != "{\""+kind+"s\":[]}\n" {
					t.Fatalf("empty %s list: %s", kind, out)
				}
			}
			old := run("fact", "add", "build.command", "oldbeacon compile", "--kind", " command ", "--source", "user+agent:codex", "--evidence", "build.go:12")
			if old.Status != "created" {
				t.Fatal(old)
			}
			before := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", base, "--json")).Facts[0]
			updated := run("fact", "add", "build.command", "newbeacon compile", "--source", "observed")
			after := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--prefix", "build.", "--dir", base, "--json")).Facts[0]
			if updated.ID != old.ID || updated.Status != "updated" || before.ID != after.ID || !before.CreatedAt.Equal(after.CreatedAt) || before.Kind != " command " || after.Kind != "" || after.Source != "observed" || after.VerifiedAt == nil || after.Stale {
				t.Fatalf("upsert = before %+v, after %+v, result %+v", before, after, updated)
			}
			humanInspect(t, base, func(st commandStore) {
				var value, author, email, message string
				var kind, evidence sql.NullString
				humanRow(t, st, "SELECT value FROM facts AS OF '"+old.Commit+"' WHERE id = ?", []any{old.ID}, &value)
				if value != before.Value {
					t.Fatal("old fact text missing from Dolt history")
				}
				humanRow(t, st, "SELECT kind, evidence FROM facts WHERE id = ?", []any{old.ID}, &kind, &evidence)
				if kind.Valid || evidence.Valid {
					t.Fatal("omitted kind/evidence were not cleared to NULL")
				}
				humanRow(t, st, "SELECT committer, email, message FROM dolt_log WHERE commit_hash = ?", []any{updated.Commit}, &author, &email, &message)
				if author != "user" || email != "user@memdolt.invalid" || message != "fact add "+old.ID || strings.Contains(message, before.Value) {
					t.Fatalf("history attribution/message = %s %s %s", author, email, message)
				}
				if _, err := st.Commit(ctx, store.CommitRequest{Author: cliActor, Message: "age verification fixture", NoText: true, Statements: []store.Statement{
					{SQL: "UPDATE facts SET verified_at = ? WHERE id = ?", Args: []any{time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), old.ID}},
				}}); err != nil {
					t.Fatal(err)
				}
			})
			verified := run("fact", "verify", "build.command")
			if verified.ID != old.ID || verified.Status != "verified" {
				t.Fatal(verified)
			}
			reverified := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", base, "--json")).Facts[0]
			after.VerifiedAt = reverified.VerifiedAt
			if !reflect.DeepEqual(after, reverified) {
				t.Fatalf("verify changed more than its timestamp: %+v vs %+v", after, reverified)
			}
			replacement := run("fact", "add", "build.replacement", "replacementbeacon")
			superseded := run("fact", "supersede", "build.command", "--by", "build.replacement")
			if superseded.ID != old.ID || superseded.By != replacement.ID || superseded.Status != "superseded" {
				t.Fatal(superseded)
			}
			if result := run("fact", "supersede", old.ID, "--by", replacement.ID); result.Status != "unchanged" || result.Commit != "" {
				t.Fatal(result)
			}
			fresh := run("fact", "add", "build.command", "freshbeacon")
			if fresh.ID == old.ID || fresh.Status != "created" {
				t.Fatal("add resurrected a superseded row")
			}
			for _, args := range [][]string{
				{"fact", "verify", "build.command"}, {"fact", "supersede", "build.command", "--by", replacement.ID},
				{"fact", "supersede", replacement.ID, "--by", old.ID}, {"fact", "supersede", fresh.ID, "--by", fresh.ID},
				{"fact", "verify", "missing"}, {"fact", "supersede", fresh.ID, "--by", "missing"},
			} {
				if out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...); err == nil || out != "" {
					t.Fatalf("invalid choice accepted: %v = %q, %v", args, out, err)
				}
			}
			run("fact", "verify", old.ID)
			facts := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", base, "--json")).Facts
			if len(facts) != 3 || facts[0].SupersededBy != replacement.ID || facts[1].ID != fresh.ID {
				t.Fatalf("superseded/live rows lost or reordered: %+v", facts)
			}
			decision := run("decision", "add", "Use tagged summaries", "--rationale", "current rationale", "--summary", "  preserved summary  ", "--alternatives", "other choices", "--evidence", "PR #139", "--source", "git")
			d := decodeJSON[decisionListReport](t, runMemdolt(t, "decision", "list", "--dir", base, "--json")).Decisions[0]
			if d.Summary != "  preserved summary  " || d.AlternativesRejected != "other choices" || d.Evidence != "PR #139" || d.Source != "git" || d.Status != "active" {
				t.Fatal(d)
			}
			cleared := run("decision", "set-summary", decision.ID, " \t ")
			if cleared.Status != "updated" {
				t.Fatal(cleared)
			}
			if noop := run("decision", "set-summary", decision.ID, ""); noop.Status != "unchanged" || noop.Commit != "" {
				t.Fatal(noop)
			}
			humanInspect(t, base, func(st commandStore) {
				var summary sql.NullString
				humanRow(t, st, "SELECT summary FROM decisions WHERE id = ?", []any{decision.ID}, &summary)
				if summary.Valid {
					t.Fatal("blank summary was not SQL NULL")
				}
			})
			d2 := run("decision", "add", "Replacement choice", "--rationale", "new rationale", "--summary", " \t ")
			demoted := run("decision", "supersede", decision.ID, "--by", d2.ID)
			if demoted.By != d2.ID || demoted.Status != "superseded" {
				t.Fatal(demoted)
			}
			active := decodeJSON[decisionListReport](t, runMemdolt(t, "decision", "list", "--status", "active", "--dir", base, "--json")).Decisions
			all := decodeJSON[decisionListReport](t, runMemdolt(t, "decision", "list", "--dir", base, "--json")).Decisions
			if len(active) != 1 || active[0].ID != d2.ID || len(all) != 2 || all[1].SupersededBy != d2.ID || all[1].Summary != "" || all[1].AlternativesRejected != d.AlternativesRejected {
				t.Fatalf("decision lifecycle: active=%+v all=%+v", active, all)
			}
			for _, args := range [][]string{
				{"decision", "supersede", d2.ID, "--by", strings.ToLower(d2.ID)},
				{"decision", "supersede", d2.ID, "--by", decision.ID}, {"decision", "supersede", d2.ID, "--by", fresh.ID},
				{"decision", "set-summary", fresh.ID, "wrong kind"}, {"fact", "supersede", fresh.ID, "--by", d2.ID},
			} {
				if err := runMemdoltErr(t, append(args, "--dir", base)...); err == "" {
					t.Fatal("invalid same-kind choice accepted")
				}
			}
			rendered := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			ledger, err := os.ReadFile(filepath.Join(rendered.OutputDir, "PROJECT_LEDGER.md"))
			if err != nil || rendered.SourceCommit != demoted.Commit {
				t.Fatalf("render = %+v %v", rendered, err)
			}
			for _, want := range []string{old.ID, fresh.ID, "freshbeacon", "other choices", "PR #139", decision.ID, d2.ID} {
				if !bytes.Contains(ledger, []byte(want)) {
					t.Fatalf("render omits %q", want)
				}
			}
			if bytes.Contains(ledger, []byte("oldbeacon")) || bytes.Contains(ledger, []byte("preserved summary")) {
				t.Fatal("render presented overwritten text as current")
			}
			recalled := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "freshbeacon", "--mode", "fts", "--provenance", "--dir", base, "--json"))
			if len(recalled.Results) != 1 || recalled.Results[0].SourceID != fresh.ID || recalled.Results[0].LastChanged == nil || recalled.Results[0].LastChanged.Author != "user" {
				t.Fatalf("production recall = %+v", recalled)
			}
		})
	}
}

func TestHumanMemoryDirtyProposalAndDenyRefusalsDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			fact := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "safe.key", "committed fact", "--dir", base, "--json"))
			st := openInitializedStore(t, base)
			pending, err := st.ProposeFact(context.Background(), localdolt.Proposal{Rationale: "pending", Actor: cliStagingActor, Target: localdolt.TargetRepo}, localdolt.Fact{Key: "pending.key", Value: "pending hidden"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db := openRepoFixtureDB(t, base)
			if _, err := db.Exec("UPDATE facts SET value = 'dirty hidden' WHERE id = ?", fact.ID); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if routed {
				serveStore(t, base)
			}
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			for _, args := range [][]string{
				{"fact", "add", "new.key", "safe"}, {"fact", "verify", fact.ID}, {"fact", "supersede", fact.ID, "--by", fact.ID},
				{"decision", "add", "new", "--rationale", "safe"}, {"decision", "set-summary", fact.ID, "safe"}, {"decision", "supersede", fact.ID, "--by", fact.ID},
			} {
				if err := runMemdoltErr(t, append(args, "--dir", base)...); !strings.Contains(err, "uncommitted") {
					t.Fatalf("dirty write refusal = %s", err)
				}
			}
			listed := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", base, "--json"))
			if len(listed.Facts) != 1 || listed.Facts[0].Value != "committed fact" {
				t.Fatal("list exposed proposal or dirty text")
			}
			humanInspect(t, base, func(st commandStore) {
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
					t.Fatal("refused operations changed main roots or proposals")
				}
				proposals, err := st.PendingProposals(context.Background())
				if err != nil || len(proposals) != 1 || proposals[0].ID != pending.ID || proposals[0].Commit != pending.Commit {
					t.Fatalf("proposal changed: %+v %v", proposals, err)
				}
			})
		})
	}
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("deny-owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			writeTestFile(t, pathsFor(t, base).ConfigFile(), "[deny_list]\npatterns=['DENIED']\n")
			cases := [][]string{
				{"fact", "add", "safe.key", "DENIED"}, {"fact", "add", "DENIED.key", "safe"},
				{"fact", "add", "safe.key", "safe", "--kind", "DENIED"}, {"fact", "add", "safe.key", "safe", "--evidence", "DENIED"},
				{"decision", "add", "DENIED", "--rationale", "safe"}, {"decision", "add", "safe", "--rationale", "DENIED"},
				{"decision", "add", "safe", "--rationale", "safe", "--summary", "DENIED"},
				{"decision", "add", "safe", "--rationale", "safe", "--alternatives", "DENIED"},
				{"decision", "add", "safe", "--rationale", "safe", "--evidence", "DENIED"},
			}
			for _, args := range cases {
				if out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...); err == nil || out != "" || !strings.Contains(err.Error(), "deny-list") {
					t.Fatalf("deny refusal %v = %q %v", args, out, err)
				}
			}
			for _, config := range []string{"[deny_list]\npatterns=['[']", "[deny_list"} {
				writeTestFile(t, pathsFor(t, base).ConfigFile(), config)
				if out, err := runMemdoltResult(t, "fact", "add", "safe.key", "safe", "--dir", base, "--json"); err == nil || out != "" {
					t.Fatalf("bad config permitted a write: %q %v", out, err)
				}
			}
		})
	}
}

func TestHumanMemoryValidationOwnerAndFinalizationErrors(t *testing.T) {
	base := initStore(t)
	for _, args := range [][]string{
		{"fact", "add", "plain", "value"}, {"fact", "add", "bad..key", "value"}, {"fact", "add", "safe.key", ""},
		{"fact", "add", "safe.key", "value", "--source", "cli:user"}, {"fact", "add", "safe.key", "value", "--actor", "codex"},
		{"fact", "add", "safe.key", "value", "--kind", strings.Repeat("界", 65)},
		{"decision", "add", "choice"}, {"decision", "add", "choice", "--rationale", "reason", "--global"},
		{"fact", "promote", "anything"}, {"decision", "list", "--status", "invalid"},
	} {
		if out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...); err == nil || out != "" {
			t.Fatalf("invalid arguments %v = %q %v", args, out, err)
		}
	}
	missing := t.TempDir()
	if err := runMemdoltErr(t, "fact", "list", "--dir", missing); !strings.Contains(err, "memdolt init") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(missing, ".memdolt")); !os.IsNotExist(err) {
		t.Fatal("list created missing store")
	}
	if err := os.Remove(pathsFor(t, base).LockFile()); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic authentication refusal", http.StatusUnauthorized)
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = endpoint.Close() }()
	for _, args := range [][]string{{"fact", "add", "safe.key", "safe"}, {"decision", "list"}} {
		if err := runMemdoltErr(t, append(args, "--dir", base)...); !strings.Contains(err, "401") {
			t.Fatal(err)
		}
		if _, err := os.Stat(pathsFor(t, base).LockFile()); !os.IsNotExist(err) {
			t.Fatal("owner failure fell back to an embedded open")
		}
	}
	for _, failure := range []string{"close", "human", "json"} {
		t.Run(failure, func(t *testing.T) {
			base := initStore(t)
			fixtureError := errors.New("synthetic human memory finalization failure")
			st := &repoFaultStore{commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base}}
			cmd := &cobra.Command{Use: "add"}
			cmd.SetContext(context.Background())
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			jsonOutput = failure != "human"
			if failure == "close" {
				st.closeErr = fixtureError
			} else {
				cmd.SetOut(repoFailWriter{fixtureError})
			}
			flags := humanMemoryCommand{kind: "fact", operation: "add", source: "user"}
			err := flags.run(cmd, st, memory.UserActor, []string{"safe.key", "confirmed"})
			if !errors.Is(err, fixtureError) || !st.closed || !strings.Contains(err.Error(), "confirmed created") {
				t.Fatalf("late error = %v, closed=%t", err, st.closed)
			}
			if failure == "close" {
				result := decodeJSON[localdolt.HumanMemoryResult](t, out.String())
				if result.Commit == "" || result.ID == "" || result.Error == "" {
					t.Fatal(result)
				}
			}
			listed := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--dir", base, "--json"))
			if len(listed.Facts) != 1 || listed.Facts[0].Value != "confirmed" {
				t.Fatal("late failure lost the confirmed row")
			}
		})
	}
}
