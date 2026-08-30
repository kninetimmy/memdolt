package localdolt_test

import (
	"context"
	"path/filepath"
	"testing"

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
