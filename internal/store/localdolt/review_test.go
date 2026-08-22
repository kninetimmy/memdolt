package localdolt_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// reviewer is the human PRD §7 puts on the promoting end of the gate. It is
// deliberately not stagingActor: the whole point of the review lane is that
// the actor who proposed and the actor who promoted are different, and
// dolt_log has to show both.
var reviewer = store.Actor{Name: "user", Email: "user@memdolt.invalid"}

// TestReviewAcceptMergesTheProposalAndDeletesItsBranch covers acceptance
// criteria 1, 2 and 3: a staged fact is listed as pending, accepting it
// merges exactly its branch into main and deletes the branch, the row is
// durable on main, and the whole propose-review-merge cycle is auditable
// from dolt_log alone — the staging commit and the merge commit both
// present with their own actors, and the merge commit's message recording
// the review action.
func TestReviewAcceptMergesTheProposalAndDeletesItsBranch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the race lane is the one CI runs",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{Key: "build.command", Value: "go test -race ./...", Kind: "command"})
	if err != nil {
		t.Fatalf("propose fact: %v", err)
	}
	mainHead := headCommit(t, st)

	// The listing sees it, and sees it as what was staged.
	pending, err := st.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("list pending proposals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d proposals are pending, want 1", len(pending))
	}
	listed := pending[0]
	if listed.ID != staged.ID || listed.Branch != staged.Branch || listed.Commit != staged.Commit {
		t.Fatalf("listed proposal is %+v, want the staged %+v", listed, staged)
	}
	if listed.Kind != localdolt.KindFact || listed.Target != localdolt.TargetRepo {
		t.Fatalf("listed proposal is %s/%s, want fact/repo", listed.Kind, listed.Target)
	}
	if listed.Actor != stagingActor.Name {
		t.Fatalf("listed proposal actor = %q, want the staging actor %q", listed.Actor, stagingActor.Name)
	}
	if listed.Rationale != "the race lane is the one CI runs" {
		t.Fatalf("listed rationale = %q", listed.Rationale)
	}
	if listed.Message != "propose fact build.command" {
		t.Fatalf("listed staging message = %q", listed.Message)
	}

	// And the diff of the one commit that is the proposal's whole payload.
	diff, err := st.ProposalDiff(ctx, staged.ID)
	if err != nil {
		t.Fatalf("show proposal: %v", err)
	}
	if diff.Parent != mainHead {
		t.Fatalf("the proposal's commit is diffed against %q, want main's head %q", diff.Parent, mainHead)
	}
	changed := map[string]string{}
	for _, change := range diff.Changes {
		changed[change.Table] = change.Type
		if change.From != nil {
			t.Fatalf("an added row was rendered with a before image: %+v", change)
		}
	}
	if len(changed) != 2 || changed["facts"] != "added" || changed["proposals"] != "added" {
		t.Fatalf("the proposal's diff touches %v, want facts and proposals added", changed)
	}
	for _, change := range diff.Changes {
		if change.Table != "facts" {
			continue
		}
		if change.To["key"] != "build.command" || change.To["value"] != "go test -race ./..." {
			t.Fatalf("the rendered fact row is %v, want the proposed key and value", change.To)
		}
	}

	result, err := st.AcceptProposal(ctx, staged.ID, reviewer)
	if err != nil {
		t.Fatalf("accept proposal: %v", err)
	}
	if result.Proposal.ID != staged.ID {
		t.Fatalf("accepted %q, want %q", result.Proposal.ID, staged.ID)
	}
	if len(result.Cleared) != 0 {
		t.Fatalf("a clean merge cleared %v", result.Cleared)
	}

	// Criterion 2: the row is durable on main, and the branch is gone.
	if got := scanInt(t, st,
		"SELECT COUNT(*) FROM facts WHERE id = ? AND live_key = ?", staged.RowID, "build.command"); got != 1 {
		t.Fatal("the accepted fact is not live on main")
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM proposals WHERE id = ?", staged.ID); got != 1 {
		t.Fatal("the proposal's metadata row did not merge onto main with it")
	}
	requireProposalBranches(t, st)
	if pending, err := st.PendingProposals(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("after accepting, %d proposals are pending (err %v), want 0", len(pending), err)
	}

	// Criterion 3: dolt_log alone tells the whole story. The merge commit
	// names the review action and the reviewer; the staging commit is still
	// there under the agent that wrote it; and the merge commit descends
	// from both, so the branch's work is reachable rather than replayed.
	merge := headCommitOn(t, st, localdolt.MainBranch)
	if merge.hash != result.Commit {
		t.Fatalf("main's head is %q, want the reported merge commit %q", merge.hash, result.Commit)
	}
	if want := "review accept fact " + staged.ID; merge.message != want {
		t.Fatalf("merge commit message = %q, want %q", merge.message, want)
	}
	if merge.committer != reviewer.Name || merge.email != reviewer.Email {
		t.Fatalf("merge commit author = %q <%s>, want the reviewer %q <%s>",
			merge.committer, merge.email, reviewer.Name, reviewer.Email)
	}
	parents := parentsOf(t, st, result.Commit)
	if len(parents) != 2 || parents[0] != mainHead || parents[1] != staged.Commit {
		t.Fatalf("merge commit parents = %v, want [%s %s]", parents, mainHead, staged.Commit)
	}
	staging := commitRecordOf(t, st, staged.Commit)
	if staging.message != "propose fact build.command" || staging.committer != stagingActor.Name {
		t.Fatalf("the staging commit reads %q by %q, want it preserved under the agent that staged it",
			staging.message, staging.committer)
	}
}

