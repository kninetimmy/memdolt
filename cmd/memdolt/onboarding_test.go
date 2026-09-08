package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// Exercise the approved-fixture CLI recipe, not an agent's approval behavior.
// Each CLI helper closes its store; subsequent calls reopen the same replica.
func TestOnboardingNarrativeRecipePreservesMemoryAfterReopening(t *testing.T) {
	for _, host := range []struct{ raw, canonical string }{
		{"Claude Code", "agent:claude-code"}, {"codex", "agent:codex"}, {"opencode", "agent:opencode"},
	} {
		t.Run(host.raw, func(t *testing.T) {
			base := initStore(t)
			t.Chdir(t.TempDir()) // All commands must use the explicit target.
			const state = "Fixture project: starlight\n\nBootstrap is ready for review."
			const arch = "Fixture architecture\n\nCLI → local storage; preserve UTF-8 and paragraphs."
			initial := map[string]narrativeInfo{}
			for kind, body := range map[string]string{"state": state, "arch": arch} {
				if got := runMemdoltErr(t, kind, "show", "--dir", base); !strings.Contains(got, "not found") {
					t.Fatalf("inspect missing %s: %s", kind, got)
				}
				initial[kind] = decodeJSON[narrativeInfo](t, runMemdoltIn(t, body,
					kind, "set", "--actor", host.raw, "--dir", base, "--json"))
			}
			empty := decodeJSON[retrieval.Response](t, runMemdolt(t,
				"recall", "starlight", "--mode", "fts", "--dir", base, "--json"))
			if len(empty.Results) != 0 || empty.ReturnedCount != 0 {
				t.Fatalf("narrative-only memory unexpectedly recalled: %+v", empty)
			}
			firstRender := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			if len(firstRender.WrittenFiles) != 2 || firstRender.SourceCommit == "" {
				t.Fatalf("bootstrap render = %+v", firstRender)
			}

			// Existing unrelated memory and a pending claim must survive updates.
			runMemdolt(t, "task", "add", "Keep unrelated work", "--actor", host.raw, "--dir", base)
			runMemdolt(t, "note", "add", "Keep unrelated evidence", "--actor", host.raw, "--dir", base)
			actor, err := memory.NormalizeActor(host.raw)
			if err != nil {
				t.Fatal(err)
			}
			st := openInitializedStore(t, base)
			_, err = st.ProposeFact(context.Background(), localdolt.Proposal{
				Actor: actor.CommitAuthor(), Rationale: "pending fixture", Target: localdolt.TargetRepo,
			}, localdolt.Fact{Key: "fixture.pending", Value: "UNAPPROVED_ONBOARDING_CLAIM"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			tasks := runMemdolt(t, "task", "list", "--status", memory.StatusAny, "--dir", base, "--json")
			notes := runMemdolt(t, "note", "list", "--dir", base, "--json")
			const proposalQuery = "SELECT CONCAT(name, ':', hash) FROM dolt_branches WHERE name LIKE 'proposal/%' ORDER BY name"
			proposals := queryStrings(t, base, proposalQuery)
			if len(proposals) != 1 {
				t.Fatalf("fixture proposal refs = %q", proposals)
			}
			before := storeCommitCount(t, base)
			updated := map[string]narrativeInfo{}
			for _, kind := range []string{"state", "arch"} {
				inspected := decodeJSON[memory.Narrative](t, runMemdolt(t, kind, "show", "--dir", base, "--json"))
				if !reflect.DeepEqual(inspected, initial[kind].Narrative) {
					t.Fatalf("reopened %s differs from approved bootstrap: %+v", kind, inspected)
				}
				body := inspected.Body + "\n\nApproved fixture update; literal $(text) stays text."
				updated[kind] = decodeJSON[narrativeInfo](t, runMemdoltIn(t, body,
					kind, "set", "--actor", host.raw, "--dir", base, "--json"))
				got := decodeJSON[memory.Narrative](t, runMemdolt(t, kind, "show", "--dir", base, "--json"))
				if got.Body != body || got.Actor != host.canonical || got.ActorRaw != host.raw ||
					got.ID == initial[kind].ID || !reflect.DeepEqual(got, updated[kind].Narrative) {
					t.Fatalf("reopened approved %s update = %+v", kind, got)
				}
			}
			result := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			if result.SourceCommit != updated["arch"].Commit || len(result.WrittenFiles) != 2 || len(result.BackupFiles) != 2 {
				t.Fatalf("updated render = %+v", result)
			}
			project, err := os.ReadFile(filepath.Join(result.OutputDir, "PROJECT.md"))
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{updated["state"].Body, updated["arch"].Body, "by " + host.canonical} {
				if !strings.Contains(string(project), text) {
					t.Errorf("rendered project lacks approved text/actor %q", text)
				}
			}
			for _, path := range result.WrittenFiles {
				body, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(body), "UNAPPROVED_ONBOARDING_CLAIM") {
					t.Fatalf("render exposed pending claim or cannot be read: %s, %v", path, err)
				}
			}
			if got := runMemdolt(t, "task", "list", "--status", memory.StatusAny, "--dir", base, "--json"); got != tasks {
				t.Fatal("narrative update changed unrelated tasks")
			}
			if got := runMemdolt(t, "note", "list", "--dir", base, "--json"); got != notes {
				t.Fatal("narrative update changed unrelated notes")
			}
			if got := queryStrings(t, base, proposalQuery); !reflect.DeepEqual(got, proposals) {
				t.Fatalf("narrative update changed proposal refs: %q -> %q", proposals, got)
			}
			log := commitLog(t, base)
			for _, writes := range []map[string]narrativeInfo{initial, updated} {
				for kind, write := range writes {
					if entry := log[write.Commit]; entry.author != host.canonical || entry.message != kind+" set" {
						t.Errorf("%s commit %s attribution = %+v", kind, write.Commit, entry)
					}
				}
			}
			// An unchanged/rejected narrative means no set call, even on rerender.
			runMemdolt(t, "render", "--dir", base, "--json")
			if got := storeCommitCount(t, base); got != before+2 {
				t.Fatalf("two approved updates plus renders left %d commits, want %d", got, before+2)
			}
			for kind, query := range map[string]string{
				"state": "SELECT body FROM project_state WHERE id = ?",
				"arch":  "SELECT body FROM project_arch WHERE id = ?",
			} {
				if got := queryStrings(t, base, query, initial[kind].ID); !reflect.DeepEqual(got, []string{initial[kind].Body}) {
					t.Fatalf("approved update removed earlier %s version: %q", kind, got)
				}
			}
		})
	}
}
