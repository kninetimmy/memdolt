package localdolt_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// crockford is the ULID alphabet, repeated here so the assertions read
// against the specification rather than against the minter's own constant.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var stagingActor = store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}

// TestProposeFactStagesOneCommitOnItsOwnBranch covers PRD §3.1's branch
// model and §6.2's payload for a fact: one proposal/<ulid> branch, exactly
// one commit on it, the proposed row sourced to the staging actor, one
// proposals metadata row, and a main that has not moved.
func TestProposeFactStagesOneCommitOnItsOwnBranch(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	mainHead := headCommit(t, st)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	staged, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the soak lane changed how the suite is run",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{
		Key:      "build.command",
		Value:    "go test -tags soak ./tests/soak/...",
		Kind:     "command",
		Evidence: "CLAUDE.md:42",
	})
	if err != nil {
		t.Fatalf("propose fact: %v", err)
	}

	// The branch is named for the proposal, and both ids are client-minted
	// ULIDs (PRD §6.1).
	if want := "proposal/" + staged.ID; staged.Branch != want {
		t.Fatalf("branch = %q, want %q", staged.Branch, want)
	}
	if staged.Kind != localdolt.KindFact {
		t.Fatalf("kind = %q, want %q", staged.Kind, localdolt.KindFact)
	}
	requireULID(t, "proposal id", staged.ID)
	requireULID(t, "staged row id", staged.RowID)
	if staged.ID == staged.RowID {
		t.Fatal("the proposal and the row it stages were given the same id")
	}

	// Exactly one proposal branch (PRD §3.1: one branch per proposal).
	requireProposalBranches(t, st, staged.Branch)

	// main did not move, and holds none of the staged rows.
	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: staging moved main", got, mainHead)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != mainCommits {
		t.Fatalf("main has %d commits after staging, want %d", got, mainCommits)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 0 {
		t.Fatalf("main carries %d facts after staging, want 0", got)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM proposals"); got != 0 {
		t.Fatalf("main carries %d proposals after staging, want 0", got)
	}

	// The branch carries exactly one commit on top of main's head, and it is
	// the one the call reported, attributed to the staging actor with the
	// structured one-liner of PRD §3.1.
	if got := commitsOn(t, st, staged.Branch); got != mainCommits+1 {
		t.Fatalf("branch %s has %d commits, want %d (one on top of main)", staged.Branch, got, mainCommits+1)
	}
	if got := parentOnBranch(t, st, staged.Branch, staged.Commit); got != mainHead {
		t.Fatalf("the staged commit's parent is %q, want main's head %q", got, mainHead)
	}
	commit := headCommitOn(t, st, staged.Branch)
	if commit.hash != staged.Commit {
		t.Fatalf("branch head = %q, want the reported commit %q", commit.hash, staged.Commit)
	}
	if want := "propose fact build.command"; commit.message != want {
		t.Fatalf("commit message = %q, want %q", commit.message, want)
	}
	if commit.committer != stagingActor.Name || commit.email != stagingActor.Email {
		t.Fatalf("commit author = %q <%s>, want %q <%s>",
			commit.committer, commit.email, stagingActor.Name, stagingActor.Email)
	}

	// The staged row, as it stands on the branch.
	fact := readStagedFact(t, st, staged.Branch, staged.RowID)
	if fact.key != "build.command" || fact.value != "go test -tags soak ./tests/soak/..." {
		t.Fatalf("staged fact = %q/%q, want the proposed key and value", fact.key, fact.value)
	}
	// PRD §6.2: the proposed row records the staging actor as its source.
	if fact.source != stagingActor.Name {
		t.Fatalf("staged fact source = %q, want the staging actor %q", fact.source, stagingActor.Name)
	}
	if fact.kind.String != "command" || fact.evidence.String != "CLAUDE.md:42" {
		t.Fatalf("staged fact kind/evidence = %q/%q, want the proposed ones", fact.kind.String, fact.evidence.String)
	}
	if fact.supersededBy.Valid {
		t.Fatalf("a plain fact proposal set superseded_by = %q", fact.supersededBy.String)
	}
	if fact.liveKey.String != "build.command" {
		t.Fatalf("staged fact live_key = %q, want it live under its key", fact.liveKey.String)
	}
	requireRecent(t, "staged fact created_at", fact.createdAt)

	// And the proposals metadata row beside it (PRD §6.2).
	proposal := readProposal(t, st, staged.Branch, staged.ID)
	if proposal.kind != string(localdolt.KindFact) {
		t.Fatalf("proposals.kind = %q, want %q", proposal.kind, localdolt.KindFact)
	}
	if proposal.rationale != "the soak lane changed how the suite is run" {
		t.Fatalf("proposals.rationale = %q, want the rationale the caller gave", proposal.rationale)
	}
	if proposal.actor != stagingActor.Name {
		t.Fatalf("proposals.actor = %q, want %q", proposal.actor, stagingActor.Name)
	}
	if proposal.target != string(localdolt.TargetRepo) {
		t.Fatalf("proposals.target = %q, want %q", proposal.target, localdolt.TargetRepo)
	}
	requireRecent(t, "proposals.created_at", proposal.createdAt)
	if got := scanInt(t, st, fmt.Sprintf("SELECT COUNT(*) FROM proposals AS OF '%s'", staged.Branch)); got != 1 {
		t.Fatalf("branch carries %d proposals rows, want exactly 1", got)
	}

	// Nothing else rode along in the commit: it adds one facts row and one
	// proposals row and touches no other table (PRD §6.2).
	requireCommitAdds(t, st, mainHead, staged.Commit, map[string]int{"facts": 1, "proposals": 1})

	// A second proposal is a second branch, not a second commit on the
	// first (PRD §3.1: one branch per proposal, not per session).
	next, err := st.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "the linter is part of the contract",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err != nil {
		t.Fatalf("propose a second fact: %v", err)
	}
	requireProposalBranches(t, st, staged.Branch, next.Branch)
	for _, branch := range []string{staged.Branch, next.Branch} {
		if got := commitsOn(t, st, branch); got != mainCommits+1 {
			t.Fatalf("branch %s has %d commits, want %d", branch, got, mainCommits+1)
		}
	}
	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: a second staging moved main", got, mainHead)
	}

	// The session is back on main, and the store is usable there: the
	// connection that did the staging went back into the pool on main
	// (PRD §6.4's migration guard reads active_branch()).
	requireOnMain(t, st)
	// PRD §6.4's runner refuses to migrate off main and asks the same pool
	// where it is, so it is the caller most exposed to a session left on a
	// proposal branch. Here it is a no-op that has to succeed.
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate after staging: %v", err)
	}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "INSERT INTO meta (k, v) VALUES ('probe', 'after staging')"},
		},
		Message: "record a durable write after staging",
		Author:  store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("commit on main after staging: %v", err)
	}
	if got := commitsOn(t, st, localdolt.MainBranch); got != mainCommits+1 {
		t.Fatalf("main has %d commits after one durable write, want %d", got, mainCommits+1)
	}
}