// TestReviewAcceptSupersedeClearsAnAlreadySatisfiedViolation covers
// acceptance criterion 4 and PRD §6.3's accepted-supersede case: merging a
// supersede proposal into a main whose facts table has moved since staging
// reports a constraint violation that is already satisfied. Verification
// runs against the merged rows, the reviewed records are cleared on that
// evidence, and nothing beyond the merged rows changes.
func TestReviewAcceptSupersedeClearsAnAlreadySatisfiedViolation(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	const supersededID = "01J0000000000000000000FACT"
	const bystanderID = "01J000000000000000000OTHER"
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)",
			Args: []any{
				supersededID, "build.command", "go test ./...", "user", "2026-08-02 12:00:00",
				bystanderID, "convention.style", "gofmt", "user", "2026-08-02 12:00:00",
			},
		}},
		Text:    []string{"build.command", "go test ./...", "convention.style", "gofmt"},
		Message: "record the durable facts",
		Author:  reviewer,
	}); err != nil {
		t.Fatalf("seed durable facts: %v", err)
	}

	staged, err := st.ProposeSupersede(ctx, localdolt.Proposal{
		Rationale: "the race lane replaced the plain one",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, supersededID, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
	if err != nil {
		t.Fatalf("propose supersede: %v", err)
	}

	// Move facts on main after staging. Per the M1 spike this is exactly
	// what turns the accept into an already-satisfied constraint violation:
	// the trigger is facts having commits on both sides, whatever rows they
	// touched (docs/spikes/m1-fact-key-uniqueness.md §6, control 1).
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "UPDATE facts SET value = ? WHERE id = ?",
			Args: []any{"gofmt -l .", bystanderID},
		}},
		Text:    []string{"gofmt -l ."},
		Message: "an unrelated durable edit",
		Author:  reviewer,
	}); err != nil {
		t.Fatalf("move main: %v", err)
	}
	mainHead := headCommit(t, st)

	result, err := st.AcceptProposal(ctx, staged.ID, reviewer)
	if err != nil {
		t.Fatalf("accept the supersede proposal: %v", err)
	}

	// The violation was reported, verified against the merged rows, and
	// cleared on that evidence rather than raised.
	if len(result.Cleared) != 1 {
		t.Fatalf("the accept cleared %v, want one already-satisfied violation", result.Cleared)
	}
	cleared := result.Cleared[0]
	if cleared.Table != "facts" || cleared.Constraint != "uk_fact_live_key" {
		t.Fatalf("cleared %s on %s, want uk_fact_live_key on facts", cleared.Constraint, cleared.Table)
	}
	if len(cleared.Rows) != 2 {
		t.Fatalf("cleared rows %v, want the superseded row and its replacement", cleared.Rows)
	}
	for _, want := range []string{supersededID, staged.RowID} {
		if !slices.Contains(cleared.Rows, want) {
			t.Fatalf("cleared rows %v do not include %s", cleared.Rows, want)
		}
	}

	// The merged rows are what the supersede staged, and nothing else moved.
	old := readStagedFact(t, st, localdolt.MainBranch, supersededID)
	if old.supersededBy.String != staged.RowID || old.liveKey.Valid || old.value != "go test ./..." {
		t.Fatalf("the superseded row on main is %+v, want it linked, not live and otherwise untouched", old)
	}
	replacement := readStagedFact(t, st, localdolt.MainBranch, staged.RowID)
	if replacement.value != "go test -race ./..." || replacement.liveKey.String != "build.command" {
		t.Fatalf("the replacement row on main is %+v, want it live under the key", replacement)
	}
	bystander := readStagedFact(t, st, localdolt.MainBranch, bystanderID)
	if bystander.value != "gofmt -l ." || bystander.liveKey.String != "convention.style" {
		t.Fatalf("a row outside the merge changed: %+v", bystander)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 3 {
		t.Fatalf("main carries %d facts, want 3 (nothing was deleted to satisfy the constraint)", got)
	}

	// Criterion 4's "no row beyond the merged ones changes", read off the
	// merge commit's own diff rather than off the rows.
	requireCommitAdds(t, st, mainHead, result.Commit, map[string]int{"facts": 1, "proposals": 1})
	requireProposalBranches(t, st)
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_constraint_violations_facts"); got != 0 {
		t.Fatalf("%d constraint-violation records survived the accept", got)
	}
	requireOnMain(t, st)
}

