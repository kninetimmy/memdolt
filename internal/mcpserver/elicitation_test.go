package mcpserver

import (
	"context"
	"fmt"
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

type afterAcceptBackend struct {
	*elicitationBackend
	afterAccept func()
}

func (s *afterAcceptBackend) ReviewAcceptExpected(ctx context.Context, id, expectedCommit string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
	result, err := s.elicitationBackend.ReviewAcceptExpected(ctx, id, expectedCommit, reviewer, force)
	if err == nil && s.afterAccept != nil {
		after := s.afterAccept
		s.afterAccept = nil
		after()
	}
	return result, err
}

func (s *elicitationBackend) ReviewAcceptExpected(ctx context.Context, id, expectedCommit string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
	paths, err := layout.New(s.baseDir)
	if err != nil {
		return localdolt.AcceptResult{}, err
	}
	return reviewgate.AcceptExpected(ctx, s.Store, paths.ConfigFile(), id, expectedCommit, reviewer, force)
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

func TestReviewPendingLegacySuccessiveUsesOneFormRound(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	first := stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.legacy.first", "approve me")
	second := stageReviewDecision(t, st.Store, localdolt.TargetRepo, "Skip this legacy proposal")

	server := New("test")
	tools := RegisterTools(server, base, st)
	var rounds int
	options := &mcp.ClientOptions{
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			rounds++
			if !strings.Contains(req.Params.Message, first.ID) || !strings.Contains(req.Params.Message, second.ID) {
				t.Fatalf("legacy one-round review omitted proposals: %s", req.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{
				first.ID: "approve", second.ID: "skip",
			}}, nil
		},
	}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "legacy-client", Version: "1"}, true, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if rounds != 1 || output.Status != "complete" || len(output.Accepted) != 1 ||
		output.Accepted[0].Proposal.ID != first.ID || len(output.Skipped) != 1 || output.Skipped[0] != second.ID {
		t.Fatalf("legacy successive output = %+v after %d form rounds", output, rounds)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingSuccessiveCursorReachesProposalTen(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	proposals := make([]localdolt.StagedProposal, 10)
	for i := range proposals {
		proposals[i] = stageReviewDecision(t, st.Store, localdolt.TargetRepo, fmt.Sprintf("Cursor proposal %02d", i+1))
	}

	server := New("test")
	tools := RegisterTools(server, base, st)
	var rounds int
	options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		rounds++
		if !strings.Contains(req.Params.Message, proposals[rounds-1].ID) {
			t.Fatalf("round %d showed the wrong proposal: %s", rounds, req.Params.Message)
		}
		decision := "skip"
		if rounds == 10 {
			decision = "approve"
		}
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": decision}}, nil
	}}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)

	firstPage := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if rounds != 9 || firstPage.Status != "partial" || firstPage.NextCursor == "" ||
		len(firstPage.Skipped) != 9 || len(firstPage.Accepted) != 0 || firstPage.RepoPending != 10 {
		t.Fatalf("first successive page = %+v after %d rounds", firstPage, rounds)
	}
	final := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{"cursor": firstPage.NextCursor})
	if rounds != 10 || final.Status != "complete" || len(final.Accepted) != 1 ||
		final.Accepted[0].Proposal.ID != proposals[9].ID || len(final.Skipped) != 9 || final.RepoPending != 9 {
		t.Fatalf("continued successive review = %+v after %d rounds", final, rounds)
	}
	replay, err := client.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "review_pending", Arguments: map[string]any{"cursor": firstPage.NextCursor},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.IsError {
		t.Fatal("used continuation cursor was replayable")
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingWithoutElicitationReportsMixedCLIPath(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.cli.fact", "review in a terminal")
	stageReviewDecision(t, st.Store, localdolt.TargetGlobal, "Global also stays in the terminal")
	server := New("test")
	tools := RegisterTools(server, base, st)
	client, serverSession := connect(t, server,
		&mcp.Implementation{Name: "no-elicitation-client", Version: "1"}, false)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if output.Status != "cli_required" || output.RepoPending != 1 || output.GlobalPending != 1 ||
		!strings.Contains(output.Remedy, "1 global") || !strings.Contains(output.Remedy, "memdolt review") {
		t.Fatalf("mixed CLI fallback = %+v", output)
	}
	if pending, err := st.PendingProposals(context.Background()); err != nil || len(pending) != 2 {
		t.Fatalf("mixed CLI fallback changed proposals: %+v (err %v)", pending, err)
	}
	if got := testCount(t, st.Store, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 0 {
		t.Fatalf("CLI fallback wrote %d facts", got)
	}

	closeSessions(t, client, serverSession)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPendingURLOnlyElicitationUsesCLIPath(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.url.fact", "URL mode cannot render this form")
	server := New("test")
	tools := RegisterTools(server, base, st)
	var elicited bool
	options := &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			URL: &mcp.URLElicitationCapabilities{},
		}},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicited = true
			return &mcp.ElicitResult{Action: "cancel"}, nil
		},
	}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "url-only", Version: "1"}, false, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if elicited || output.Status != "cli_required" || output.RepoPending != 1 || !strings.Contains(output.Remedy, "memdolt review") {
		t.Fatalf("URL-only fallback elicited=%v output=%+v", elicited, output)
	}
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
		len(bound.ProposalIDs) != 1 || len(bound.ProposalCommits) != 1 || bound.ProposalCommits[0] == "" ||
		bound.Position != 0 || bound.Action != actionReviewOne {
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

func TestReviewPendingBatchReportsPartialProgressOnLaterGuardFailure(t *testing.T) {
	base, st := initializedElicitationBackend(t)
	first := stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.partial.first", "safe first fact")
	second := stageReviewFact(t, st.Store, localdolt.TargetRepo, "review.partial.second", "BLOCKED second fact")
	paths, err := layout.New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("[deny_list]\npatterns = ['BLOCKED']\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := New("test")
	tools := RegisterTools(server, base, st)
	var shown string
	options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		shown = req.Params.Message
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "approve_all"}}, nil
	}}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{"mode": "batch"})
	if !strings.Contains(shown, "without undoing earlier approvals") || output.Status != "blocked" ||
		len(output.Accepted) != 1 || output.Accepted[0].Proposal.ID != first.ID ||
		len(output.Failures) != 1 || output.Failures[0].ProposalID != second.ID || output.RepoPending != 1 {
		t.Fatalf("partial batch output = %+v; dialog = %q", output, shown)
	}
	if got := testCount(t, st.Store, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 1 {
		t.Fatalf("partial batch left %d durable facts, want the first one", got)
	}
	pending, err := st.PendingProposals(context.Background())
	if err != nil || len(pending) != 1 || pending[0].ID != second.ID {
		t.Fatalf("partial batch pending = %+v (err %v)", pending, err)
	}

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

func TestReviewPendingContinuationStorageFailureReportsLandedApproval(t *testing.T) {
	base, inner := initializedElicitationBackend(t)
	first := stageReviewFact(t, inner.Store, localdolt.TargetRepo, "review.storage.first", "lands before bookkeeping fails")
	stageReviewDecision(t, inner.Store, localdolt.TargetRepo, "Never reached after bookkeeping failure")
	backend := &afterAcceptBackend{elicitationBackend: inner}
	server := New("test")
	tools := RegisterTools(server, base, backend)
	backend.afterAccept = func() { _ = tools.elicit.Close() }
	options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"decision": "approve"}}, nil
	}}
	client, serverSession := connectWithOptions(t, server,
		&mcp.Implementation{Name: "Codex", Version: "1"}, false, options)

	output := callAs[reviewPendingOutput](t, client, "review_pending", map[string]any{})
	if output.Status != "stopped" || len(output.Accepted) != 1 ||
		output.Accepted[0].Proposal.ID != first.ID || len(output.Failures) != 1 ||
		!strings.Contains(output.Failures[0].Error, "closed") || output.RepoPending != 1 {
		t.Fatalf("post-approval bookkeeping failure output = %+v", output)
	}
	if got := testCount(t, inner.Store, "SELECT COUNT(*) FROM facts AS OF 'main'"); got != 1 {
		t.Fatalf("bookkeeping failure lost the authorized merge; durable facts = %d", got)
	}

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