// TestProposeDecisionStagesTheDecisionRow covers §6.2's other insert: a
// decision proposal is the same one-branch, one-commit shape, and it is the
// decisions row that carries the staging actor.
func TestProposeDecisionStagesTheDecisionRow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	mainHead := headCommit(t, st)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	staged, err := st.ProposeDecision(ctx, localdolt.Proposal{
		Rationale: "records why the embedded driver was chosen",
		Actor:     stagingActor,
		Target:    localdolt.TargetGlobal,
	}, localdolt.Decision{
		Title:                "Embed the Dolt driver rather than run a server",
		Rationale:            "a local-first CLI cannot depend on a running daemon",
		AlternativesRejected: "dolt sql-server per repository",
	})
	if err != nil {
		t.Fatalf("propose decision: %v", err)
	}
	if staged.Kind != localdolt.KindDecision {
		t.Fatalf("kind = %q, want %q", staged.Kind, localdolt.KindDecision)
	}
	requireULID(t, "proposal id", staged.ID)
	requireULID(t, "staged row id", staged.RowID)
	requireProposalBranches(t, st, staged.Branch)

	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: staging moved main", got, mainHead)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM decisions"); got != 0 {
		t.Fatalf("main carries %d decisions after staging, want 0", got)
	}
	if got := commitsOn(t, st, staged.Branch); got != mainCommits+1 {
		t.Fatalf("branch %s has %d commits, want %d", staged.Branch, got, mainCommits+1)
	}
	if want := "propose decision Embed the Dolt driver rather than run a server"; headCommitOn(t, st, staged.Branch).message != want {
		t.Fatalf("commit message = %q, want %q", headCommitOn(t, st, staged.Branch).message, want)
	}

	as := fmt.Sprintf(" AS OF '%s'", staged.Branch)
	if got := scanString(t, st,
		"SELECT title FROM decisions"+as+" WHERE id = ?", staged.RowID); got != "Embed the Dolt driver rather than run a server" {
		t.Fatalf("staged decision title = %q", got)
	}
	if got := scanString(t, st,
		"SELECT source FROM decisions"+as+" WHERE id = ?", staged.RowID); got != stagingActor.Name {
		t.Fatalf("staged decision source = %q, want the staging actor %q", got, stagingActor.Name)
	}
	// PRD §3.2: the branch is the proposal's status, so the row it carries
	// is already active and merging it is what makes it durable.
	if got := scanString(t, st,
		"SELECT status FROM decisions"+as+" WHERE id = ?", staged.RowID); got != "active" {
		t.Fatalf("staged decision status = %q, want %q", got, "active")
	}
	if got := scanInt(t, st,
		"SELECT COUNT(*) FROM decisions"+as+" WHERE id = ? AND summary IS NULL AND evidence IS NULL",
		staged.RowID); got != 1 {
		t.Fatalf("an unset summary or evidence was stored as something other than NULL")
	}
	if got := scanString(t, st,
		"SELECT alternatives_rejected FROM decisions"+as+" WHERE id = ?", staged.RowID); got != "dolt sql-server per repository" {
		t.Fatalf("staged decision alternatives_rejected = %q", got)
	}

	proposal := readProposal(t, st, staged.Branch, staged.ID)
	if proposal.kind != string(localdolt.KindDecision) {
		t.Fatalf("proposals.kind = %q, want %q", proposal.kind, localdolt.KindDecision)
	}
	if proposal.target != string(localdolt.TargetGlobal) {
		t.Fatalf("proposals.target = %q, want %q", proposal.target, localdolt.TargetGlobal)
	}
	requireOnMain(t, st)
}