// TestReviewAcceptStaysBlocked covers acceptance criterion 5: each of the
// three refusals PRD §6.3 requires leaves main where it was, the proposal
// still pending, both surfaces empty and every row on both sides intact.
func TestReviewAcceptStaysBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed writes the durable rows the case needs and returns the id of
		// the fact the proposal will be staged against, if any.
		seed func(*testing.T, *localdolt.Store) string
		// stage stages the proposal that will fail to merge.
		stage func(*testing.T, *localdolt.Store, string) localdolt.StagedProposal
		// diverge moves main after staging.
		diverge func(*testing.T, *localdolt.Store, string)
		want    string
	}{
		{
			// PRD §6.3's first class: two machines gave one fact key two
			// live values between syncs. The violation is real, verification
			// says so, and no row is superseded or deleted to make it go away.
			name: "a constraint violation that verification says is real",
			seed: func(*testing.T, *localdolt.Store) string { return "" },
			stage: func(t *testing.T, st *localdolt.Store, _ string) localdolt.StagedProposal {
				staged, err := st.ProposeFact(context.Background(), localdolt.Proposal{
					Rationale: "the race lane", Actor: stagingActor, Target: localdolt.TargetRepo,
				}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
				if err != nil {
					t.Fatalf("propose fact: %v", err)
				}
				return staged
			},
			diverge: func(t *testing.T, st *localdolt.Store, _ string) {
				commitOnMain(t, st, "record a durable fact under the same key",
					[]string{"build.command", "go test ./..."},
					store.Statement{
						SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
						Args: []any{"01J0000000000000000000MAIN", "build.command", "go test ./...", "user", "2026-08-02 12:00:00"},
					})
			},
			want: "constraint verification failed",
		},
		{
			// PRD §6.3's third class: one cell, two disagreeing writes. The
			// operator picks; this lane must not.
			name: "a data conflict",
			seed: func(t *testing.T, st *localdolt.Store) string {
				const id = "01J0000000000000000000FACT"
				commitOnMain(t, st, "record the durable build command",
					[]string{"build.command", "go test ./..."},
					store.Statement{
						SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
						Args: []any{id, "build.command", "go test ./...", "user", "2026-08-02 12:00:00"},
					})
				return id
			},
			stage: func(t *testing.T, st *localdolt.Store, id string) localdolt.StagedProposal {
				staged, err := st.ProposeSupersede(context.Background(), localdolt.Proposal{
					Rationale: "the race lane", Actor: stagingActor, Target: localdolt.TargetRepo,
				}, id, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
				if err != nil {
					t.Fatalf("propose supersede: %v", err)
				}
				return staged
			},
			diverge: func(t *testing.T, st *localdolt.Store, id string) {
				// main writes the same cell the proposal's link writes.
				commitOnMain(t, st, "supersede the same row on main", nil,
					store.Statement{
						SQL:  "UPDATE facts SET superseded_by = ? WHERE id = ?",
						Args: []any{"01J000000000000000000MAIN", id},
					})
			},
			want: "data conflict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)

			target := tc.seed(t, st)
			staged := tc.stage(t, st, target)
			tc.diverge(t, st, target)

			mainHead := headCommit(t, st)
			factsBefore := scanInt(t, st, "SELECT COUNT(*) FROM facts")

			_, err := st.AcceptProposal(ctx, staged.ID, reviewer)
			if err == nil {
				t.Fatal("the accept concluded a merge that should have stayed blocked")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "blocked") {
				t.Fatalf("refusal %q does not say the merge is blocked", err)
			}

			// Nothing was auto-resolved and no path deleted unmerged data.
			if got := headCommit(t, st); got != mainHead {
				t.Fatalf("main head = %q, want %q: a blocked merge moved main", got, mainHead)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != factsBefore {
				t.Fatalf("main carries %d facts after a blocked merge, want %d", got, factsBefore)
			}
			requireProposalBranches(t, st, staged.Branch)
			if got := commitsOn(t, st, staged.Branch); got == 0 {
				t.Fatal("the blocked merge emptied the proposal branch")
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_status"); got != 0 {
				t.Fatalf("a blocked merge left %d table(s) uncommitted in the working set", got)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_conflicts"); got != 0 {
				t.Fatalf("a blocked merge left %d conflicted table(s) behind", got)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_constraint_violations"); got != 0 {
				t.Fatalf("a blocked merge left %d violated table(s) behind", got)
			}
			requireOnMain(t, st)

			// The store is still usable, and the proposal can still be
			// reviewed: a refusal is not a wedge.
			commitOnMain(t, st, "a durable write after the blocked merge", nil,
				store.Statement{SQL: "INSERT INTO meta (k, v) VALUES ('probe', 'after block')"})
			if _, err := st.RejectProposal(ctx, staged.ID); err != nil {
				t.Fatalf("reject the blocked proposal: %v", err)
			}
		})
	}
}

// TestReviewAcceptRefusesMoreThanTheOneStagedCommit covers the guard that
// closes the gap between what an accept renders and scans — the head commit
// of the proposal branch — and what it used to merge, which was the branch.
// A proposal is one commit cut from main (PRD §3.1), memdolt's own
// staged-write lane produces nothing else, and a branch carrying more is
// refused by count rather than merged partly unreviewed.
func TestReviewAcceptRefusesMoreThanTheOneStagedCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// craft turns a proposal memdolt staged into one memdolt could not
		// have, the way the only writer that can do it would.
		craft func(*testing.T, *localdolt.Store, localdolt.StagedProposal)
	}{
		{
			// The straightforward case: a second commit, written onto the
			// branch after review would have seen the first.
			name: "a second commit written straight onto the branch",
			craft: func(t *testing.T, st *localdolt.Store, staged localdolt.StagedProposal) {
				commitByHand(t, st, staged.Branch, "sneak a second commit onto the proposal",
					store.Statement{
						SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
						Args: []any{
							"01J00000000000000000SNEAK", "env.token", "unreviewed",
							"agent:claude-code", "2026-08-02 12:00:00",
						},
					})
			},
		},
		{
			// The same thing arrived at sideways: the branch head is a merge
			// commit, so the branch carries its staging commit and the merge
			// that buried it.
			name: "a merge commit at the branch head",
			craft: func(t *testing.T, st *localdolt.Store, staged localdolt.StagedProposal) {
				commitOnMain(t, st, "a durable write on main",
					[]string{"convention.style", "gofmt -l ."},
					store.Statement{
						SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
						Args: []any{
							"01J0000000000000000000MAIN", "convention.style", "gofmt -l .",
							"user", "2026-08-02 12:00:00",
						},
					})
				mergeMainByHand(t, st, staged.Branch)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)

			staged, err := st.ProposeFact(ctx, localdolt.Proposal{
				Rationale: "the race lane is the one CI runs", Actor: stagingActor, Target: localdolt.TargetRepo,
			}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
			if err != nil {
				t.Fatalf("propose fact: %v", err)
			}
			tc.craft(t, st, staged)

			mainHead := headCommit(t, st)
			mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")
			factsBefore := scanInt(t, st, "SELECT COUNT(*) FROM facts")

			_, err = st.AcceptProposal(ctx, staged.ID, reviewer)
			if err == nil {
				t.Fatal("the accept merged a branch carrying more than the proposal's own commit")
			}
			// The refusal names the proposal and what it counted, so the
			// operator can go and look at the branch.
			for _, want := range []string{"refusing to promote", staged.ID, staged.Branch, "2 commits"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not mention %q", err, want)
				}
			}

			// Nothing was promoted: main did not move and no row of either
			// commit became durable.
			if got := headCommit(t, st); got != mainHead {
				t.Fatalf("main head = %q, want %q: a refused accept moved main", got, mainHead)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != mainCommits {
				t.Fatalf("main has %d commits after a refused accept, want %d", got, mainCommits)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != factsBefore {
				t.Fatalf("main carries %d facts after a refused accept, want %d", got, factsBefore)
			}
			if got := scanInt(t, st, "SELECT COUNT(*) FROM proposals"); got != 0 {
				t.Fatalf("a refused accept put %d proposals row(s) on main", got)
			}

			// And nothing was discarded either: the branch is still there,
			// the proposal is still pending, and the operator still has
			// every other review verb.
			requireProposalBranches(t, st, staged.Branch)
			pending, err := st.PendingProposals(ctx)
			if err != nil {
				t.Fatalf("list pending proposals: %v", err)
			}
			if len(pending) != 1 || pending[0].ID != staged.ID {
				t.Fatalf("after a refused accept the pending proposals are %+v, want the refused one", pending)
			}
			if _, err := st.ProposalDiff(ctx, staged.ID); err != nil {
				t.Fatalf("show the refused proposal: %v", err)
			}
			requireOnMain(t, st)
			if _, err := st.RejectProposal(ctx, staged.ID); err != nil {
				t.Fatalf("reject the refused proposal: %v", err)
			}
		})
	}
}

