package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// This is an authorized disposable CLI recipe, not live-agent compliance.
// FTS needs no vector rebuild; the existing golden gate covers real inference.
func TestCatchUpFTSRecipePreservesContextAfterReopening(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owner"}[owner], func(t *testing.T) {
			home := scratchDir(t)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("DOLT_REMOTE_PASSWORD", "")
			source := initStore(t)
			remote := remoteFileURL(scratchDir(t))
			runMemdolt(t, "repo", "remote", "add", "upstream", remote, "--dir", source)
			runMemdoltIn(t, "Fixture: waiting for new work", "state", "set", "--actor", "codex", "--dir", source)
			arch := decodeJSON[narrativeInfo](t, runMemdoltIn(t, "Fixture architecture: CLI and local memory",
				"arch", "set", "--actor", "codex", "--dir", source, "--json"))
			first := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "upstream", "--dir", source, "--json"))
			base := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", base)
			runMemdolt(t, "repo", "remote", "add", "upstream", remote, "--dir", base)
			config := "[retrieval]\nmode = 'fts'\n[render]\noutput_dir = '.memdolt/rendered'\n"
			writeTestFile(t, pathsFor(t, base).ConfigFile(), config)
			initialView := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			projectPath := filepath.Join(initialView.OutputDir, "PROJECT.md")
			initialProject := mustPullFile(t, projectPath)

			st := openInitializedStore(t, base)
			_, err := st.ProposeFact(context.Background(), localdolt.Proposal{
				Actor: cliActor, Rationale: "pending local fixture", Target: localdolt.TargetRepo,
			}, localdolt.Fact{Key: "catchup.pending", Value: "UNAPPROVED_CATCHUP_CLAIM"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			pending := runMemdolt(t, "review", "list", "--dir", base, "--json")
			state := decodeJSON[narrativeInfo](t, runMemdoltIn(t, "Fixture: review nebula transfer next",
				"state", "set", "--actor", "codex", "--dir", source, "--json"))
			task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", "Review nebula transfer",
				"--actor", "codex", "--dir", source, "--json"))
			pushed := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "upstream", "--dir", source, "--json"))
			stop := func() {}
			if owner {
				stop = serveTransferProcess(t, base)
			}
			t.Chdir(t.TempDir()) // Every recipe command must honor the absolute target.
			local := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", base, "--json"))
			if local.Status != "offline" || !local.Clean || local.MainCommit != first.MainCommit || local.PendingProposals.Repo != 1 {
				t.Fatalf("local inspection = %+v", local)
			}
			remotes := decodeJSON[remoteListReport](t, runMemdolt(t, "repo", "remote", "list", "--dir", base, "--json"))
			if len(remotes.Remotes) != 2 || remotes.Remotes[1].Name != "upstream" || remotes.Remotes[1].URL != remote {
				t.Fatalf("configured selected remote = %+v", remotes)
			}
			behind := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "upstream", "--diff", "--dir", base, "--json"))
			if behind.Status != "behind" || behind.Remote != "upstream" || behind.MainCommit != first.MainCommit || behind.RemoteCommit != pushed.MainCommit || behind.Diff == nil || len(behind.Diff.Tables) != 2 {
				t.Fatalf("fetched status = %+v", behind)
			}
			pulled := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "upstream", "--dir", base, "--json"))
			if !pulled.Changed || pulled.Remote != "upstream" || pulled.LocalCommit != behind.MainCommit || pulled.MainCommit != behind.RemoteCommit {
				t.Fatalf("authorized pull = %+v", pulled)
			}
			if got := mustPullFile(t, projectPath); string(got) != string(initialProject) {
				t.Fatal("pull refreshed the local view implicitly")
			}
			index := decodeJSON[embedding.StatusReport](t, runMemdolt(t, "index", "status", "--dir", base, "--json"))
			if !index.NeedsRebuild || index.Missing != 1 || index.Current != 0 {
				t.Fatalf("FTS vector refresh remains optional: %+v", index)
			}
			view := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			if view.SourceCommit != pulled.MainCommit || len(view.WrittenFiles) != 2 || len(view.BackupFiles) != 2 || len(view.NoteCommits) != 0 {
				t.Fatalf("explicit render without queued notes = %+v", view)
			}
			stop() // Subsequent direct CLI calls reopen the released replica.
			for kind, want := range map[string]memory.Narrative{"state": state.Narrative, "arch": arch.Narrative} {
				got := decodeJSON[memory.Narrative](t, runMemdolt(t, kind, "show", "--dir", base, "--json"))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("reopened %s = %+v, want %+v", kind, got, want)
				}
				if !strings.Contains(string(mustPullFile(t, projectPath)), want.Body) {
					t.Fatalf("refreshed PROJECT.md lacks %s", kind)
				}
			}
			for _, path := range view.WrittenFiles {
				body := string(mustPullFile(t, path))
				if strings.Contains(body, "UNAPPROVED_CATCHUP_CLAIM") {
					t.Fatalf("render exposed pending memory: %s", path)
				}
			}
			if body := string(mustPullFile(t, filepath.Join(view.OutputDir, "PROJECT_LEDGER.md"))); !strings.Contains(body, task.Title) {
				t.Fatal("refreshed ledger lacks incoming task")
			}
			queue := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", base, "--json"))
			if len(queue.Tasks) != 1 || queue.Tasks[0].ID != task.ID {
				t.Fatalf("reopened queue = %+v", queue)
			}
			if got := runMemdolt(t, "review", "list", "--dir", base, "--json"); got != pending {
				t.Fatal("catch-up changed pending local proposals")
			}
			recalled := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "nebula", "--dir", base, "--json"))
			if recalled.Mode != retrieval.ModeFTS || len(recalled.Results) != 1 || recalled.Results[0].SourceID != task.ID {
				t.Fatalf("FTS without vectors = %+v", recalled)
			}
			current := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "upstream", "--dir", base, "--json"))
			if current.Status != "current" || !current.Clean || current.MainCommit != pulled.MainCommit {
				t.Fatalf("refresh wrote extra memory or changed remote: %+v", current)
			}
			if got := string(mustPullFile(t, pathsFor(t, base).ConfigFile())); got != config {
				t.Fatal("catch-up replaced local configuration")
			}
			if _, err := os.Stat(filepath.Join(home, ".memdolt", "models")); !os.IsNotExist(err) {
				t.Fatalf("FTS recipe provisioned models or model inspection failed: %v", err)
			}
		})
	}
}