// TestProposeSupersedeStagesTheLinkBeforeTheReplacement is the regression
// for PRD §6.1's ordering: uk_fact_live_key rejects a second live row under
// a key, and the check is per statement, so a supersede proposal only
// stages at all because the link is written before the replacement. Staging
// the pair in the other order fails on the insert
// (docs/spikes/m1-fact-key-uniqueness.md §4, steps C and D).
func TestProposeSupersedeStagesTheLinkBeforeTheReplacement(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	// A durable fact on main, as review would have left it.
	const supersededID = "01J0000000000000000000FACT"
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{
				supersededID, "build.command", "go test ./...", "user", "2026-08-02 12:00:00",
			},
		}},
		Message: "record the durable build command",
		Author:  store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("seed a durable fact: %v", err)
	}
	mainHead := headCommit(t, st)
	mainCommits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")

	staged, err := st.ProposeSupersede(ctx, localdolt.Proposal{
		Rationale: "the race lane replaced the plain one",
		Actor:     stagingActor,
		Target:    localdolt.TargetRepo,
	}, supersededID, localdolt.Fact{
		Key:   "build.command",
		Value: "go test -race ./...",
	})
	if err != nil {
		t.Fatalf("propose supersede against a live fact key: %v", err)
	}
	if staged.Kind != localdolt.KindSupersede {
		t.Fatalf("kind = %q, want %q", staged.Kind, localdolt.KindSupersede)
	}
	requireULID(t, "replacement row id", staged.RowID)
	requireProposalBranches(t, st, staged.Branch)

	if got := headCommit(t, st); got != mainHead {
		t.Fatalf("main head = %q, want %q: staging moved main", got, mainHead)
	}
	if got := commitsOn(t, st, staged.Branch); got != mainCommits+1 {
		t.Fatalf("branch %s has %d commits, want %d", staged.Branch, got, mainCommits+1)
	}
	if want := "propose supersede fact build.command"; headCommitOn(t, st, staged.Branch).message != want {
		t.Fatalf("commit message = %q, want %q", headCommitOn(t, st, staged.Branch).message, want)
	}

	// On the branch: the old row is linked to the new one and no longer
	// live, the replacement is live under the key, and both rows are still
	// there — supersession is a link, not a delete (PRD §3.2).
	old := readStagedFact(t, st, staged.Branch, supersededID)
	if old.supersededBy.String != staged.RowID {
		t.Fatalf("superseded row's superseded_by = %q, want the replacement %q", old.supersededBy.String, staged.RowID)
	}
	if old.liveKey.Valid {
		t.Fatalf("superseded row still carries live_key %q", old.liveKey.String)
	}
	if old.value != "go test ./..." {
		t.Fatalf("superseded row value = %q, want it untouched", old.value)
	}
	replacement := readStagedFact(t, st, staged.Branch, staged.RowID)
	if replacement.value != "go test -race ./..." || replacement.liveKey.String != "build.command" {
		t.Fatalf("replacement row = %q with live_key %q, want the new value live under the key",
			replacement.value, replacement.liveKey.String)
	}
	if replacement.source != stagingActor.Name {
		t.Fatalf("replacement source = %q, want the staging actor %q", replacement.source, stagingActor.Name)
	}
	if got := scanInt(t, st, fmt.Sprintf(
		"SELECT COUNT(*) FROM facts AS OF '%s' WHERE `key` = ?", staged.Branch), "build.command"); got != 2 {
		t.Fatalf("branch carries %d rows under the key, want 2 (the superseded row stays)", got)
	}

	// main still has only the durable row, untouched.
	if got := scanInt(t, st, "SELECT COUNT(*) FROM facts"); got != 1 {
		t.Fatalf("main carries %d facts after staging, want 1", got)
	}
	if got := scanInt(t, st,
		"SELECT COUNT(*) FROM facts WHERE id = ? AND superseded_by IS NULL", supersededID); got != 1 {
		t.Fatalf("staging superseded the row on main")
	}

	proposal := readProposal(t, st, staged.Branch, staged.ID)
	if proposal.kind != string(localdolt.KindSupersede) {
		t.Fatalf("proposals.kind = %q, want %q", proposal.kind, localdolt.KindSupersede)
	}
	// The commit adds the replacement row and the metadata row, and modifies
	// the superseded one in place; nothing else rode along.
	requireCommitAdds(t, st, mainHead, staged.Commit, map[string]int{"facts": 1, "proposals": 1})
	requireOnMain(t, st)
}