// TestReviewAcceptPromotesAProposalStagedBeforeMainMoved covers the other
// side of that guard: the count is of what the proposal carries past its
// merge base, not of how far main has run on since, so a direct-lane write
// landing between staging and review is not a reason to refuse. It also
// pins what the merge commit descends from — main's previous head first,
// and the staging commit `review list` reported second.
func TestReviewAcceptPromotesAProposalStagedBeforeMainMoved(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the race lane is the one CI runs", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
	if err != nil {
		t.Fatalf("propose fact: %v", err)
	}

	// A direct lane writes to main while the proposal sits in review
	// (PRD §3.1: the direct lanes do not wait for anybody).
	commitOnMain(t, st, "note what the session was doing", []string{"reviewing the build command"},
		store.Statement{
			SQL: "INSERT INTO session_notes (id, actor, text, created_at) VALUES (?, ?, ?, ?)",
			Args: []any{
				"01J000000000000000000NOTE", "user", "reviewing the build command", "2026-08-02 12:00:00",
			},
		})
	mainHead := headCommit(t, st)
	if mainHead == staged.Commit {
		t.Fatal("the direct-lane write did not move main")
	}

	// What review reports before the accept is still the commit the
	// proposal was staged with, not the branch's position relative to main.
	pending, err := st.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("list pending proposals: %v", err)
	}
	if len(pending) != 1 || pending[0].Commit != staged.Commit {
		t.Fatalf("review list reports %+v, want the staging commit %q", pending, staged.Commit)
	}

	result, err := st.AcceptProposal(ctx, staged.ID, reviewer)
	if err != nil {
		t.Fatalf("accept a proposal staged before main moved: %v", err)
	}

	// The merge commit descends from both sides, and its second parent is
	// the commit review reported — the one the deny-list scanned.
	parents := parentsOf(t, st, result.Commit)
	if len(parents) != 2 || parents[0] != mainHead || parents[1] != pending[0].Commit {
		t.Fatalf("merge commit parents = %v, want [%s %s]", parents, mainHead, pending[0].Commit)
	}
	if got := headCommit(t, st); got != result.Commit {
		t.Fatalf("main head = %q, want the merge commit %q", got, result.Commit)
	}

	// Both sides are durable: the promoted fact and the direct-lane row the
	// merge had to carry past.
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts WHERE id = ? AND live_key = ?",
		staged.RowID, "build.command"); got != 1 {
		t.Fatal("the accepted fact is not live on main")
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM session_notes"); got != 1 {
		t.Fatalf("main carries %d session notes after the merge, want the direct-lane row", got)
	}
	requireProposalBranches(t, st)
	requireOnMain(t, st)
}