func TestProposeFactConflictCurrentChangeCannotStageStaleResolution(t *testing.T) {
	for _, action := range []string{"overwrite", "supersede"} {
		t.Run(action, func(t *testing.T) {
			base, st := initializedToolStore(t)
			currentID := seedDurableFact(t, st, "build.command", "go test ./...")
			server := New("test")
			backend := testElicitationBackend(base, st)
			tools := RegisterTools(server, base, backend)
			current, err := tools.liveFactByKey(context.Background(), "build.command")
			if err != nil {
				t.Fatal(err)
			}
			changer, err := st.ProposeFactResolution(context.Background(), localdolt.Proposal{
				Rationale: "change the current row while its old image is displayed",
				Actor:     memory.Actor{Name: "agent:concurrent"}.CommitAuthor(),
				Target:    localdolt.TargetRepo,
			}, *current, localdolt.Fact{Key: "build.command", Value: "go test -count=1 ./..."}, localdolt.FactResolutionOverwrite)
			if err != nil {
				t.Fatal(err)
			}
			var mainAfterConcurrentChange string
			options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				if _, err := st.AcceptProposal(context.Background(), changer.ID, memory.UserActor.CommitAuthor(), localdolt.AcceptOptions{Force: true}); err != nil {
					t.Fatalf("accept concurrent current-row change: %v", err)
				}
				mainAfterConcurrentChange = testText(t, st, "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1")
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"action": action}}, nil
			}}
			client, serverSession := connectWithOptions(t, server,
				&mcp.Implementation{Name: "Claude Code", Version: "1"}, false, options)

			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "propose_fact", Arguments: map[string]any{
				"key": "build.command", "value": "go test -race ./...", "rationale": "respond against the displayed old row",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("stale %s response staged a proposal: %+v", action, result.Content)
			}
			if got := testText(t, st, "SELECT value FROM facts AS OF 'main' WHERE id = ?", currentID); got != "go test -count=1 ./..." {
				t.Fatalf("main fact after stale response = %q", got)
			}
			if got := testText(t, st, "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1"); got != mainAfterConcurrentChange {
				t.Fatalf("stale %s response moved main from %s to %s", action, mainAfterConcurrentChange, got)
			}
			if pending, err := st.PendingProposals(context.Background()); err != nil || len(pending) != 0 {
				t.Fatalf("stale %s response left proposals %+v (err %v)", action, pending, err)
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
