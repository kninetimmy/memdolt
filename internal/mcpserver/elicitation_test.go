package mcpserver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/memory"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type elicitationBackend struct {
	*localdolt.Store
	baseDir string
}

func (s *elicitationBackend) ReviewAccept(ctx context.Context, id string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
	paths, err := layout.New(s.baseDir)
	if err != nil {
		return localdolt.AcceptResult{}, err
	}
	return reviewgate.Accept(ctx, s.Store, paths.ConfigFile(), id, reviewer, force)
}

func initializedElicitationBackend(t *testing.T) (string, *elicitationBackend) {
	t.Helper()
	base, st := initializedToolStore(t)
	return base, testElicitationBackend(base, st)
}

func testElicitationBackend(base string, st *localdolt.Store) *elicitationBackend {
	return &elicitationBackend{Store: st, baseDir: base}
}

func TestReviewPendingBatchWorksThroughModernAndLegacyElicitation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "modern"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			base, st := initializedElicitationBackend(t)
			fact := stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.batch.fact", "batch fact")
			decision := stageReviewDecision(t, st.Store, localdolt.TargetRepo, "Batch decision")
			global := stageReviewDecision(t, st.Store, localdolt.TargetGlobal, "Global remains terminal-only")

			server := New("test")
			tools := RegisterTools(server, base, st)
			var shown string
			options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				shown = req.Params.Message
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "approve_all"}}, nil
			}}
			client, serverSession := connectWithOptions(t, server,
				&mcp.Implementation{Name: "Claude Code", Version: "1"}, legacy, options)

			output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{"mode": "batch"})
			if output.Status != "complete" || len(output.Accepted) != 2 || output.RepoPending != 0 {
				t.Fatalf("batch review output = %+v", output)
			}
			if output.GlobalPending != 1 || output.Remedy != "1 global proposals pending — run `memdolt review` in a terminal." {
				t.Fatalf("global terminal report = %+v", output)
			}
			if !strings.Contains(shown, fact.ID) || !strings.Contains(shown, decision.ID) || strings.Contains(shown, global.ID) {
				t.Fatalf("batch elicitation showed the wrong proposal set: %s", shown)
			}
			if got := testCount(t, st.Store, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 1 {
				t.Fatalf("durable facts after batch = %d, want 1", got)
			}
			if got := testCount(t, st.Store, "SELECT COUNT(*) FROM decisions AS OF 'main'"); got != 1 {
				t.Fatalf("durable decisions after batch = %d, want 1", got)
			}
			if got := testCount(t, st.Store, "SELECT COUNT(*) FROM dolt_log WHERE committer = 'user' AND message LIKE 'review accept %'"); got != 2 {
				t.Fatalf("human-authored review commits = %d, want 2", got)
			}

			closeSessions(t, client, serverSession)
			if err := tools.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewPendingSuccessiveApprovesAndSkips(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	first := stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.successive.fact", "approve me")
	second := stageReviewDecision(t, st.Store, localdolt.TargetRepo, "Skip me")

	server := New("test")
	tools := RegisterTools(server, base, st)
	var rounds int
	options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		rounds++
		if !strings.Contains(req.Params.Message, []string{first.ID, second.ID}[rounds-1]) {
			t.Fatalf("round %d showed unexpected proposal: %s", rounds, req.Params.Message)
		}
		decision := []string{"approve", "skip"}[rounds-1]
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": decision}}, nil
	}}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if rounds != 2 || output.Status != "complete" || len(output.Accepted) != 1 ||
		len(output.Skipped) != 1 || output.Skipped[0] != second.ID || output.RepoPending != 1 {
		t.Fatalf("successive review output = %+v after %d rounds", output, rounds)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingWithoutElicitationKeepsTheCLIPath(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.cli.fact", "review in a terminal")
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server,
		&mcp.Implementation{Name: "no-elicitation-client", Version: "1"}, false)

	callError(t, client, "review_pending", map[string]any{}, "memdolt review")
	assertOnePendingNoDurableFacts(t, st.Store)

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingGlobalOnlyReportsTerminalRemedyWithoutElicitation(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewDecision(t, st.Store, localdolt.TargetGlobal, "Global stays CLI-only")
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server,
		&mcp.Implementation{Name: "no-elicitation-client", Version: "1"}, false)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if output.Status != "empty" || output.GlobalPending != 1 ||
		output.Remedy != "1 global proposals pending — run `memdolt review` in a terminal." {
		t.Fatalf("global-only review output = %+v", output)
	}
	if pending, err := st.PendingProposals(context.Background()); err != nil || len(pending) != 1 {
		t.Fatalf("global proposal changed: %+v (err %v)", pending, err)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingStateIsSingleUseBoundAndFailClosed(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.state.fact", "state-bound fact")
	server := New("test")
	tools := RegisterTools(server, base, st)
	options := &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		},
	}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Claude Code", Version: "1"}, false, options)

	approval := mcp.InputResponseMap{reviewResponseID: &mcp.ElicitResult{
		Action: "accept", Content: map[string]any{"decision": "approve"},
	}}
	forged := callManualReview(t, client, "forged", approval, "successive")
	if !forged.IsError {
		t.Fatal("forged requestState approved a proposal")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	missingResponseState := startManualReview(t, client)
	bound, err := tools.elicit.peek(context.Background(), missingResponseState)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Repository != st.DataDir() || bound.Actor.Name != "agent:claude-code" ||
		len(bound.ProposalIDs) != 1 || bound.Position != 0 || bound.Action != actionReviewOne {
		t.Fatalf("pending requestState row is not exactly bound: %+v", bound)
	}
	missing := callManualReview(t, client, missingResponseState, nil, "successive")
	if !missing.IsError {
		t.Fatal("missing elicitation response approved a proposal")
	}
	if replay := callManualReview(t, client, missingResponseState, approval, "successive"); !replay.IsError {
		t.Fatal("requestState survived a missing response")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	mismatchState := startManualReview(t, client)
	mismatch := callManualReview(t, client, mismatchState, mcp.InputResponseMap{reviewResponseID: &mcp.ElicitResult{
		Action: "accept", Content: map[string]any{"decision": "approve_all"},
	}}, "batch")
	if !mismatch.IsError {
		t.Fatal("requestState accepted a mismatched action")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	incompleteState := startManualReview(t, client)
	incomplete := callManualReview(t, client, incompleteState, mcp.InputResponseMap{reviewResponseID: &mcp.ElicitResult{
		Action: "accept", Content: map[string]any{},
	}}, "successive")
	if !incomplete.IsError {
		t.Fatal("incomplete elicitation response approved a proposal")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	expiredState := startManualReview(t, client)
	if err := tools.elicit.expire(context.Background(), expiredState); err != nil {
		t.Fatal(err)
	}
	if result := callManualReview(t, client, expiredState, approval, "successive"); !result.IsError {
		t.Fatal("expired requestState approved a proposal")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	declinedState := startManualReview(t, client)
	declined := callManualReview(t, client, declinedState, mcp.InputResponseMap{
		reviewResponseID: &mcp.ElicitResult{Action: "decline"},
	}, "successive")
	if declined.IsError {
		t.Fatalf("declined review should report a terminal result: %+v", declined.Content)
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	validState := startManualReview(t, client)
	accepted := callManualReview(t, client, validState, approval, "successive")
	if accepted.IsError || accepted.NeedsInput() {
		t.Fatalf("valid review did not complete: %+v", accepted.Content)
	}
	if got := testCount(t, st.Store, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 1 {
		t.Fatalf("valid approval left %d durable facts, want 1", got)
	}
	commits := testCount(t, st.Store, "SELECT COUNT(*) FROM dolt_log")
	if replay := callManualReview(t, client, validState, approval, "successive"); !replay.IsError {
		t.Fatal("used requestState was replayable")
	}
	if got := testCount(t, st.Store, "SELECT COUNT(*) FROM dolt_log"); got != commits {
		t.Fatalf("replay changed main history from %d to %d commits", commits, got)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingStillUsesAcceptTimeGuards(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.denied.fact", "becomes DENIED before accept")
	paths, err := layout.New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("[deny_list]\npatterns = ['DENIED']\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := New("test")
	tools := RegisterTools(server, base, st)
	options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "approve_all"}}, nil
	}}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{"mode": "batch"})
	if output.Status != "blocked" || len(output.Failures) != 1 || !strings.Contains(output.Failures[0].Error, "deny-list") {
		t.Fatalf("guarded review output = %+v", output)
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingStateStorageFailureLeavesProposalPending(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.storage.fact", "storage must fail closed")
	server := New("test")
	tools := RegisterTools(server, base, st)
	options := &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		},
	}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Claude Code", Version: "1"}, false, options)
	state := startManualReview(t, client)
	if err := tools.elicit.Close(); err != nil {
		t.Fatal(err)
	}
	result := callManualReview(t, client, state, mcp.InputResponseMap{
		reviewResponseID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "approve"}},
	}, "successive")
	if !result.IsError {
		t.Fatal("closed requestState storage approved a proposal")
	}
	assertOnePendingNoDurableFacts(t, st.Store)

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProposeFactConflictChoicesStageOnlyReviewedProposals(t *testing.T) {
	tests := []struct {
		name       string
		legacy     bool
		response   map[string]any
		wantKind   localdolt.ProposalKind
		wantType   string
		wantKey    string
		wantCancel bool
	}{
		{name: "overwrite", response: map[string]any{"action": "overwrite"}, wantKind: localdolt.KindFact, wantType: "modified", wantKey: "build.command"},
		{name: "supersede legacy", legacy: true, response: map[string]any{"action": "supersede"}, wantKind: localdolt.KindSupersede, wantType: "modified", wantKey: "build.command"},
		{name: "keep both", response: map[string]any{"action": "keep_both", "new_key": "build.race_command"}, wantKind: localdolt.KindFact, wantType: "added", wantKey: "build.race_command"},
		{name: "cancel", response: map[string]any{"action": "cancel"}, wantCancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, st := initializedToolStore(t)
			currentID := seedDurableFact(t, st, "build.command", "go test ./...")
			server := New("test")
			tools := RegisterTools(server, base, testElicitationBackend(base, st))
			var shown string
			options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				shown = req.Params.Message
				return &mcp.ElicitResult{Action: "accept", Content: test.response}, nil
			}}
			client, serverSession := connectWithOptions(t, server,
				&mcp.Implementation{Name: "Claude Code", Version: "1"}, test.legacy, options)

			output := callAs[proposeFactOutput](t, client, "propose_fact", map[string]any{
				"key": "build.command", "value": "go test -race ./...", "rationale": "the required race lane changed",
			})
			if !strings.Contains(shown, currentID) || !strings.Contains(shown, "go test ./...") {
				t.Fatalf("conflict elicitation omitted the current fact: %s", shown)
			}
			if got := testText(t, st, "SELECT value FROM facts AS OF 'main' WHERE id = ?", currentID); got != "go test ./..." {
				t.Fatalf("conflict choice changed main to %q before review", got)
			}
			pending, err := st.PendingProposals(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCancel {
				if !output.Canceled || output.ID != "" || len(pending) != 0 {
					t.Fatalf("cancel output=%+v pending=%+v", output, pending)
				}
			} else {
				if output.ID == "" || output.Resolution != test.response["action"] || len(pending) != 1 || pending[0].Kind != test.wantKind {
					t.Fatalf("staged output=%+v pending=%+v", output, pending)
				}
				diff, err := st.ProposalDiff(context.Background(), output.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !hasFactChange(diff, test.wantType, test.wantKey) {
					t.Fatalf("proposal diff = %+v, want %s fact %s", diff, test.wantType, test.wantKey)
				}
			}

			closeSessions(t, client, serverSession)
			if err := tools.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProposeFactConflictDeclineAndInvalidKeepBothWriteNothing(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *mcp.ElicitResult
		wantErr  bool
	}{
		{name: "decline", response: &mcp.ElicitResult{Action: "decline"}},
		{name: "missing distinct dotted key", response: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"action": "keep_both", "new_key": "build.command"}}, wantErr: true},
		{name: "incomplete", response: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"action": "keep_both"}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, st := initializedToolStore(t)
			seedDurableFact(t, st, "build.command", "go test ./...")
			server := New("test")
			tools := RegisterTools(server, base, testElicitationBackend(base, st))
			options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				return test.response, nil
			}}
			client, serverSession := connectWithOptions(t, server,
				&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "propose_fact", Arguments: map[string]any{
				"key": "build.command", "value": "go test -race ./...", "rationale": "collision response safety",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError != test.wantErr {
				t.Fatalf("result error=%v content=%+v, want error=%v", result.IsError, result.Content, test.wantErr)
			}
			if pending, err := st.PendingProposals(context.Background()); err != nil || len(pending) != 0 {
				t.Fatalf("invalid/declined response left proposals %+v (err %v)", pending, err)
			}
			if got := testCount(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 1 {
				t.Fatalf("invalid/declined response left %d durable facts, want 1", got)
			}
			closeSessions(t, client, serverSession)
			if err := tools.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func startManualReview(t *testing.T, client *mcp.ClientSession) string {
	t.Helper()
	result := callManualReview(t, client, "", nil, "successive")
	if result.IsError || !result.NeedsInput() || result.RequestState == "" {
		t.Fatalf("manual review did not request input: %+v", result)
	}
	return result.RequestState
}

func callManualReview(t *testing.T, client *mcp.ClientSession, state string, responses mcp.InputResponseMap, mode string) *mcp.CallToolResult {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "review_pending", Arguments: map[string]any{"mode": mode},
		RequestState: state, InputResponses: responses,
	})
	if err != nil {
		t.Fatalf("manual review call: %v", err)
	}
	return result
}

func assertOnePendingNoDurableFacts(t *testing.T, st *localdolt.Store) {
	t.Helper()
	pending, err := st.PendingProposals(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending proposals = %+v (err %v), want one", pending, err)
	}
	if got := testCount(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 0 {
		t.Fatalf("failed review wrote %d durable facts", got)
	}
}

func stageReviewFact(t *testing.T, st *localdolt.Store, target localdolt.ProposalTarget, key, value string) localdolt.StagedProposal {
	t.Helper()
	staged, err := st.ProposeFact(context.Background(), localdolt.Proposal{
		Rationale: "review the staged fact", Actor: memory.Actor{Name: "agent:test"}.CommitAuthor(), Target: target,
	}, localdolt.Fact{Key: key, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

func stageReviewDecision(t *testing.T, st *localdolt.Store, target localdolt.ProposalTarget, title string) localdolt.StagedProposal {
	t.Helper()
	staged, err := st.ProposeDecision(context.Background(), localdolt.Proposal{
		Rationale: "review the staged decision", Actor: memory.Actor{Name: "agent:test"}.CommitAuthor(), Target: target,
	}, localdolt.Decision{Title: title, Rationale: "the test requires a distinct reviewed kind"})
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

func seedDurableFact(t *testing.T, st *localdolt.Store, key, value string) string {
	t.Helper()
	staged := stageReviewFact(t, st, localdolt.TargetRepo, key, value)
	if _, err := st.AcceptProposal(context.Background(), staged.ID, memory.UserActor.CommitAuthor(), localdolt.AcceptOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	return staged.RowID
}

func hasFactChange(diff localdolt.ProposalDiff, changeType, key string) bool {
	for _, change := range diff.Changes {
		if change.Table == "facts" && change.Type == changeType && change.To["key"] == key {
			return true
		}
	}
	return false
}
