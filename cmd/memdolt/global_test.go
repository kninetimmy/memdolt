package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestGlobalTogglePreflightRefusalPreservesConfig(t *testing.T) {
	for _, operation := range []string{"enable", "disable"} {
		for _, unsafe := range []string{"relative home", "linked replica"} {
			for _, mode := range []string{"human", "json"} {
				t.Run(operation+"/"+unsafe+"/"+mode, func(t *testing.T) {
					home := isolatedGlobalHome(t)
					base := initStore(t)
					initial := "false"
					if operation == "disable" {
						initial = "true"
					}
					path := pathsFor(t, base).ConfigFile()
					before := "[global]\nenabled = " + initial + "\ninclude_docs_in_default = true\n"
					writeTestFile(t, path, before)
					if unsafe == "relative home" {
						t.Setenv("HOME", "relative")
						t.Setenv("USERPROFILE", "relative")
					} else {
						if err := os.Mkdir(filepath.Join(home, ".memdolt"), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(interopTempDir(t), filepath.Join(home, ".memdolt", "global")); err != nil {
							t.Skipf("host cannot create a linked-replica fixture: %v", err)
						}
					}
					args := []string{"global", operation, "--dir", base}
					if mode == "json" {
						args = append(args, "--json")
					}
					out, err := runMemdoltResult(t, args...)
					if err == nil || out != "" {
						t.Fatalf("unsafe preflight did not refuse without a change report: %q, %v", out, err)
					}
					after, err := os.ReadFile(path)
					if err != nil || string(after) != before {
						t.Fatalf("refused toggle changed repository config: %q, %v", after, err)
					}
				})
			}
		}
	}
}

func TestGlobalToggleReportsConfirmedChangeThroughLaterFailures(t *testing.T) {
	for _, operation := range []string{"enable", "disable"} {
		for _, failure := range []string{"setter close", "config reread", "output"} {
			for _, mode := range []string{"human", "json"} {
				t.Run(operation+"/"+failure+"/"+mode, func(t *testing.T) {
					isolatedGlobalHome(t)
					base := initStore(t)
					wanted := operation == "enable"
					if _, err := localdolt.SetGlobalEnabled(base, !wanted); err != nil {
						t.Fatal(err)
					}
					late := errors.New("synthetic failure after real config replacement")
					calls := 0
					cmd := newGlobalCommandWithSetter(func(repo string, enabled bool) (bool, error) {
						calls++
						changed, err := localdolt.SetGlobalEnabled(repo, enabled)
						if err != nil || !changed {
							t.Fatalf("fixture did not perform its real toggle: changed=%t, %v", changed, err)
						}
						cfg, err := localdolt.ReadGlobalConfig(repo)
						if err != nil || cfg.Enabled != wanted {
							t.Fatalf("toggle was not persisted: %+v, %v", cfg, err)
						}
						if failure == "setter close" {
							return changed, late
						}
						if failure == "config reread" {
							path := pathsFor(t, repo).ConfigFile()
							raw, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							// A foreign edit after the confirmed replacement prevents
							// follow-up decoding; it cannot erase the confirmed effect.
							writeTestFile(t, path, string(raw)+"\n[unterminated")
						}
						return changed, nil
					})
					cmd.SetArgs([]string{operation, "--dir", base})
					cmd.SilenceUsage, cmd.SilenceErrors = true, true
					cmd.SetContext(context.Background())
					out := &bytes.Buffer{}
					cmd.SetOut(out)
					cmd.SetErr(&bytes.Buffer{})
					jsonOutput = mode == "json"
					if failure == "output" {
						cmd.SetOut(repoFailWriter{late})
					}
					err := cmd.Execute()
					value := "enabled=false"
					if wanted {
						value = "enabled=true"
					}
					if err == nil || calls != 1 || !strings.Contains(err.Error(), "configuration confirmed "+value) || !strings.Contains(err.Error(), "inspect") || !strings.Contains(err.Error(), base) {
						t.Fatalf("lost confirmed toggle or retried it: calls=%d, %v", calls, err)
					}
					if failure != "config reread" && !errors.Is(err, late) {
						t.Fatalf("underlying late error lost: %v", err)
					}
					if failure != "output" {
						if mode == "json" {
							report := decodeJSON[globalStatusReport](t, out.String())
							if !report.Changed || report.Enabled != wanted || report.Path == "" || !strings.Contains(report.Error, "configuration confirmed "+value) || !strings.Contains(report.Error, "inspect") {
								t.Fatalf("JSON lost confirmed config change: %+v", report)
							}
						} else if !strings.Contains(out.String(), "configuration confirmed "+value) {
							t.Fatalf("human report lost confirmed config change: %q", out.String())
						}
						if failure == "config reread" && !strings.Contains(err.Error(), "parse global configuration") {
							t.Fatalf("fixture did not reach failed follow-up read: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestGlobalUnchangedDocumentReportsConfirmedConfigAfterLateFailure(t *testing.T) {
	for _, failure := range []string{"close", "human output", "json output"} {
		t.Run(failure, func(t *testing.T) {
			_, a := globalFixture(t)
			file := filepath.Join(interopTempDir(t), "shared.md")
			writeTestFile(t, file, "# Shared reference\n")
			runMemdolt(t, "doc", "add", file, "--global", "--dir", a)
			b := initStore(t)
			runMemdolt(t, "global", "enable", "--dir", b)
			global, err := localdolt.OpenGlobal(context.Background(), b)
			if err != nil {
				t.Fatal(err)
			}
			late := errors.New("synthetic failure after config flip")
			st := &repoFaultStore{commandStore: &localCommandStore{Store: global, baseDir: b}}
			cmd := &cobra.Command{Use: "doc"}
			cmd.Flags().Bool("global", true, "test global selection")
			cmd.SetContext(context.Background())
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			jsonOutput = failure != "human output"
			if failure == "close" {
				st.closeErr = late
			} else {
				cmd.SetOut(repoFailWriter{late})
			}
			err = runDoc(cmd, st, "add", file, "", memory.UserActor)
			if !errors.Is(err, late) || !strings.Contains(err.Error(), "configuration confirmed enabled") || !st.closed {
				t.Fatalf("lost config-only progress: %v", err)
			}
			if failure == "close" {
				result := decodeJSON[localdolt.DocResult](t, out.String())
				if result.Commit != "" || result.Status != "unchanged" || !result.EnabledDefaultRecall || result.Error == "" {
					t.Fatal(result)
				}
			}
			cfg, err := localdolt.ReadGlobalConfig(b)
			if err != nil || !cfg.IncludeDocsInDefault {
				t.Fatalf("reported config is not durable: %+v %v", cfg, err)
			}
		})
	}
}

func isolatedGlobalHome(t *testing.T) string {
	t.Helper()
	home := interopTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func globalFixture(t *testing.T) (string, string) {
	t.Helper()
	home := isolatedGlobalHome(t)
	base := initStore(t)
	runMemdolt(t, "global", "enable", "--dir", base)
	runMemdolt(t, "global", "init", "--dir", base)
	return home, base
}

func globalInspect(t *testing.T, base string, inspect func(*localdolt.Store)) {
	t.Helper()
	st, err := localdolt.OpenGlobal(context.Background(), base)
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

func TestGlobalCLIThreeRepositoryIsolationPromotionAndDocuments(t *testing.T) {
	_, a := globalFixture(t)
	b, c := initStore(t), initStore(t)
	for _, base := range []string{b, c} {
		status := decodeJSON[globalStatusReport](t, runMemdolt(t, "global", "status", "--dir", base, "--json"))
		if status.Enabled || status.Replica != nil {
			t.Fatal(status)
		}
		if _, err := runMemdoltResult(t, "fact", "add", "global.disabled", "refuse", "--global", "--dir", base); err == nil {
			t.Fatal("disabled global write succeeded")
		}
	}
	fact := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "shared.compiler", "compilerbeacon go build", "--kind", " command ", "--evidence", "build.md:9", "--source", "user+agent:codex", "--dir", a, "--json"))
	before := interopQueryStrings(t, a, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
	// Capture through the actual authenticated repository owner.
	serveStore(t, a)
	promoted := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "promote", fact.ID, "--global", "--dir", a, "--json"))
	if promoted.ID == fact.ID || promoted.SourceID != fact.ID || promoted.SourceCommit != fact.Commit || promoted.Commit == "" {
		t.Fatalf("promotion evidence=%+v", promoted)
	}
	if after := interopQueryStrings(t, a, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); !reflect.DeepEqual(before, after) {
		t.Fatal("promotion changed repository history")
	}
	globalFacts := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--global", "--dir", a, "--json")).Facts
	if len(globalFacts) != 1 || globalFacts[0].Kind != " command " || globalFacts[0].Evidence != "build.md:9" || globalFacts[0].Source != "user+agent:codex" {
		t.Fatal(globalFacts)
	}
	if _, err := runMemdoltResult(t, "fact", "promote", fact.ID, "--global", "--dir", a); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("existing-key promotion=%v", err)
	}
	updated := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "shared.compiler", "compilerbeacon global update", "--global", "--dir", a, "--json"))
	if updated.ID != promoted.ID || updated.Status != "updated" {
		t.Fatal(updated)
	}
	decision := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "decision", "add", "Share decisions", "--rationale", "Keep full fields", "--summary", "summary", "--alternatives", "manual copies", "--evidence", "ADR:4", "--source", "observed", "--dir", a, "--json"))
	first := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "decision", "promote", decision.ID, "--global", "--dir", a, "--json"))
	second := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "decision", "promote", decision.ID, "--global", "--dir", a, "--json"))
	if len(second.TitleCollisions) != 1 || second.TitleCollisions[0] != first.ID || second.ID == first.ID {
		t.Fatalf("decision title collision dropped data: %+v", second)
	}
	decisions := decodeJSON[decisionListReport](t, runMemdolt(t, "decision", "list", "--global", "--dir", a, "--json")).Decisions
	if len(decisions) != 2 || decisions[0].Summary != "summary" || decisions[0].AlternativesRejected != "manual copies" || decisions[0].Evidence != "ADR:4" || decisions[0].Source != "observed" {
		t.Fatal(decisions)
	}
	for _, args := range [][]string{{"fact", "add", "actor.refused", "value"}, {"decision", "add", "Refused", "--rationale", "agent"}} {
		if _, err := runMemdoltResult(t, append(args, "--global", "--actor", "codex", "--dir", a)...); err == nil {
			t.Fatal("agent global write succeeded")
		}
	}
	file := filepath.Join(interopTempDir(t), "global.md")
	if err := os.WriteFile(file, []byte("# Global documentation\n\ncompilerbeacon shared reference\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--global", "--dir", a, "--json"))
	if !doc.EnabledDefaultRecall || doc.Commit == "" {
		t.Fatal(doc)
	}
	runMemdolt(t, "global", "enable", "--dir", b)
	repeat := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--global", "--dir", b, "--json"))
	if repeat.Status != "unchanged" || repeat.Commit != "" || !repeat.EnabledDefaultRecall {
		t.Fatalf("another repository's existing global doc did not flip this repo: %+v", repeat)
	}
	for _, base := range []string{a, b, c} {
		cfg, err := localdolt.ReadGlobalConfig(base)
		if err != nil || cfg.Enabled != (base != c) || cfg.IncludeDocsInDefault != (base != c) {
			t.Fatalf("per-repository global policy=%+v %v", cfg, err)
		}
		localDocs := decodeJSON[docListReport](t, runMemdolt(t, "doc", "list", "--dir", base, "--json"))
		if len(localDocs.Documents) != 0 {
			t.Fatal("global documents leaked into repository document list")
		}
	}
	shown := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "show", doc.Document.ID, "--global", "--dir", b, "--json"))
	if shown.Document.ContentHash != doc.Document.ContentHash || len(shown.Chunks) != 1 {
		t.Fatal(shown)
	}
	if err := os.WriteFile(file, []byte("# Changed\n\nreplacementbeacon content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", file, "--global", "--dir", b, "--json"))
	if changed.Document.ID != doc.Document.ID || changed.Document.ContentHash == doc.Document.ContentHash || changed.Chunks[0].ID == doc.Chunks[0].ID {
		t.Fatal(changed)
	}
	runMemdolt(t, "doc", "remove", doc.Document.ID, "--global", "--dir", b)
	runMemdolt(t, "global", "disable", "--dir", a)
	if cfg, err := localdolt.ReadGlobalConfig(b); err != nil || !cfg.Enabled {
		t.Fatal("disable affected another repository")
	}
	if facts := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--global", "--dir", b, "--json")); len(facts.Facts) != 1 {
		t.Fatal("disable lost durable global data")
	}
}