// TestProposeRefusalsLeaveNoBranchBehind covers the cleanup half of the
// lane: a staged write that cannot be completed takes its branch with it,
// because an empty proposal/<ulid> branch is indistinguishable to review
// from a pending proposal.
func TestProposeRefusalsLeaveNoBranchBehind(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{"01J0000000000000000000FACT", "build.command", "go test ./...", "user", "2026-08-02 12:00:00"},
		}},
		Message: "record the durable build command",
		Author:  store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("seed a durable fact: %v", err)
	}
	mainHead := headCommit(t, st)

	good := localdolt.Proposal{Rationale: "why", Actor: stagingActor, Target: localdolt.TargetRepo}

	for _, refusal := range []struct {
		name string
		call func() (localdolt.StagedProposal, error)
		want string
	}{
		{
			// PRD §11.1 elicits this collision (overwrite / supersede /
			// keep-both / cancel); the lane reports it rather than deciding.
			name: "a key that is already live on main",
			call: func() (localdolt.StagedProposal, error) {
				return st.ProposeFact(ctx, good, localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
			},
			want: "build.command",
		},
		{
			name: "a supersede against a fact that is not there",
			call: func() (localdolt.StagedProposal, error) {
				return st.ProposeSupersede(ctx, good, "01J000000000000000000MISS",
					localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})
			},
			want: "no live fact",
		},
		{
			name: "a proposal with no rationale for the reviewer",
			call: func() (localdolt.StagedProposal, error) {
				return st.ProposeFact(ctx,
					localdolt.Proposal{Actor: stagingActor, Target: localdolt.TargetRepo},
					localdolt.Fact{Key: "convention.style", Value: "gofmt"})
			},
			want: "rationale",
		},
		{
			name: "a proposal with no target scope",
			call: func() (localdolt.StagedProposal, error) {
				return st.ProposeFact(ctx,
					localdolt.Proposal{Rationale: "why", Actor: stagingActor},
					localdolt.Fact{Key: "convention.style", Value: "gofmt"})
			},
			want: "target",
		},
		{
			name: "a fact with no value",
			call: func() (localdolt.StagedProposal, error) {
				return st.ProposeFact(ctx, good, localdolt.Fact{Key: "convention.style"})
			},
			want: "value",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			staged, err := refusal.call()
			if err == nil {
				t.Fatalf("staging %s succeeded, as %s", refusal.name, staged.Branch)
			}
			if !strings.Contains(err.Error(), refusal.want) {
				t.Fatalf("refusal %q does not mention %q", err, refusal.want)
			}
			requireProposalBranches(t, st)
			if got := headCommit(t, st); got != mainHead {
				t.Fatalf("main head = %q, want %q: a refused proposal moved main", got, mainHead)
			}
			requireOnMain(t, st)
		})
	}
}