// TestReviewShowDiffsAgainstTheHeadsFirstParent covers what `review show`
// renders on a branch whose head has two parents. Nothing in SQL orders the
// rows of dolt_commit_ancestors, so the parent an unordered read returns is
// whichever one came back — and here the two are different diffs, since one
// parent is the proposal's own staging commit and the other is main.
//
// This asserts the contract rather than reproducing a failure: probed on
// this build, the unordered read returned parent_index 0 ten times out of
// ten (docs/spikes/m1-conflict-surfaces.md §8). An order nothing asks for is
// not one to depend on, and this is the assertion that notices if it changes.
//
// Accept refuses this branch (it carries two commits past main); show does
// not, which is how an operator sees what is on it before rejecting it.
func TestReviewShowDiffsAgainstTheHeadsFirstParent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the race lane is the one CI runs", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
	if err != nil {
		t.Fatalf("propose fact: %v", err)
	}
	commitOnMain(t, st, "a durable write on main", []string{"convention.style", "gofmt -l ."},
		store.Statement{
			SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{
				"01J0000000000000000000MAIN", "convention.style", "gofmt -l .", "user", "2026-08-02 12:00:00",
			},
		})
	mergeMainByHand(t, st, staged.Branch)

	head := headCommitOn(t, st, staged.Branch).hash
	parents := parentsOf(t, st, head)
	if len(parents) != 2 {
		t.Fatalf("the crafted branch head has parents %v, want two", parents)
	}
	if parents[0] != staged.Commit {
		t.Fatalf("the crafted head's first parent is %q, want the staging commit %q", parents[0], staged.Commit)
	}

	first, err := st.ProposalDiff(ctx, staged.ID)
	if err != nil {
		t.Fatalf("show proposal: %v", err)
	}
	if first.Proposal.Commit != head {
		t.Fatalf("show rendered commit %q, want the branch head %q", first.Proposal.Commit, head)
	}
	if first.Parent != parents[0] {
		t.Fatalf("show diffed against %q, want the head's first parent %q (its second is %q)",
			first.Parent, parents[0], parents[1])
	}
	// Which parent it is decides what the diff says, not just which hash it
	// reports: against the first parent the head commit is what the merge
	// brought over from main, and the proposal's own row is not in it.
	if len(first.Changes) != 1 {
		t.Fatalf("show rendered %+v, want the one row the head commit changed", first.Changes)
	}
	if change := first.Changes[0]; change.Table != "facts" || change.To["key"] != "convention.style" {
		t.Fatalf("show rendered %+v, want the row the merge brought over from main", change)
	}

	for i := range 5 {
		again, err := st.ProposalDiff(ctx, staged.ID)
		if err != nil {
			t.Fatalf("show proposal again (%d): %v", i, err)
		}
		if again.Parent != first.Parent || !reflect.DeepEqual(again.Changes, first.Changes) {
			t.Fatalf("show rendered %q %+v on invocation %d, want the %q %+v it rendered first",
				again.Parent, again.Changes, i, first.Parent, first.Changes)
		}
	}
}

// commitByHand puts a second commit on a proposal branch the way the only
// writer that can reach past memdolt would: an agent with a shell and a SQL
// prompt, while no owner holds the store. The staged-write lane cannot
// produce such a branch — stage() cuts from main, writes once, commits once,
// or takes the branch with it — so the guard that refuses one has to be
// tested against a branch built this way.
//
// It goes through RunOnBranch because the checkout, the writes and the
// commit have to share one session, which the store's pool cannot give a
// test: the embedded driver throws every connection away rather than reuse
// it (see export_test.go).
func commitByHand(t *testing.T, st *localdolt.Store, branch, message string, statements ...store.Statement) {
	t.Helper()
	statements = append(slices.Clone(statements), store.Statement{
		SQL:  "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
		Args: []any{message, stagingActor.String()},
	})
	if err := st.RunOnBranch(context.Background(), branch, statements...); err != nil {
		t.Fatalf("write a second commit onto %s by hand: %v", branch, err)
	}
	requireOnMain(t, st)
}

// mergeMainByHand gives a proposal branch a head with two parents, the other
// shape memdolt's own lane cannot produce. main has to have moved first, or
// there is nothing to merge.
func mergeMainByHand(t *testing.T, st *localdolt.Store, branch string) {
	t.Helper()
	if err := st.RunOnBranch(context.Background(), branch, store.Statement{
		SQL:  "CALL DOLT_MERGE('--no-ff', '-m', ?, ?, '--author', ?)",
		Args: []any{"merge main into the proposal by hand", localdolt.MainBranch, stagingActor.String()},
	}); err != nil {
		t.Fatalf("merge %s into %s by hand: %v", localdolt.MainBranch, branch, err)
	}
	requireOnMain(t, st)
}

// TestReviewRejectDeletesTheBranchAndLeavesMainUnchanged covers the other
// half of acceptance criterion 2.
func TestReviewRejectDeletesTheBranchAndLeavesMainUnchanged(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	keep, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "worth keeping", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose the fact to keep: %v", err)
	}
	drop, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "not this one", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
	if err != nil {
		t.Fatalf("propose the fact to reject: %v", err)
	}
	mainHead := headCommit(t, st)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	rejected, err := st.RejectProposal(ctx, drop.ID)
	if err != nil {
		t.Fatalf("reject proposal: %v", err)
	}
	if rejected.ID != drop.ID || rejected.Rationale != "not this one" {
		t.Fatalf("rejected %+v, want the proposal that was named", rejected)
	}

	// Only that proposal's branch went, main did not move, and rejecting
	// added no commit of its own.
	requireProposalBranches(t, st, keep.Branch)
	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: a rejection moved main", got, mainHead)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != mainCommits {
		t.Fatalf("main has %d commits after a rejection, want %d", got, mainCommits)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 0 {
		t.Fatalf("main carries %d facts after a rejection, want 0", got)
	}

	// A rejected proposal is gone, and saying so twice is an error rather
	// than a silent success.
	if _, err := st.RejectProposal(ctx, drop.ID); err == nil {
		t.Fatal("rejecting the same proposal twice succeeded")
	}
}