func TestGlobalCLITwoReplicasRemoteRoundTripAndLocalArtifacts(t *testing.T) {
	homeA, a := globalFixture(t)
	remote := remoteFileURL(interopTempDir(t))
	runMemdolt(t, "repo", "remote", "add", "origin", remote, "--global", "--dir", a)
	first := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "replica.first", "shared main", "--global", "--dir", a, "--json"))
	runMemdolt(t, "push", "--global", "--dir", a)
	homeB := isolatedGlobalHome(t)
	b := initStore(t)
	runMemdolt(t, "global", "enable", "--dir", b)
	runMemdolt(t, "global", "clone", remote, "--dir", b)
	pathsB, err := localdolt.GlobalPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pathsB.EmbeddingsFile()); !os.IsNotExist(err) {
		t.Fatalf("clone transferred/created derived vectors: %v", err)
	}
	second := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "decision", "add", "Replica B", "--rationale", "independent client", "--global", "--dir", b, "--json"))
	runMemdolt(t, "push", "--global", "--dir", b)
	t.Setenv("HOME", homeA)
	t.Setenv("USERPROFILE", homeA)
	pulled := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--global", "--dir", a, "--json"))
	if !pulled.Changed || pulled.MainCommit != second.Commit || pulled.LocalCommit != first.Commit {
		t.Fatal(pulled)
	}
	status := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--global", "--dir", a, "--json"))
	if status.MainCommit != second.Commit || status.Status != "current" {
		t.Fatal(status)
	}
	globalInspect(t, a, func(st *localdolt.Store) {
		var count int
		humanRow(t, st, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash IN (?, ?)", []any{first.Commit, second.Commit}, &count)
		if count != 2 {
			t.Fatal("reopened global replica lost history")
		}
	})
	t.Setenv("HOME", homeB)
	t.Setenv("USERPROFILE", homeB)
	if facts := decodeJSON[factListReport](t, runMemdolt(t, "fact", "list", "--global", "--dir", b, "--json")); len(facts.Facts) != 1 || facts.Facts[0].ID != first.ID {
		t.Fatal(facts)
	}
}

