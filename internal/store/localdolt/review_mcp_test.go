package localdolt_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/mcpserver"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type elicitedReviewBackend struct {
	*localdolt.Store
	baseDir string
}

func (s *elicitedReviewBackend) ReviewAcceptExpected(
	ctx context.Context,
	id, expectedCommit string,
	reviewer store.Actor,
	force bool,
) (localdolt.AcceptResult, error) {
	return reviewgate.AcceptExpected(
		ctx, s.Store, filepath.Join(s.baseDir, ".memdolt", "config.toml"),
		id, expectedCommit, reviewer, force)
}

// A branch reset can leave a different, still-single-commit proposal under the
// same id. These protocol-level regressions prove both MCP review modes carry
// the displayed commit into AcceptProposal's serialized pre-merge check.
func TestElicitedReviewRefusesResetProposalCommit(t *testing.T) {
	for _, mode := range []string{"successive", "batch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			base := filepath.Dir(filepath.Dir(st.DataDir()))
			staged, err := st.ProposeFact(ctx, localdolt.Proposal{
				Rationale: "the displayed proposal must be the one accepted",
				Actor:     stagingActor,
				Target:    localdolt.TargetRepo,
			}, localdolt.Fact{Key: "review.snapshot", Value: "the reviewed value"})
			if err != nil {
				t.Fatal(err)
			}

			server := mcpserver.New("test")
			tools := mcpserver.RegisterTools(server, base, &elicitedReviewBackend{Store: st, baseDir: base})
			clientSide, serverSide := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverSide, nil)
			if err != nil {
				t.Fatal(err)
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "review-reset-test", Version: "1"}, &mcp.ClientOptions{
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
				ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return &mcp.ElicitResult{Action: "cancel"}, nil
				},
			})
			clientSession, err := client.Connect(ctx, clientSide, nil)
			if err != nil {
				t.Fatal(err)
			}

			first, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
				Name: "review_pending", Arguments: map[string]any{"mode": mode},
			})
			if err != nil {
				t.Fatal(err)
			}
			if first.IsError || !first.NeedsInput() || first.RequestState == "" {
				t.Fatalf("review did not display the original proposal: %+v", first)
			}

			resetProposalToDifferentSingleCommit(t, st, staged)
			decision := "approve"
			if mode == "batch" {
				decision = "approve_all"
			}
			result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
				Name: "review_pending", Arguments: map[string]any{"mode": mode},
				RequestState: first.RequestState,
				InputResponses: mcp.InputResponseMap{reviewResponseIDForTest: &mcp.ElicitResult{
					Action: "accept", Content: map[string]any{"decision": decision},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.NeedsInput() {
				t.Fatalf("reset %s review requested more input: %+v", mode, result)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 0 {
				t.Fatalf("reset %s review promoted %d facts", mode, got)
			}
			pending, err := st.PendingProposals(ctx)
			if err != nil || len(pending) != 1 || pending[0].ID != staged.ID || pending[0].Commit == staged.Commit {
				t.Fatalf("reset %s review pending = %+v (err %v)", mode, pending, err)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'review accept %'"); got != 0 {
				t.Fatalf("reset %s review created %d merge commits", mode, got)
			}

			if err := clientSession.Close(); err != nil {
				t.Error(err)
			}
			if err := serverSession.Wait(); err != nil {
				t.Error(err)
			}
			if err := tools.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestExpectedCommitAcceptPreservesExternalMutationAfterValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *localdolt.Store, localdolt.StagedProposal)
	}{
		{name: "reset", mutate: resetProposalToDifferentSingleCommit},
		{name: "amend", mutate: amendProposalCommit},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			staged, err := st.ProposeFact(ctx, localdolt.Proposal{
				Rationale: "merge only the commit the reviewer saw",
				Actor:     stagingActor,
				Target:    localdolt.TargetRepo,
			}, localdolt.Fact{Key: "review.snapshot", Value: "the reviewed value"})
			if err != nil {
				t.Fatal(err)
			}
			mainHead := headCommit(t, st)

			result, err := st.AcceptProposalAfterExpectedCommit(ctx, staged.ID, reviewer, localdolt.AcceptOptions{
				Force: true, ExpectedCommit: staged.Commit,
			}, func() { test.mutate(t, st, staged) })
			if err != nil || result.Commit == "" {
				t.Fatalf("accept result=%+v error=%v, want landed merge with retained changed branch", result, err)
			}
			if parents := parentsOf(t, st, result.Commit); len(parents) != 2 || parents[0] != mainHead || parents[1] != staged.Commit {
				t.Fatalf("merge parents = %v, want [%s %s]", parents, mainHead, staged.Commit)
			}
			if got := scanString(t, st, "SELECT value FROM facts AS OF 'main' WHERE id = ?", staged.RowID); got != "the reviewed value" {
				t.Fatalf("main value = %q, want the displayed commit's value", got)
			}
			pending, pendingErr := st.PendingProposals(ctx)
			if pendingErr != nil || len(pending) != 1 || pending[0].ID != staged.ID || pending[0].Commit == staged.Commit {
				t.Fatalf("changed branch was not retained as the only pending proposal: %+v (err %v)", pending, pendingErr)
			}
		})
	}
}