// TestReviewExpireSweepsByAgeAndStaleOnlyReports covers acceptance
// criterion 6: expiry removes proposal branches older than the configured
// age, and the audit that reports them mutates nothing.
func TestReviewExpireSweepsByAgeAndStaleOnlyReports(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	first, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "one", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	second, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "two", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	mainHead := headCommit(t, st)

	// The audit reads and writes nothing. It is run first, and the sweep
	// below proves both proposals were still there for it to find.
	pending, err := st.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("list pending proposals: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("%d proposals are pending, want 2", len(pending))
	}
	for _, proposal := range pending {
		if proposal.Age(time.Now()) < 0 {
			t.Fatalf("proposal %s reports a negative age; it was staged at %s", proposal.ID, proposal.StagedAt)
		}
	}
	requireProposalBranches(t, st, first.Branch, second.Branch)
	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("listing the proposals moved main to %q", got)
	}

	// A cutoff older than every proposal sweeps nothing.
	swept, err := st.ExpireProposals(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("expire with an hour-old cutoff: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("an hour-old cutoff swept %d proposal(s), want none", len(swept))
	}
	requireProposalBranches(t, st, first.Branch, second.Branch)

	// A cutoff of now sweeps both — and reports what it removed, with the
	// metadata read before the branch carrying it was deleted.
	swept, err = st.ExpireProposals(ctx, time.Now())
	if err != nil {
		t.Fatalf("expire with a cutoff of now: %v", err)
	}
	if len(swept) != 2 {
		t.Fatalf("expiry removed %d proposal(s), want 2", len(swept))
	}
	rationales := []string{swept[0].Rationale, swept[1].Rationale}
	if !slices.Contains(rationales, "one") || !slices.Contains(rationales, "two") {
		t.Fatalf("expiry reported %v, want both proposals described", rationales)
	}
	requireProposalBranches(t, st)

	// Expiry is a branch sweep: main never moves and nothing becomes durable.
	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: expiry moved main", got, mainHead)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 0 {
		t.Fatalf("main carries %d facts after expiry, want 0", got)
	}
}

// TestReviewAgingIsStableAcrossReaderTimezones covers the N7 question about
// expiry's clock: age is measured from latest_commit_date as the driver
// reports it into branchRecord.stagedAt, so whether a branch can be
// mis-aged is a question about how that value crosses the driver boundary.
// A commit's instant does not depend on the zone it was written under —
// time.Now is the same instant whatever time.Local says — but a zone-naive
// parse on the way out would not be, so this stages under one forced
// offset, reads and expires under the opposite one, and requires the
// reported staging time to stay inside the wall-clock window the staging
// call took. An hour-scale mis-age cannot fit that window.
func TestReviewAgingIsStableAcrossReaderTimezones(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	original := time.Local
	t.Cleanup(func() { time.Local = original })

	// Stage under UTC+13.
	time.Local = time.FixedZone("probe+13", 13*3600)
	stageStarted := time.Now()
	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "aged across a timezone change", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	stageEnded := time.Now()

	// Read and expire under UTC-11.
	time.Local = time.FixedZone("probe-11", -11*3600)
	pending, err := st.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("list pending proposals under the shifted reader clock: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != staged.ID {
		t.Fatalf("pending proposals are %+v, want the staged one", pending)
	}

	windowStart := stageStarted.Add(-time.Minute)
	windowEnd := stageEnded.Add(time.Minute)
	if reported := pending[0].StagedAt; reported.Before(windowStart) || reported.After(windowEnd) {
		t.Fatalf("staging time reported as %s under a shifted reader clock, "+
			"want an instant inside [%s, %s]: latest_commit_date did not survive the driver as the instant it was committed as",
			reported, windowStart, windowEnd)
	}
	if age := pending[0].Age(time.Now()); age < 0 || age > time.Hour {
		t.Fatalf("proposal age = %s, want a small non-negative duration", age)
	}

	swept, err := st.ExpireProposals(ctx, time.Now())
	if err != nil {
		t.Fatalf("expire under the shifted reader clock: %v", err)
	}
	if len(swept) != 1 || swept[0].ID != staged.ID {
		t.Fatalf("expiry swept %+v under the shifted reader clock, want the staged proposal", swept)
	}
	requireProposalBranches(t, st)
}