func TestGlobalCLIDisabledEquivalenceMissingStateAndOwnerContention(t *testing.T) {
	home := isolatedGlobalHome(t)
	a := initStore(t)
	runMemdolt(t, "fact", "add", "scope.query", "scopebeacon retained repository text", "--dir", a)
	before := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "scopebeacon", "--dir", a, "--json"))
	runMemdolt(t, "global", "disable", "--dir", a)
	after := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "scopebeacon", "--dir", a, "--json"))
	before.ElapsedMS, after.ElapsedMS = 0, 0
	if !reflect.DeepEqual(before, after) {
		t.Fatal("disabled global changed repository recall output")
	}
	// Retrieval has always ignored unrelated document policy. The global flag
	// reader must not make a disabled recall newly validate that other table.
	writeTestFile(t, pathsFor(t, a).ConfigFile(), "[doc]\nallowed_dirs=7\nunknown_key=true\n[global]\nenabled=false\n")
	withUnrelatedConfig := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "scopebeacon", "--dir", a, "--json"))
	withUnrelatedConfig.ElapsedMS = 0
	if !reflect.DeepEqual(before, withUnrelatedConfig) {
		t.Fatal("disabled recall changed due to unrelated doc config")
	}
	writeTestFile(t, pathsFor(t, a).ConfigFile(), "[global]\nenabled=false\n")
	runMemdolt(t, "global", "enable", "--dir", a)
	if _, err := runMemdoltResult(t, "recall", "scopebeacon", "--dir", a); err == nil || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("missing global state silently ignored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".memdolt", "global")); !os.IsNotExist(err) {
		t.Fatal("recall implicitly created missing replica")
	}
	runMemdolt(t, "global", "init", "--dir", a)
	b := initStore(t)
	runMemdolt(t, "global", "enable", "--dir", b)
	globalInspect(t, a, func(st *localdolt.Store) {
		for _, args := range [][]string{{"recall", "scopebeacon"}, {"fact", "add", "lock.refused", "value", "--global"}, {"index", "status", "--global"}} {
			if _, err := runMemdoltResult(t, append(args, "--dir", b)...); err == nil || !strings.Contains(err.Error(), "current operation/owner") {
				t.Fatalf("competing replica opened: %v", err)
			}
		}
	})
	paths, err := localdolt.GlobalPaths()
	if err != nil {
		t.Fatal(err)
	}
	serveStore(t, paths.Base())
	if _, err := runMemdoltResult(t, "fact", "add", "owner.refused", "value", "--global", "--dir", b); err == nil {
		t.Fatal("global write routed around global owner")
	}
}