func TestExpectedCommitAcceptLeavesHiddenCleanupResidue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "retain merged MCP cleanup residue",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{Key: "review.cleanup-residue", Value: "the reviewed value"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := st.AcceptProposal(ctx, staged.ID, reviewer, localdolt.AcceptOptions{
		Force: true, ExpectedCommit: staged.Commit,
	})
	if err != nil || result.Commit == "" {
		t.Fatalf("expected-commit accept result=%+v error=%v", result, err)
	}
	requireProposalBranches(t, st, staged.Branch)
	if pending, pendingErr := st.PendingProposals(ctx); pendingErr != nil || len(pending) != 0 {
		t.Fatalf("merged cleanup residue was offered for review: %+v (err %v)", pending, pendingErr)
	}
}

func TestExpectedCommitAcceptPreservesForeignCommitAtCleanupBoundary(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "never delete unseen foreign branch content",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{Key: "review.cleanup-boundary", Value: "the reviewed value"})
	if err != nil {
		t.Fatal(err)
	}
	const foreignID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	result, err := st.AcceptProposalBeforeCleanup(ctx, staged.ID, reviewer, localdolt.AcceptOptions{
		Force: true, ExpectedCommit: staged.Commit,
	}, func() {
		err := st.RunOnBranch(context.Background(), staged.Branch,
			store.Statement{
				SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
				Args: []any{foreignID, "review.foreign-content", "must survive", stagingActor.Name, "2026-08-30 12:00:00"},
			},
			store.Statement{
				SQL:  "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
				Args: []any{"foreign commit in the former cleanup interval", stagingActor.String()},
			},
		)
		if err != nil {
			t.Fatalf("add foreign branch content at cleanup boundary: %v", err)
		}
	})
	if err != nil || result.Commit == "" {
		t.Fatalf("expected-commit accept result=%+v error=%v", result, err)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts AS OF 'main' WHERE id = ?", foreignID); got != 0 {
		t.Fatalf("foreign branch fact appeared on main %d time(s)", got)
	}
	pending, pendingErr := st.PendingProposals(ctx)
	if pendingErr != nil || len(pending) != 1 || pending[0].ID != staged.ID || pending[0].Commit == staged.Commit {
		t.Fatalf("foreign branch content was not retained for review: %+v (err %v)", pending, pendingErr)
	}
	diff, diffErr := st.ProposalDiff(ctx, staged.ID)
	if diffErr != nil {
		t.Fatal(diffErr)
	}
	var found bool
	for _, change := range diff.Changes {
		if change.Table == "facts" && change.To["id"] == foreignID && change.To["value"] == "must survive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retained branch diff does not contain foreign fact: %+v", diff.Changes)
	}
}