// TestProposeRefusesToStageOverUncommittedWork covers the guard that keeps
// a proposal's single commit honest: DOLT_COMMIT('-A') stages every table,
// so an uncommitted change sitting in the working set would be promoted
// along with the proposal on accept.
func TestProposeRefusesToStageOverUncommittedWork(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	// An autocommitted write with no Dolt commit behind it — exactly what a
	// direct-lane batch looks like before it is committed (PRD §3.1).
	execQuery(t, st, "INSERT INTO session_notes (id, actor, text, created_at) "+
		"VALUES ('01J000000000000000000NOTE', 'user', 'uncommitted', '2026-08-02 12:00:00')")
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_status"); got == 0 {
		t.Fatal("the probe write did not leave the working set dirty")
	}

	_, err := st.ProposeFact(ctx,
		localdolt.Proposal{Rationale: "why", Actor: stagingActor, Target: localdolt.TargetRepo},
		localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})
	if err == nil {
		t.Fatal("staged a proposal over uncommitted work")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("refusal %q does not say what is in the way", err)
	}
	requireProposalBranches(t, st)

	execQuery(t, st, "CALL DOLT_RESET('--hard')")
	execQuery(t, st, "CALL DOLT_CLEAN()")
	if _, err := st.ProposeFact(ctx,
		localdolt.Proposal{Rationale: "why", Actor: stagingActor, Target: localdolt.TargetRepo},
		localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."}); err != nil {
		t.Fatalf("propose fact once the working set is clean: %v", err)
	}
}