func TestGlobalCLIPromotionNullsEqualIDsAndScopedRecall(t *testing.T) {
	_, base := globalFixture(t)
	fact := decodeJSON[localdolt.HumanMemoryResult](t, runMemdolt(t, "fact", "add", "equal.ids", "scopebeacon local", "--dir", base, "--json"))
	humanInspect(t, base, func(st commandStore) {
		_, err := st.Commit(context.Background(), store.CommitRequest{Author: cliActor, NoText: true, Message: "nullable promotion fixture", Statements: []store.Statement{
			{SQL: "UPDATE facts SET kind = '', evidence = NULL, verified_at = NULL WHERE id = ?", Args: []any{fact.ID}},
		}})
		if err != nil {
			t.Fatal(err)
		}
	})
	runMemdolt(t, "fact", "promote", fact.ID, "--global", "--dir", base)
	globalInspect(t, base, func(st *localdolt.Store) {
		var kind, evidence sql.NullString
		var verified sql.NullTime
		humanRow(t, st, "SELECT kind, evidence, verified_at FROM facts", nil, &kind, &evidence, &verified)
		if !kind.Valid || kind.String != "" || evidence.Valid || verified.Valid {
			t.Fatal("promotion changed SQL NULL/empty distinctions")
		}
		_, err := st.Commit(context.Background(), store.CommitRequest{Author: cliActor, Text: []string{"scopebeacon global"}, Message: "equal identity and non-global corpus fixture", Statements: []store.Statement{
			{SQL: "UPDATE facts SET id = ?, value = 'scopebeacon global'", Args: []any{fact.ID}},
			{SQL: "INSERT INTO tasks(id, title, status) VALUES (?, 'scopebeacon hidden task', 'open')", Args: []any{fact.ID}},
		}})
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, owner := range []bool{false, true} {
		if owner {
			serveStore(t, base)
		}
		result := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "scopebeacon", "--provenance", "--dir", base, "--json"))
		if len(result.Results) != 2 || result.CandidateCount != 2 {
			t.Fatalf("colliding ids/keys dropped or global task admitted: %+v", result)
		}
		for _, hit := range result.Results {
			if hit.SourceID != fact.ID || hit.Scope == "" || hit.SnapshotCommit == "" || hit.LastChanged == nil || hit.SourceType != "fact" {
				t.Fatalf("untruthful scope/commit: %+v", hit)
			}
		}
		if result.Results[0].Scope == result.Results[1].Scope {
			t.Fatal("scope identity collapsed")
		}
	}
	index := decodeJSON[embedding.StatusReport](t, runMemdolt(t, "index", "status", "--global", "--dir", base, "--json"))
	if index.Eligible != 1 || index.Missing != 1 {
		t.Fatalf("global index admitted tasks or hid freshness: %+v", index)
	}
}