// TestReviewRefusalsAreLoud covers the errors the review verbs owe rather
// than swallow, including the two that are boundaries: an id that is not a
// ULID never reaches the SQL text of an AS OF clause, and a global-target
// proposal is not promoted into the repository store that PRD §10 says is
// the wrong destination for it.
func TestReviewRefusalsAreLoud(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	global, err := st.ProposeDecision(ctx, localdolt.Proposal{
		Rationale: "belongs to every project", Actor: stagingActor, Target: localdolt.TargetGlobal,
	}, localdolt.Decision{Title: "Prefer the standard library", Rationale: "fewer dependencies to audit"})
	if err != nil {
		t.Fatalf("propose a global decision: %v", err)
	}

	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "accepting an id that is not a ULID",
			call: func() error {
				_, err := st.AcceptProposal(ctx, "'; DROP TABLE facts; --", reviewer)
				return err
			},
			want: "is not a proposal id",
		},
		{
			name: "accepting an id of the right length that is not base32",
			call: func() error {
				_, err := st.AcceptProposal(ctx, strings.Repeat("u", 26), reviewer)
				return err
			},
			want: "base32 alphabet",
		},
		{
			name: "showing a proposal that is not pending",
			call: func() error {
				_, err := st.ProposalDiff(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
				return err
			},
			want: "there is no pending proposal",
		},
		{
			name: "accepting a proposal that is not pending",
			call: func() error {
				_, err := st.AcceptProposal(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV", reviewer)
				return err
			},
			want: "there is no pending proposal",
		},
		{
			name: "rejecting a proposal that is not pending",
			call: func() error {
				_, err := st.RejectProposal(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
				return err
			},
			want: "there is no pending proposal",
		},
		{
			name: "accepting a global proposal into the repository store",
			call: func() error {
				_, err := st.AcceptProposal(ctx, global.ID, reviewer)
				return err
			},
			want: "global store",
		},
		{
			name: "accepting with no reviewer identity",
			call: func() error {
				_, err := st.AcceptProposal(ctx, global.ID, store.Actor{})
				return err
			},
			want: "reviewer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s succeeded", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}

	// The refused global proposal is still pending, still listable and
	// still rejectable: refusing to promote it is not refusing to review it.
	requireProposalBranches(t, st, global.Branch)
	if _, err := st.ProposalDiff(ctx, global.ID); err != nil {
		t.Fatalf("show the global proposal: %v", err)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM decisions"); got != 0 {
		t.Fatalf("the refused global proposal put %d decision(s) on main", got)
	}
}

// TestReviewAcceptRefusesToPromoteOverUncommittedWork covers the guard that
// keeps the merge commit honest: it concludes with DOLT_COMMIT('-A'), which
// stages every table, so anything already in the working set would be
// promoted along with the proposal it names.
func TestReviewAcceptRefusesToPromoteOverUncommittedWork(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "why", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}

	execQuery(t, st, "INSERT INTO session_notes (id, actor, text, created_at) "+
		"VALUES ('01J000000000000000000NOTE', 'user', 'uncommitted', '2026-08-02 12:00:00')")
	if _, err := st.AcceptProposal(ctx, staged.ID, reviewer); err == nil {
		t.Fatal("accepted a proposal over uncommitted work")
	} else if !strings.Contains(err.Error(), "refusing to promote a proposal") || strings.Contains(err.Error(), "stage a proposal") {
		t.Fatalf("refusal %q does not accurately describe blocked promotion", err)
	}
	requireProposalBranches(t, st, staged.Branch)

	execQuery(t, st, "CALL DOLT_RESET('--hard')")
	execQuery(t, st, "CALL DOLT_CLEAN()")
	if _, err := st.AcceptProposal(ctx, staged.ID, reviewer); err != nil {
		t.Fatalf("accept once the working set is clean: %v", err)
	}
}

// commitOnMain records one durable write, the way review or a direct lane
// would leave main. text is what the deny-list would scan (PRD §11.3);
// empty declares the write carries no prose.
func commitOnMain(t *testing.T, st *localdolt.Store, message string, text []string, statements ...store.Statement) {
	t.Helper()
	if _, err := st.Commit(context.Background(), store.CommitRequest{
		Statements: statements,
		Text:       text,
		NoText:     len(text) == 0,
		Message:    message,
		Author:     reviewer,
	}); err != nil {
		t.Fatalf("%s: %v", message, err)
	}
}

// parentsOf returns a commit's parents in parent-index order, which for a
// merge commit is [the branch merged into, the branch merged from].
func parentsOf(t *testing.T, st *localdolt.Store, commit string) []string {
	t.Helper()
	rows, err := st.Query(context.Background(),
		"SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? ORDER BY parent_index", commit)
	if err != nil {
		t.Fatalf("read the parents of %s: %v", commit, err)
	}
	defer func() { _ = rows.Close() }()

	var parents []string
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			t.Fatalf("scan a parent of %s: %v", commit, err)
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the parents of %s: %v", commit, err)
	}
	return parents
}

