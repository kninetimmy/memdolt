package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func stageGlobalCLI(t *testing.T, base string, decision bool) localdolt.StagedProposal {
	t.Helper()
	st := openInitializedStore(t, base)
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	p := localdolt.Proposal{Rationale: "a human reviews this shared claim", Actor: cliStagingActor, Target: localdolt.TargetGlobal}
	var result localdolt.StagedProposal
	var err error
	if decision {
		result, err = st.ProposeDecision(context.Background(), p, localdolt.Decision{Title: "Keep shared tools local", Rationale: "portable and durable", Evidence: "fixture.md:9"})
	} else {
		result, err = st.ProposeFact(context.Background(), p, localdolt.Fact{Key: "build.global", Value: "Run go build ./... to compile the application", Evidence: "fixture.md:7"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGlobalReviewCLIReopensDirectAndLiveOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(map[bool]string{false: "direct", true: "owner"}[owner]+"/"+map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
				_, base := globalFixture(t)
				staged := stageGlobalCLI(t, base, jsonMode)
				before := interopQueryStrings(t, base, "SELECT hash FROM dolt_branches WHERE name = 'main'")[0]
				stop := func() {}
				if owner {
					stop = serveTransferProcess(t, base)
				}
				args := []string{"review", "accept", staged.ID, "--dir", base, "--force"}
				if jsonMode {
					args = append(args, "--json")
				}
				out, err := interopProcess(t, args...)
				if err != nil {
					t.Fatalf("fresh terminal accept: %s %v", out, err)
				}
				var accepted localdolt.AcceptResult
				if jsonMode {
					accepted = decodeJSON[localdolt.AcceptResult](t, out)
					if accepted.Commit == "" || accepted.Proposal.Commit != staged.Commit || !accepted.SourceRetained || accepted.AlreadyAccepted {
						t.Fatal(accepted)
					}
				} else if !strings.Contains(out, "global staging") || !strings.Contains(out, staged.Commit) || !strings.Contains(out, "retained") {
					t.Fatal(out)
				}
				stop()
				// A separate owning process must rediscover acceptance from native
				// history; neither warmed node caches nor process memory may help.
				if owner {
					stop = serveTransferProcess(t, base)
				}
				out, err = interopProcess(t, "review", "accept", staged.ID, "--force", "--dir", base, "--json")
				if err != nil {
					t.Fatalf("fresh repeat: %s %v", out, err)
				}
				repeated := decodeJSON[localdolt.AcceptResult](t, out)
				if !repeated.AlreadyAccepted || repeated.Commit == "" || repeated.Proposal.Commit != staged.Commit || jsonMode && repeated.Commit != accepted.Commit {
					t.Fatal(repeated)
				}
				stop()
				if after := interopQueryStrings(t, base, "SELECT hash FROM dolt_branches WHERE name = 'main'")[0]; after != before {
					t.Fatal("source main moved")
				}
				globalInspect(t, base, func(st *localdolt.Store) {
					if queryString(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = ?", "review accept "+string(staged.Kind)+" "+staged.ID) != "1" {
						t.Fatal("duplicate native acceptance")
					}
					if queryString(t, st, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index = 1", repeated.Commit) != repeated.GlobalStageCommit {
						t.Fatal("missing native staging parent")
					}
				})
			})
		}
	}
}

type globalReviewCloseStore struct{ commandStore }

func (s globalReviewCloseStore) Close() error {
	return errors.Join(s.commandStore.Close(), errConfirmedCLI)
}

func TestGlobalReviewCLIPreservesCloseAndOutputFailures(t *testing.T) {
	for _, failure := range []string{"close", "output"} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(failure+"/"+map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
				_, base := globalFixture(t)
				staged := stageGlobalCLI(t, base, false)
				cmd := newReviewAcceptCommandWithRun(func(flags *storeFlags, cmd *cobra.Command, fn func(context.Context, commandStore, memory.Actor) error) error {
					st, err := openCommandStore(cmd.Context(), flags.dir, memory.UserActor.CommitAuthor())
					if err != nil {
						return err
					}
					if failure == "close" {
						st = globalReviewCloseStore{st}
					}
					return errors.Join(fn(cmd.Context(), st, memory.UserActor), st.Close())
				})
				cmd.SetArgs([]string{staged.ID, "--dir", base, "--force"})
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
				var output bytes.Buffer
				cmd.SetOut(&output)
				if failure == "output" {
					cmd.SetOut(repoFailWriter{errConfirmedCLI})
				}
				previous := jsonOutput
				jsonOutput = jsonMode
				defer func() { jsonOutput = previous }()
				err := cmd.ExecuteContext(context.Background())
				if err == nil || !strings.Contains(err.Error(), staged.Commit) || !strings.Contains(err.Error(), "inspect") {
					t.Fatalf("lost source/progress: %s %v", &output, err)
				}
				globalInspect(t, base, func(st *localdolt.Store) {
					head := queryString(t, st, "SELECT DOLT_HASHOF('main')")
					if !strings.Contains(err.Error(), head) || queryString(t, st, "SELECT COUNT(*) FROM facts") != "1" {
						t.Fatalf("lost confirmed merge: %s %v", head, err)
					}
				})
				if failure == "close" && jsonMode {
					result := decodeJSON[struct {
						localdolt.AcceptResult
						Error string `json:"error"`
					}](t, output.String())
					if result.Commit == "" || result.Error == "" || result.Unknown {
						t.Fatal(result)
					}
				}
			})
		}
	}
}