func TestExpectedCommitAcceptSerializesRejectAndExpire(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })

	for _, operation := range []string{"reject", "expire"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			staged, err := st.ProposeFact(ctx, localdolt.Proposal{
				Rationale: "serialize proposal mutation after validation",
				Actor:     stagingActor,
				Target:    localdolt.TargetRepo,
			}, localdolt.Fact{Key: "review." + operation, Value: "the displayed value"})
			if err != nil {
				t.Fatal(err)
			}

			validated := make(chan struct{})
			resume := make(chan struct{})
			type acceptOutcome struct {
				result localdolt.AcceptResult
				err    error
			}
			acceptDone := make(chan acceptOutcome, 1)
			go func() {
				result, acceptErr := st.AcceptProposalAfterExpectedCommit(ctx, staged.ID, reviewer, localdolt.AcceptOptions{
					Force: true, ExpectedCommit: staged.Commit,
				}, func() {
					close(validated)
					<-resume
				})
				acceptDone <- acceptOutcome{result: result, err: acceptErr}
			}()
			<-validated

			started := make(chan struct{})
			mutationDone := make(chan error, 1)
			go func() {
				close(started)
				if operation == "reject" {
					_, mutationErr := st.RejectProposal(ctx, staged.ID)
					mutationDone <- mutationErr
					return
				}
				expired, mutationErr := st.ExpireProposals(ctx, time.Now().Add(time.Hour))
				if mutationErr == nil && len(expired) != 1 {
					mutationErr = errors.New("expiry did not remove the merged cleanup residue after acceptance finished")
				}
				mutationDone <- mutationErr
			}()
			<-started
			select {
			case mutationErr := <-mutationDone:
				t.Fatalf("%s completed inside the acceptance critical section: %v", operation, mutationErr)
			default:
			}
			close(resume)

			accepted := <-acceptDone
			if accepted.err != nil || accepted.result.Commit == "" {
				t.Fatalf("accept outcome = %+v", accepted)
			}
			mutationErr := <-mutationDone
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if parents := parentsOf(t, st, accepted.result.Commit); len(parents) != 2 || parents[1] != staged.Commit {
				t.Fatalf("merge parents = %v, want displayed commit %s second", parents, staged.Commit)
			}
			requireProposalBranches(t, st)
		})
	}
}

func TestAcceptCleanupFailureReturnsLandedResultWithoutReofferingBranch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "preserve durable truth when cleanup fails",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{Key: "review.cleanup", Value: "already merged"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := st.AcceptProposalWithCleanupError(ctx, staged.ID, reviewer, localdolt.AcceptOptions{
		Force: true,
	}, errors.New("injected branch deletion failure"))
	if err == nil || result.Commit == "" || result.Proposal.ID != staged.ID {
		t.Fatalf("accept result=%+v error=%v, want landed result plus cleanup error", result, err)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts AS OF 'main' WHERE id = ?", staged.RowID); got != 1 {
		t.Fatalf("landed cleanup failure left %d durable facts, want 1", got)
	}
	requireProposalBranches(t, st, staged.Branch)
	pending, pendingErr := st.PendingProposals(ctx)
	if pendingErr != nil || len(pending) != 0 {
		t.Fatalf("already-merged leftover branch was re-offered as pending: %+v (err %v)", pending, pendingErr)
	}
	if _, err := st.RejectProposal(ctx, staged.ID); err != nil {
		t.Fatalf("clean up the leftover branch explicitly: %v", err)
	}
	requireProposalBranches(t, st)
}

const reviewResponseIDForTest = "review"

func resetProposalToDifferentSingleCommit(t *testing.T, st *localdolt.Store, staged localdolt.StagedProposal) {
	t.Helper()
	err := st.RunOnBranch(context.Background(), staged.Branch,
		store.Statement{SQL: "CALL DOLT_RESET('--hard', ?)", Args: []any{localdolt.MainBranch}},
		store.Statement{
			SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{staged.RowID, "review.snapshot", "an unreviewed replacement", stagingActor.Name, "2026-08-30 12:00:00"},
		},
		store.Statement{
			SQL:  "INSERT INTO proposals (id, kind, rationale, actor, created_at, target) VALUES (?, ?, ?, ?, ?, ?)",
			Args: []any{staged.ID, string(localdolt.KindFact), "replacement rationale", stagingActor.Name, "2026-08-30 12:00:00", string(localdolt.TargetRepo)},
		},
		store.Statement{
			SQL:  "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
			Args: []any{"replace the proposal after display", stagingActor.String()},
		},
	)
	if err != nil {
		t.Fatalf("reset proposal to a different single commit: %v", err)
	}
	requireOnMain(t, st)
}

func amendProposalCommit(t *testing.T, st *localdolt.Store, staged localdolt.StagedProposal) {
	t.Helper()
	err := st.RunOnBranch(context.Background(), staged.Branch, store.Statement{
		SQL:  "CALL DOLT_COMMIT('--amend', '-m', ?, '--author', ?)",
		Args: []any{"amend the proposal after display", stagingActor.String()},
	})
	if err != nil {
		t.Fatalf("amend proposal after display: %v", err)
	}
	requireOnMain(t, st)
}