// migratedStore opens a disposable store and brings it to the schema this
// binary ships, which is the state every staged write starts from.
func migratedStore(t *testing.T) *localdolt.Store {
	t.Helper()
	st := openStore(t, baseDir(t), discardLogger())
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// requireOnMain asserts that the store's pooled connections are on main.
func requireOnMain(t *testing.T, st *localdolt.Store) {
	t.Helper()
	if got := scanString(t, st, "SELECT active_branch()"); got != localdolt.MainBranch {
		t.Fatalf("active branch = %q, want %q", got, localdolt.MainBranch)
	}
}

// requireCommitAdds asserts which tables a commit added rows to, and how
// many, so that "one commit carrying exactly the proposal" is checked
// against the diff rather than against the rows that happen to be readable.
// The revisions are formatted into the query because DOLT_DIFF_STAT takes
// them as literals; both are hashes memdolt just produced.
func requireCommitAdds(t *testing.T, st *localdolt.Store, from, to string, want map[string]int) {
	t.Helper()
	rows, err := st.Query(context.Background(), fmt.Sprintf(
		"SELECT table_name, rows_added FROM DOLT_DIFF_STAT('%s', '%s')", from, to))
	if err != nil {
		t.Fatalf("diff %s..%s: %v", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	got := map[string]int{}
	for rows.Next() {
		var table string
		var added int
		if err := rows.Scan(&table, &added); err != nil {
			t.Fatalf("scan diff stat: %v", err)
		}
		// Dolt's own internal FULLTEXT index tables (PRD §6.1's five per
		// FULLTEXT key) change along with the row that feeds them and are
		// reported here as dolt_<table>_<key>_fts_*. They are the index, not
		// part of the proposal's payload.
		if strings.HasPrefix(table, "dolt_") {
			continue
		}
		got[table] = added
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("diff %s..%s: %v", from, to, err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("the commit added %v, want %v", got, want)
	}
}

// requireProposalBranches asserts that exactly the named proposal branches
// exist beside main.
func requireProposalBranches(t *testing.T, st *localdolt.Store, want ...string) {
	t.Helper()
	slices.Sort(want)
	rows, err := st.Query(context.Background(),
		"SELECT name FROM dolt_branches WHERE name <> ? ORDER BY name", localdolt.MainBranch)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan branch: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("branches beside main = %v, want %v", got, want)
	}
}

func headCommit(t *testing.T, st *localdolt.Store) string {
	t.Helper()
	return scanString(t, st, "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1")
}

// commitsOn counts the commits reachable from branch. The revision is
// formatted into the query because DOLT_LOG takes it as a literal; it is a
// branch name memdolt minted itself, never external input.
func commitsOn(t *testing.T, st *localdolt.Store, branch string) int {
	t.Helper()
	return scanInt(t, st, fmt.Sprintf("SELECT COUNT(*) FROM DOLT_LOG('%s')", branch))
}

type commitRecord struct {
	hash      string
	message   string
	committer string
	email     string
}

func headCommitOn(t *testing.T, st *localdolt.Store, branch string) commitRecord {
	t.Helper()
	rows, err := st.Query(context.Background(), fmt.Sprintf(
		"SELECT commit_hash, message, committer, email FROM DOLT_LOG('%s') ORDER BY date DESC LIMIT 1", branch))
	if err != nil {
		t.Fatalf("log of %s: %v", branch, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("log of %s is empty (err %v)", branch, rows.Err())
	}
	var record commitRecord
	if err := rows.Scan(&record.hash, &record.message, &record.committer, &record.email); err != nil {
		t.Fatalf("scan log of %s: %v", branch, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("log of %s: %v", branch, err)
	}
	return record
}

// parentOnBranch returns the commit hash of the given commit's parent, as
// recorded on branch.
func parentOnBranch(t *testing.T, st *localdolt.Store, branch, commit string) string {
	t.Helper()
	return scanString(t, st, fmt.Sprintf(
		"SELECT parent_hash FROM dolt_commit_ancestors AS OF '%s' WHERE commit_hash = ?", branch), commit)
}

type stagedFactRow struct {
	key          string
	value        string
	source       string
	kind         sql.NullString
	evidence     sql.NullString
	supersededBy sql.NullString
	liveKey      sql.NullString
	createdAt    time.Time
}

// readStagedFact reads a facts row as it stands on a proposal branch. The
// revision is formatted into the query because AS OF takes it as a literal;
// it is memdolt's own minted branch name, never external input.
func readStagedFact(t *testing.T, st *localdolt.Store, branch, id string) stagedFactRow {
	t.Helper()
	rows, err := st.Query(context.Background(), fmt.Sprintf(
		"SELECT `key`, value, source, kind, evidence, superseded_by, live_key, created_at "+
			"FROM facts AS OF '%s' WHERE id = ?", branch), id)
	if err != nil {
		t.Fatalf("read fact %s on %s: %v", id, branch, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("fact %s is not on %s (err %v)", id, branch, rows.Err())
	}
	var row stagedFactRow
	if err := rows.Scan(&row.key, &row.value, &row.source, &row.kind,
		&row.evidence, &row.supersededBy, &row.liveKey, &row.createdAt); err != nil {
		t.Fatalf("scan fact %s on %s: %v", id, branch, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read fact %s on %s: %v", id, branch, err)
	}
	return row
}

type proposalRow struct {
	kind      string
	rationale string
	actor     string
	target    string
	createdAt time.Time
}

func readProposal(t *testing.T, st *localdolt.Store, branch, id string) proposalRow {
	t.Helper()
	rows, err := st.Query(context.Background(), fmt.Sprintf(
		"SELECT kind, rationale, actor, target, created_at FROM proposals AS OF '%s' WHERE id = ?",
		branch), id)
	if err != nil {
		t.Fatalf("read proposal %s on %s: %v", id, branch, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("proposal %s is not on %s (err %v)", id, branch, rows.Err())
	}
	var row proposalRow
	if err := rows.Scan(&row.kind, &row.rationale, &row.actor, &row.target, &row.createdAt); err != nil {
		t.Fatalf("scan proposal %s on %s: %v", id, branch, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read proposal %s on %s: %v", id, branch, err)
	}
	return row
}

// requireULID checks that id is what PRD §6.1's CHAR(26) columns are for: a
// client-minted ULID, 26 Crockford base32 characters whose leading 48 bits
// are the mint time in Unix milliseconds.
func requireULID(t *testing.T, label, id string) {
	t.Helper()
	if len(id) != 26 {
		t.Fatalf("%s %q is %d characters, want 26", label, id, len(id))
	}
	var timestamp int64
	for i, char := range id {
		digit := strings.IndexRune(crockford, char)
		if digit < 0 {
			t.Fatalf("%s %q has %q at %d, which is not in Crockford's base32 alphabet", label, id, char, i)
		}
		if i < 10 {
			timestamp = timestamp<<5 | int64(digit)
		}
	}
	minted := time.UnixMilli(timestamp)
	if minted.Before(time.Now().Add(-time.Hour)) || minted.After(time.Now().Add(time.Minute)) {
		t.Fatalf("%s %q decodes to %s, which is not around now", label, id, minted.UTC())
	}
}

// requireRecent checks a DATETIME column that memdolt stamped itself. The
// columns carry no zone and memdolt writes UTC (PRD §6.1).
func requireRecent(t *testing.T, label string, stamped time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if stamped.Before(now.Add(-time.Hour)) || stamped.After(now.Add(time.Minute)) {
		t.Fatalf("%s = %s, want a timestamp around now (%s)", label, stamped, now)
	}
}