// commitRecordOf reads one commit out of main's log.
func commitRecordOf(t *testing.T, st *localdolt.Store, commit string) commitRecord {
	t.Helper()
	rows, err := st.Query(context.Background(), fmt.Sprintf(
		"SELECT commit_hash, message, committer, email FROM DOLT_LOG('%s') WHERE commit_hash = ?",
		localdolt.MainBranch), commit)
	if err != nil {
		t.Fatalf("look up commit %s: %v", commit, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("commit %s is not reachable from main (err %v)", commit, rows.Err())
	}
	var record commitRecord
	if err := rows.Scan(&record.hash, &record.message, &record.committer, &record.email); err != nil {
		t.Fatalf("scan commit %s: %v", commit, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("look up commit %s: %v", commit, err)
	}
	return record
}

// TestReviewAcceptScansTheDenyListAgain covers the gap between staging and
// promotion: PRD §11.3's deny-list is read per write, so that an edited
// rule binds the next one, and the accept is a second write. A rule added
// after an agent staged a proposal has to stop that proposal becoming
// durable — otherwise review, the lane the gate exists to police, is the
// one lane that can carry text past a rule.
func TestReviewAcceptScansTheDenyListAgain(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	payloadStaged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the deploy key rotates monthly", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "env.token", Value: "ghp_0123456789abcdefghijklmnopqrstuvwxyz"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	actorStaged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the actor is stored with the proposal", Actor: deniedProposalActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.actor", Value: "normalized"})
	if err != nil {
		t.Fatalf("propose with the actor later denied: %v", err)
	}

	// The rules arrive after the proposals were staged, which is the whole
	// point: staging saw no deny-list at all.
	writeDenyList(t, base, `ghp_[A-Za-z0-9]{36}`, deniedProposalActor.Name)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	for name, staged := range map[string]localdolt.StagedProposal{
		"proposal payload": payloadStaged,
		"proposal actor":   actorStaged,
	} {
		_, err = st.AcceptProposal(ctx, staged.ID, reviewer)
		if err == nil {
			t.Fatalf("the accept promoted the denied %s", name)
		}
		if !strings.Contains(err.Error(), "refusing to promote") || !strings.Contains(err.Error(), "deny_list.patterns[") {
			t.Fatalf("%s refusal %q does not name the deny-list rule that stopped it", name, err)
		}
	}

	// Nothing landed and nothing was thrown away: the proposal is still
	// there to reject or to fix the rule for.
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 0 {
		t.Fatalf("main carries %d facts after a refused promotion, want 0", got)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != mainCommits {
		t.Fatalf("main history has %d commits after refused promotions, want %d", got, mainCommits)
	}
	requireProposalBranches(t, st, payloadStaged.Branch, actorStaged.Branch)

	// A proposal the deny-list does not match still promotes, so the
	// refusal is the rules' and not the lane's.
	clean, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the linter is part of the contract", Actor: stagingActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose a clean fact: %v", err)
	}
	if _, err := st.AcceptProposal(ctx, clean.ID, reviewer); err != nil {
		t.Fatalf("accept a proposal the deny-list does not match: %v", err)
	}
}

// TestReviewSupersedeScanSparesUnchangedDurableProse covers one direction
// of the accept-time scan's narrowing: it matches what a proposal's commit
// adds or changes, never prose main already carries. A supersede is the
// case that forced the distinction — its UPDATE touches superseded_by
// alone, so the superseded row's key and value ride through the diff
// unchanged on both sides. Before the narrowing every scanned-column value
// on a To side went to the deny-list whatever the diff said, so a rule
// written after an earlier review had made that text durable refused an
// unrelated supersede of that row.
func TestReviewSupersedeScanSparesUnchangedDurableProse(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The durable fact predates any rule: its value was accepted by an
	// earlier review of a store with no deny-list configured.
	const supersededID = "01J0000000000000000000FACT"
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{supersededID, "aws.access_key", theSecret, "user", "2026-08-02 12:00:00"},
		}},
		Text:    []string{"aws.access_key", theSecret},
		Message: "record the durable access key",
		Author:  reviewer,
	}); err != nil {
		t.Fatalf("seed the durable fact: %v", err)
	}

	// The rule names exactly what main already carries, and nothing in the
	// replacement row or the proposal metadata.
	writeDenyList(t, base, `AKIA[0-9A-Z]{16}`)

	staged, err := st.ProposeSupersede(ctx, localdolt.Proposal{
		Rationale: "rotate the key into the secret manager",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, supersededID, localdolt.Fact{Key: "aws.access_key", Value: "read from AWS Secrets Manager"})
	if err != nil {
		t.Fatalf("propose supersede: %v", err)
	}

	// Show renders whole rows either way — including the unchanged value
	// the promotion no longer re-matches — because its job is to show the
	// operator everything the branch holds.
	diff, err := st.ProposalDiff(ctx, staged.ID)
	if err != nil {
		t.Fatalf("show a supersede under a rule matching its unchanged durable prose: %v", err)
	}
	renderedUnchanged := false
	for _, change := range diff.Changes {
		if change.Table == "facts" && change.Type == "modified" && change.To["value"] == theSecret {
			renderedUnchanged = true
		}
	}
	if !renderedUnchanged {
		t.Fatalf("show did not render the superseded row's unchanged value: %+v", diff.Changes)
	}

	// And the accept promotes: the rule binds staged prose, not history.
	if _, err := st.AcceptProposal(ctx, staged.ID, reviewer); err != nil {
		t.Fatalf("accept a supersese whose diff leaves denied durable prose unchanged: %v", err)
	}
	old := readStagedFact(t, st, localdolt.MainBranch, supersededID)
	if old.supersededBy.String != staged.RowID || old.liveKey.Valid || old.value != theSecret {
		t.Fatalf("the superseded row on main is %+v, want it linked, live-keyed off and otherwise untouched", old)
	}
	replacement := readStagedFact(t, st, localdolt.MainBranch, staged.RowID)
	if replacement.value != "read from AWS Secrets Manager" || replacement.liveKey.String != "aws.access_key" {
		t.Fatalf("the replacement row on main is %+v, want it live under the transferred key", replacement)
	}
	requireProposalBranches(t, st)
	requireOnMain(t, st)
}

// TestReviewAcceptStillRefusesDeniedStagedProse covers the other direction
// of that narrowing: a rule matching prose the proposal actually stages
// still stops the promotion, even though the same rule leaves the rows
// around it alone. It also pins that show never scans — rendering a refused
// proposal is how the operator sees what matched.
func TestReviewAcceptStillRefusesDeniedStagedProse(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const supersededID = "01J0000000000000000000FACT"
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{supersededID, "build.command", "go test ./...", "user", "2026-08-02 12:00:00"},
		}},
		Text:    []string{"build.command", "go test ./..."},
		Message: "record the durable build command",
		Author:  reviewer,
	}); err != nil {
		t.Fatalf("seed the durable fact: %v", err)
	}

	// Staged before any rule exists, so staging could not have caught this.
	staged, err := st.ProposeSupersede(ctx, localdolt.Proposal{
		Rationale: "the race lane replaced the plain one",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, supersededID, localdolt.Fact{Key: "build.command", Value: "go test -race against " + theSecret})
	if err != nil {
		t.Fatalf("propose supersede: %v", err)
	}

	writeDenyList(t, base, `AKIA[0-9A-Z]{16}`)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	_, err = st.AcceptProposal(ctx, staged.ID, reviewer)
	if !errors.Is(err, store.ErrDenied) {
		t.Fatalf("accept error = %v, want one matching store.ErrDenied", err)
	}
	if !strings.Contains(err.Error(), "refusing to promote") || strings.Contains(err.Error(), theSecret) {
		t.Fatalf("refusal %q neither names the promotion nor keeps the matched text out", err)
	}

	// Show still renders the refused proposal, and nothing landed or left:
	// the operator can look at what matched before fixing the rule.
	if _, err := st.ProposalDiff(ctx, staged.ID); err != nil {
		t.Fatalf("show the refused proposal: %v", err)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != mainCommits {
		t.Fatalf("main history has %d commits after a refused promotion, want %d", got, mainCommits)
	}
	requireProposalBranches(t, st, staged.Branch)
	if _, err := st.RejectProposal(ctx, staged.ID); err != nil {
		t.Fatalf("reject the refused proposal: %v", err)
	}
}
