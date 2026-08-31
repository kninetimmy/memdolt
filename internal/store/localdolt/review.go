package localdolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

// This file is the review gate of PRD §7: agents propose on a branch
// (propose.go) and a human promotes by merging it, which is the only path
// into durable truth in the reviewed lane. Accept is where PRD §6.3's
// fail-closed merge protocol lives.
//
// Before M3, the accept-time contradiction probe of §7.4 was deliberately not
// here because it needed §8.3's cross-encoder. After M2 shipped that model,
// AcceptProposal runs the probe before its merge transaction can change main.
// The restriction binds AcceptProposal alone, not every review verb: list,
// show, reject, expire and stale do not promote durable claims and do not run
// inference.

// reviewedLaneTables are the tables an accept may merge, mapped to the
// column that identifies a row in them. A proposal's single commit writes
// the proposed facts or decisions row and its proposals metadata row
// (PRD §6.2) and nothing else, so a conflict or constraint violation
// reported against any other table is one this lane cannot attribute and
// refuses.
//
// The map is also the allow-list that makes the per-table system-table
// names safe to build: dolt_conflicts_<table> and
// dolt_constraint_violations_<table> cannot be parameterized, so the table
// name is looked up here before it is ever formatted into SQL, and a name
// Dolt reports that is not a key of this map is refused rather than
// interpolated.
//
// This allow-list is hand-maintained and binds reviewedLaneTables alone, not
// every table with an id column. A new reviewed-lane table must be added here
// explicitly or accept will fail closed when Dolt reports it.
var reviewedLaneTables = map[string]string{
	"facts":     "id",
	"decisions": "id",
	"proposals": "id",
}

// scannedColumns are the columns of each reviewed-lane table that supply
// the distinct caller-originated strings the repository's deny-list is
// matched against when a proposal is promoted (PRD §11.3).
//
// They are the same columns the staged-write lane declares as
// store.CommitRequest.Text — Fact.text, Decision.text, and the proposal's
// rationale and actor in propose.go — and they have to stay in step with
// it: a column added there and forgotten here would be scanned when a
// proposal is staged and not when it is made durable. The actor is also
// stored as the proposed fact or decision's source; proposals.actor is the
// one scan source for that duplicated value.
//
// The two scans over these columns differ in scope. Staging matches every
// declared value whole, because at that moment none of it exists anywhere
// else. The accept-time promotion matches only what the staging commit adds
// or changes: a supersede rewrites superseded_by on a row whose key, value,
// kind and evidence are already durable under earlier reviews, and those
// cells stopped being scanned at accept when the narrowing landed — see
// proposalText for the before-and-after.
var scannedColumns = map[string][]string{
	"facts":     {"key", "value", "kind", "evidence"},
	"decisions": {"title", "rationale", "summary", "alternatives_rejected", "evidence"},
	"proposals": {"rationale", "actor"},
}

// clearableViolation is the one constraint-violation class PRD §6.3 admits
// clearing, and it is admitted only against evidence: the unique index the
// record names is re-checked over the merged rows, and the record is
// removed only where that check comes back clean. Any other violation_type
// Dolt reports blocks the merge.
const clearableViolation = "unique index"

// contradictionThreshold is the fixed cross-encoder relevance logit at or
// above which PRD §7.4 blocks an ordinary fact or decision accept. It is not a
// retrieval-config knob: only a supersede proposal or AcceptOptions.Force may
// bypass it.
const contradictionThreshold = float32(2.0)

// crockfordAlphabet is ULID's base32 alphabet (PRD §6.1's CHAR(26) keys).
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PendingProposal is one proposal awaiting review: an unmerged branch that
// carries the §6.2 metadata row staged on it. A branch left behind after its
// head reached main is cleanup residue, not another pending review.
type PendingProposal struct {
	// ID is the proposal's ULID, which is also its branch's name.
	ID string `json:"id"`

	// Kind is which staged-write tool produced it.
	Kind ProposalKind `json:"kind"`

	// Target is the scope accepting it would promote into.
	Target ProposalTarget `json:"target"`

	// Actor is the agent that staged it, normalized per PRD §3.1.
	Actor string `json:"actor"`

	// Rationale is the note to the reviewer.
	Rationale string `json:"rationale"`

	// CreatedAt is when the proposal was staged, as the metadata row
	// records it.
	CreatedAt time.Time `json:"created_at"`

	// Branch is ProposalBranch(ID).
	Branch string `json:"branch"`

	// Commit is the branch's head, and on a branch memdolt staged it is the
	// branch's single commit (PRD §3.1). It is the commit review renders,
	// the commit the deny-list is matched against, and the commit accept
	// merges: requireOneCommit refuses an accept where those three would not
	// be the whole branch. Before that guard, this field described what
	// stage() produces; it is now also what accept requires.
	Commit string `json:"commit"`

	// StagedAt is that commit's date, which is what expiry and the stale
	// audit measure a proposal's age against: it is the branch's own clock
	// rather than a column an agent supplied.
	StagedAt time.Time `json:"staged_at"`

	// Message is the staging commit's structured one-liner (PRD §3.1).
	Message string `json:"message"`
}

// Age reports how long the proposal has been sitting, measured from its
// staging commit.
func (p PendingProposal) Age(now time.Time) time.Duration { return now.Sub(p.StagedAt) }

// ProposalChange is one row a proposal's commit adds, modifies or removes.
type ProposalChange struct {
	// Table is the reviewed-lane table the row belongs to.
	Table string `json:"table"`

	// Type is Dolt's diff_type: added, modified or removed.
	Type string `json:"type"`

	// From is the row before the commit; empty for an added row.
	From map[string]string `json:"from,omitempty"`

	// To is the row after; empty for a removed row.
	To map[string]string `json:"to,omitempty"`
}

// ProposalDiff is what `review show` renders: the proposal and the single
// commit that is its whole payload (PRD §3.1 — one branch per proposal is
// what makes this a single-commit diff rather than a range).
//
// Show renders the branch's head commit against that commit's first parent,
// whatever else the branch carries. On a branch memdolt staged, the head is
// the whole branch and the two are the same thing. On a hand-crafted branch
// they are not, and this renders the head commit alone — a partial view of
// the branch. That is deliberate: the refusal belongs to accept, which is
// where a partial view would become durable truth (requireOneCommit), and
// showing a branch review cannot promote is still how an operator finds out
// what is on it before rejecting it.
type ProposalDiff struct {
	Proposal PendingProposal  `json:"proposal"`
	Parent   string           `json:"parent"`
	Changes  []ProposalChange `json:"changes"`
}

// ClearedViolation records a constraint-violation record that verification
// showed was already satisfied and that accept therefore removed. PRD §6.3
// requires that this be logged rather than raised to the operator, so it is
// carried out of the accept for the caller to report and is written to the
// store's log.
type ClearedViolation struct {
	// Table is the table the records sat on.
	Table string `json:"table"`

	// Constraint is the unique index the records named.
	Constraint string `json:"constraint"`

	// Rows are the identifiers of the records removed.
	Rows []string `json:"rows"`
}

// AcceptResult describes a promotion that landed.
type AcceptResult struct {
	// Proposal is the proposal as it stood before the merge.
	Proposal PendingProposal `json:"proposal"`

	// Commit is the merge commit on main. It has two parents — main's
	// previous head and the proposal's staging commit — so the whole
	// propose-review-merge cycle is auditable from dolt_log alone.
	Commit string `json:"commit"`

	// Cleared lists the already-satisfied constraint-violation records the
	// merge protocol removed, if any.
	Cleared []ClearedViolation `json:"cleared_violations,omitempty"`
}

// ContradictionScorer is the shipped cross-encoder surface AcceptProposal
// consumes. Close belongs to the gate: a scorer that cannot be opened, used or
// closed fails before the proposal is merged.
type ContradictionScorer interface {
	Rerank(query, passage string) (float32, error)
	Close() error
}

// AcceptOptions are the contradiction-guard inputs for one promotion.
//
// These requirements bind AcceptProposal alone, not every Store method or
// every review verb. Every non-forced fact or decision accept must validate
// model configuration and, when durable same-kind rows exist, open a scorer;
// nil callbacks fail closed. Supersede proposals and explicit Force accepts
// deliberately skip both callbacks and still use the same merge flow.
type AcceptOptions struct {
	Force bool
	// ExpectedCommit, when non-empty, is the staging commit an elicited
	// reviewer saw. AcceptProposal compares it with the branch head inside its
	// proposal-mutation critical section before any merge work starts and
	// retains the merged branch as cleanup residue instead of deleting it.
	ExpectedCommit              string
	ValidateContradictionConfig func() error
	OpenContradictionScorer     func(context.Context) (ContradictionScorer, error)
}

// ErrContradiction is matched by an accept refused at PRD §7.4's fixed
// cross-encoder threshold.
var ErrContradiction = errors.New("proposal contradicts durable reviewed memory")

// ContradictionError names the durable row whose cross-encoder score blocked
// a proposal.
type ContradictionError struct {
	ProposalID string
	SourceType string
	SourceID   string
	Label      string
	Score      float32
}

func (e *ContradictionError) Error() string {
	return fmt.Sprintf(
		"accept blocked: proposal %s looks like it contradicts durable %s %s (cross-encoder score %.3f >= %.3f); "+
			"accept a supersede proposal where that lane applies, or rerun `memdolt review accept %s --force` to keep both",
		e.ProposalID, e.SourceType, e.Label, e.Score, contradictionThreshold, e.ProposalID)
}

func (e *ContradictionError) Unwrap() error { return ErrContradiction }

// PendingProposals lists every unmerged proposal awaiting review, oldest branch
// name first. Branch existence remains the lifecycle record, but a branch whose
// current head is already reachable from main is cleanup residue from a landed
// accept and is not offered for approval again.
func (s *Store) PendingProposals(ctx context.Context) ([]PendingProposal, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	branches, err := proposalBranches(ctx, db)
	if err != nil {
		return nil, err
	}
	pending := make([]PendingProposal, 0, len(branches))
	for _, branch := range branches {
		merged, err := commitMergedIntoMain(ctx, db, branch.commit)
		if err != nil {
			return nil, err
		}
		if merged {
			continue
		}
		proposal, err := hydrate(ctx, db, branch)
		if err != nil {
			return nil, err
		}
		pending = append(pending, proposal)
	}
	return pending, nil
}

// ProposalDiff renders one pending proposal as the single-commit diff of
// its branch against the main it was cut from.
func (s *Store) ProposalDiff(ctx context.Context, id string) (ProposalDiff, error) {
	db, err := s.handle()
	if err != nil {
		return ProposalDiff{}, err
	}
	branch, err := proposalBranchOf(id)
	if err != nil {
		return ProposalDiff{}, err
	}
	record, err := findProposalBranch(ctx, db, branch)
	if err != nil {
		return ProposalDiff{}, err
	}
	proposal, err := hydrate(ctx, db, record)
	if err != nil {
		return ProposalDiff{}, err
	}

	parent, err := commitParent(ctx, db, record)
	if err != nil {
		return ProposalDiff{}, err
	}

	diff := ProposalDiff{Proposal: proposal, Parent: parent}
	for _, table := range sortedReviewedTables() {
		changes, err := tableChanges(ctx, db, table, parent, record.commit)
		if err != nil {
			return ProposalDiff{}, err
		}
		diff.Changes = append(diff.Changes, changes...)
	}
	return diff, nil
}

// RejectProposal deletes a proposal's unmerged branch, leaving main exactly
// where it was (PRD §3.1). Rejection discards one operator-named proposal;
// expiry is the other discard path and sweeps old proposal branches by age.
func (s *Store) RejectProposal(ctx context.Context, id string) (PendingProposal, error) {
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()

	db, err := s.handle()
	if err != nil {
		return PendingProposal{}, err
	}
	branch, err := proposalBranchOf(id)
	if err != nil {
		return PendingProposal{}, err
	}
	record, err := findProposalBranch(ctx, db, branch)
	if err != nil {
		return PendingProposal{}, err
	}
	// Read the metadata before the branch that carries it is gone, so the
	// rejection can be reported as something more than an id.
	proposal, err := hydrate(ctx, db, record)
	if err != nil {
		return PendingProposal{}, err
	}
	if err := deleteProposalBranch(ctx, db, record, "rejected"); err != nil {
		return PendingProposal{}, fmt.Errorf("localdolt: delete the rejected proposal branch %q: %w", branch, err)
	}
	return proposal, nil
}

// ExpireProposals deletes every proposal branch whose staging commit is
// older than before, discarding its unmerged data, and reports what it removed
// (PRD §12: expiry is a branch age sweep). It reads the branch list rather
// than the proposals metadata, so a branch whose metadata row cannot be read
// is swept rather than left behind forever.
func (s *Store) ExpireProposals(ctx context.Context, before time.Time) ([]PendingProposal, error) {
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()

	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	branches, err := proposalBranches(ctx, db)
	if err != nil {
		return nil, err
	}

	expired := make([]PendingProposal, 0)
	for _, branch := range branches {
		if !branch.stagedAt.Before(before) {
			continue
		}
		// Best-effort metadata: an unreadable row must not stop the sweep,
		// because a branch review cannot describe is exactly one expiry is
		// for. The branch facts are always reported.
		proposal, err := hydrate(ctx, db, branch)
		if err != nil {
			proposal = branch.bare()
		}
		if err := deleteProposalBranch(ctx, db, branch, "expired"); err != nil {
			return expired, fmt.Errorf("localdolt: delete the expired proposal branch %q: %w", branch.name, err)
		}
		expired = append(expired, proposal)
	}
	return expired, nil
}

// AcceptProposal promotes one proposal into durable truth: it merges exactly
// the one commit that proposal was staged with into main (PRD §3.1, §7.1).
// CLI acceptance then deletes the branch. Expected-commit acceptance retains
// it as cleanup residue because Dolt has no expected-head branch deletion.
//
// "Exactly one commit" is checked rather than assumed, and the commit is
// named by hash rather than by branch. Before this the accept merged the
// branch by name and trusted it to hold one commit, which is true of every
// branch stage() produces and of nothing else: a two-commit branch was
// rendered and deny-list-scanned as its head commit alone and merged whole.
// requireOneCommit below is that check, and doltMerge merges
// proposal.Commit — the same commit the scan read.
//
// The merge runs under PRD §6.3's fail-closed protocol. It is a --no-ff
// merge, so the review action always gets a commit of its own even when
// main has not moved and Dolt would otherwise fast-forward — that commit,
// carrying the reviewer as its author, is what makes the whole
// propose-review-merge cycle auditable from dolt_log alone.
//
// The merge and everything that follows it run inside one transaction, and
// that is load-bearing rather than tidy. Measured against the embedded
// driver (github.com/dolthub/driver v1.88.1): under autocommit, a merge
// that produces conflicts or constraint violations is rolled back whole
// before the caller can look at it — dolt_constraint_violations_<table>
// comes back empty and there is no merge to abort — so the "stop before
// commit and query both per-table surfaces" of §6.3 is only reachable from
// inside an explicit transaction. Rolling that transaction back is
// therefore also the abort: main does not move, the working set is clean,
// both surfaces are empty and the proposal branch is untouched.
func (s *Store) AcceptProposal(ctx context.Context, id string, reviewer store.Actor, options AcceptOptions) (AcceptResult, error) {
	return s.acceptProposal(ctx, id, reviewer, options, acceptHooks{})
}

type acceptHooks struct {
	afterExpectedCommit func()
	beforeCleanup       func()
	cleanupError        func() error
}

func (s *Store) acceptProposal(
	ctx context.Context,
	id string,
	reviewer store.Actor,
	options AcceptOptions,
	hooks acceptHooks,
) (AcceptResult, error) {
	// One Store owns proposal-branch mutations. Accept, reject, expiry, and
	// memdolt staging share this boundary so none can invalidate another's
	// observed branch between validation and mutation.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()

	db, err := s.handle()
	if err != nil {
		return AcceptResult{}, err
	}
	if err := reviewer.Validate(); err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: accept: reviewer: %w", err)
	}
	branch, err := proposalBranchOf(id)
	if err != nil {
		return AcceptResult{}, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Accept merges into main, and this connection is the one that will do
	// it. A session parked on another branch would merge the proposal
	// somewhere that is not durable truth, so it is refused rather than
	// checked out from under whoever left it there — the same rule the
	// migration runner applies (PRD §6.4).
	active, err := activeBranch(ctx, conn)
	if err != nil {
		return AcceptResult{}, err
	}
	if active != MainBranch {
		return AcceptResult{}, fmt.Errorf(
			"localdolt: accept merges into %q (PRD §7.1); this session is on %q", MainBranch, active)
	}
	// DOLT_COMMIT('-A') concludes the merge by staging every table, so
	// anything already sitting in the working set would be promoted along
	// with the proposal.
	if err := requireCleanWorkingSet(ctx, conn, "promote a proposal"); err != nil {
		return AcceptResult{}, err
	}

	record, err := findProposalBranch(ctx, conn, branch)
	if err != nil {
		return AcceptResult{}, err
	}
	proposal, err := hydrate(ctx, conn, record)
	if err != nil {
		return AcceptResult{}, err
	}
	if options.ExpectedCommit != "" && proposal.Commit != options.ExpectedCommit {
		return AcceptResult{}, fmt.Errorf(
			"localdolt: proposal %s changed after review showed commit %s; current commit is %s; nothing was promoted or removed",
			id, options.ExpectedCommit, proposal.Commit)
	}
	if hooks.afterExpectedCommit != nil {
		hooks.afterExpectedCommit()
	}
	// PRD §10 promotes a global proposal by copying it into the global
	// database, which does not exist yet. Merging it into this repository's
	// main instead would put the row in the wrong store under the name of a
	// review that approved something else, so it is refused. Listing,
	// showing, rejecting and expiring a global proposal all still work.
	if proposal.Target == TargetGlobal {
		return AcceptResult{}, fmt.Errorf(
			"localdolt: proposal %s targets the global store, which memdolt does not have yet (PRD §10); "+
				"accepting it here would promote it into this repository instead", id)
	}

	// Everything below is about one commit: the one the proposal was staged
	// with. This is what establishes that the branch holds nothing else, and
	// it runs before the scan rather than after it because the scan reads
	// that commit's diff and would otherwise report on part of a branch.
	if err := requireOneCommit(ctx, conn, proposal); err != nil {
		return AcceptResult{}, err
	}

	// The deny-list runs again here, over the prose the branch is about to
	// make durable (PRD §11.3). It ran once already when the proposal was
	// staged, but the accept is a second write, on a second day, against a
	// config file localdolt.checkDenyList deliberately reads per write so
	// that an edited rule binds the next one. A rule added after an agent
	// staged something would otherwise be silently skipped by exactly the
	// lane the review gate exists to police.
	//
	// What binds here is what that commit writes: proposalText reads the
	// commit's diff and takes only added or changed values from the scanned
	// columns, so an edited rule does not reach back past text main has
	// carried since an earlier review accepted it.
	text, err := s.proposalText(ctx, conn, record)
	if err != nil {
		return AcceptResult{}, err
	}
	if err := s.checkDenyList(text); err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: refusing to promote proposal %s: %w", id, err)
	}
	if proposal.Kind == KindSupersede {
		if err := requireSupersedeShape(ctx, conn, record); err != nil {
			return AcceptResult{}, fmt.Errorf("localdolt: refusing contradiction bypass for proposal %s: %w", id, err)
		}
	}
	probe := proposal.Kind != KindSupersede && !options.Force
	if probe {
		if options.ValidateContradictionConfig == nil {
			return AcceptResult{}, fmt.Errorf("localdolt: refusing to promote proposal %s: contradiction probe configuration validator is unavailable", id)
		}
		if err := options.ValidateContradictionConfig(); err != nil {
			return AcceptResult{}, fmt.Errorf("localdolt: refusing to promote proposal %s: contradiction probe configuration: %w", id, err)
		}
	}

	result, err := s.acceptInTx(ctx, conn, record, proposal, reviewer, options, probe)
	if err != nil {
		return AcceptResult{}, err
	}
	if hooks.beforeCleanup != nil {
		hooks.beforeCleanup()
	}

	// DOLT_BRANCH -D accepts only a branch name. A read of dolt_branches
	// followed by that call cannot atomically prove that a foreign session did
	// not move the branch in between. Expected-commit acceptance is the MCP
	// path, so preserve its merged branch as hidden cleanup residue rather than
	// risk deleting content created after the human reviewed it.
	if options.ExpectedCommit != "" {
		return result, nil
	}

	// The CLI contract still attempts cleanup after the merge has landed, never
	// before: a deletion that ran first would discard the proposal if the merge
	// then failed.
	var cleanupErr error
	if hooks.cleanupError != nil {
		cleanupErr = hooks.cleanupError()
	} else {
		cleanupErr = deleteProposalBranch(ctx, conn, record, "accepted")
	}
	if cleanupErr != nil {
		return result, fmt.Errorf(
			"localdolt: proposal %s was merged as %s but its branch %q could not be deleted: %w",
			id, result.Commit, branch, cleanupErr)
	}
	return result, nil
}

// proposalText collects the prose a proposal's commit would make durable,
// read out of the branch's own diff rather than out of the merged working
// set, so that it is the proposal's rows that are scanned and not whatever
// else happens to be on main.
//
// It scans exactly what the commit adds or changes, and nothing it leaves
// alone. Before the supersede lane made this distinction matter, every To
// value of a scanned column went to the deny-list whatever the diff said;
// that scanned unchanged durable text too, so a rule written after a fact
// became durable refused an unrelated supersede of that fact. Now an added
// row is scanned whole (it has no From side to compare against), and a
// modified row contributes only columns whose value moved — a supersede's
// UPDATE touches superseded_by alone, so the row's key, value, kind and
// evidence are already-durable prose no rule re-matches here. A removed row
// has no To side and never contributed.
func (s *Store) proposalText(ctx context.Context, q branchQuerier, record branchRecord) ([]string, error) {
	parent, err := commitParent(ctx, q, record)
	if err != nil {
		return nil, err
	}
	var text []string
	for _, table := range sortedReviewedTables() {
		changes, err := tableChanges(ctx, q, table, parent, record.commit)
		if err != nil {
			return nil, err
		}
		for _, change := range changes {
			for _, column := range scannedColumns[table] {
				value, ok := change.To[column]
				if !ok {
					continue
				}
				if before, hadBefore := change.From[column]; hadBefore && before == value {
					continue
				}
				text = append(text, value)
			}
		}
	}
	return text, nil
}

// acceptInTx runs the merge and PRD §6.3's protocol in one transaction,
// which either concludes with the merge commit or leaves main untouched.
func (s *Store) acceptInTx(
	ctx context.Context,
	conn *sql.Conn,
	record branchRecord,
	proposal PendingProposal,
	reviewer store.Actor,
	options AcceptOptions,
	probe bool,
) (AcceptResult, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: begin the accept transaction: %w", err)
	}

	if probe {
		err = requireNoContradiction(ctx, tx, record, proposal, options.OpenContradictionScorer)
	}
	var result AcceptResult
	if err == nil {
		result, err = s.merge(ctx, tx, proposal, reviewer)
	}
	if err != nil {
		// Rolling back is the abort: nothing is auto-resolved, main does
		// not move, and the proposal branch and every row on both sides
		// survive for the operator to look at.
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return AcceptResult{}, errors.Join(err,
				fmt.Errorf("localdolt: roll the blocked merge back: %w", rollbackErr))
		}
		return AcceptResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: conclude the accept transaction: %w", err)
	}
	return result, nil
}

type contradictionClaim struct {
	sourceType string
	text       string
}

type contradictionCandidate struct {
	sourceType string
	sourceID   string
	text       string
	label      string
}

func requireNoContradiction(
	ctx context.Context,
	tx *sql.Tx,
	record branchRecord,
	proposal PendingProposal,
	open func(context.Context) (ContradictionScorer, error),
) error {
	claims, err := proposalContradictionClaims(ctx, tx, record)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		return fmt.Errorf("localdolt: proposal %s is a %s proposal but its staging commit carries no fact or decision prose to check", proposal.ID, proposal.Kind)
	}

	candidates := map[string][]contradictionCandidate{}
	for _, claim := range claims {
		if _, ok := candidates[claim.sourceType]; ok {
			continue
		}
		candidates[claim.sourceType], err = durableContradictionCandidates(ctx, tx, claim.sourceType)
		if err != nil {
			return err
		}
	}
	hasCandidates := false
	for _, rows := range candidates {
		hasCandidates = hasCandidates || len(rows) > 0
	}
	if !hasCandidates {
		return nil
	}
	if open == nil {
		return fmt.Errorf("localdolt: contradiction probe model opener is unavailable")
	}
	scorer, err := open(ctx)
	if err != nil {
		return fmt.Errorf("localdolt: open the contradiction probe model: %w", err)
	}
	if scorer == nil {
		return fmt.Errorf("localdolt: open the contradiction probe model: opener returned no scorer")
	}

	var best *ContradictionError
	for _, claim := range claims {
		for _, candidate := range candidates[claim.sourceType] {
			score, scoreErr := scorer.Rerank(claim.text, candidate.text)
			if scoreErr != nil {
				err = fmt.Errorf("localdolt: score proposal %s against durable %s %s: %w",
					proposal.ID, candidate.sourceType, candidate.label, scoreErr)
				break
			}
			if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
				err = fmt.Errorf("localdolt: score proposal %s against durable %s %s: inference returned non-finite score %v",
					proposal.ID, candidate.sourceType, candidate.label, score)
				break
			}
			if score >= contradictionThreshold && (best == nil || score > best.Score) {
				best = &ContradictionError{
					ProposalID: proposal.ID,
					SourceType: candidate.sourceType,
					SourceID:   candidate.sourceID,
					Label:      candidate.label,
					Score:      score,
				}
			}
		}
		if err != nil {
			break
		}
	}
	if closeErr := scorer.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("localdolt: close the contradiction probe model: %w", closeErr))
	}
	if err != nil {
		return err
	}
	if best != nil {
		return best
	}
	return nil
}

func requireSupersedeShape(ctx context.Context, q branchQuerier, record branchRecord) error {
	parent, err := commitParent(ctx, q, record)
	if err != nil {
		return err
	}
	facts, err := tableChanges(ctx, q, "facts", parent, record.commit)
	if err != nil {
		return err
	}
	decisions, err := tableChanges(ctx, q, "decisions", parent, record.commit)
	if err != nil {
		return err
	}
	return validateSupersedeChanges(facts, decisions)
}

func validateSupersedeChanges(facts, decisions []ProposalChange) error {
	if len(decisions) != 0 {
		return fmt.Errorf("a fact supersede proposal also changes %d decision row(s)", len(decisions))
	}
	if len(facts) != 2 {
		return fmt.Errorf("a fact supersede proposal must modify its old row and add its replacement; found %d fact change(s)", len(facts))
	}
	var old, replacement *ProposalChange
	for i := range facts {
		switch facts[i].Type {
		case "modified":
			if old != nil {
				return fmt.Errorf("a fact supersede proposal modifies more than one old row")
			}
			old = &facts[i]
		case "added":
			if replacement != nil {
				return fmt.Errorf("a fact supersede proposal adds more than one replacement row")
			}
			replacement = &facts[i]
		default:
			return fmt.Errorf("a fact supersede proposal contains an unsupported %q fact change", facts[i].Type)
		}
	}
	if old == nil || replacement == nil {
		return fmt.Errorf("a fact supersede proposal must contain one modified old row and one added replacement row")
	}

	oldID, replacementID := old.From["id"], replacement.To["id"]
	if oldID == "" || replacementID == "" || old.To["id"] != oldID {
		return fmt.Errorf("a fact supersede proposal must preserve the old row id and give its replacement an id")
	}
	if old.From["superseded_by"] != "" || old.To["superseded_by"] != replacementID {
		return fmt.Errorf("the old fact %s must be live and link to replacement %s", oldID, replacementID)
	}
	key := old.From["key"]
	if key == "" || replacement.To["key"] != key || old.From["live_key"] != key || old.To["live_key"] != "" {
		return fmt.Errorf("the replacement fact must take the old live fact's key")
	}
	if replacement.To["superseded_by"] != "" || replacement.To["live_key"] != key {
		return fmt.Errorf("the replacement fact %s must be live", replacementID)
	}

	columns := map[string]struct{}{}
	for column := range old.From {
		columns[column] = struct{}{}
	}
	for column := range old.To {
		columns[column] = struct{}{}
	}
	for column := range columns {
		if column == "superseded_by" || column == "live_key" {
			continue
		}
		if old.From[column] != old.To[column] {
			return fmt.Errorf("a fact supersede proposal also changes old row column %q", column)
		}
	}
	return nil
}

func proposalContradictionClaims(ctx context.Context, q branchQuerier, record branchRecord) ([]contradictionClaim, error) {
	parent, err := commitParent(ctx, q, record)
	if err != nil {
		return nil, err
	}
	var claims []contradictionClaim
	for _, table := range []string{"facts", "decisions"} {
		changes, err := tableChanges(ctx, q, table, parent, record.commit)
		if err != nil {
			return nil, err
		}
		for _, change := range changes {
			if change.To == nil {
				continue
			}
			switch table {
			case "facts":
				claims = append(claims, contradictionClaim{
					sourceType: "fact",
					text:       factSemanticText(change.To["key"], change.To["value"]),
				})
			case "decisions":
				claims = append(claims, contradictionClaim{
					sourceType: "decision",
					text: decisionSemanticText(
						change.To["title"], change.To["rationale"], change.To["summary"]),
				})
			}
		}
	}
	return claims, nil
}

func durableContradictionCandidates(ctx context.Context, tx *sql.Tx, sourceType string) ([]contradictionCandidate, error) {
	var query string
	switch sourceType {
	case "fact":
		query = "SELECT id, COALESCE(`key`, ''), COALESCE(value, ''), '' FROM facts " +
			"WHERE superseded_by IS NULL ORDER BY id"
	case "decision":
		query = "SELECT id, COALESCE(title, ''), COALESCE(rationale, ''), COALESCE(summary, '') FROM decisions " +
			"WHERE superseded_by IS NULL AND status = 'active' ORDER BY id"
	default:
		return nil, fmt.Errorf("localdolt: unsupported contradiction source type %q", sourceType)
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("localdolt: read durable %s rows for the contradiction probe: %w", sourceType, err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []contradictionCandidate
	for rows.Next() {
		var id, first, second, third string
		if err := rows.Scan(&id, &first, &second, &third); err != nil {
			return nil, fmt.Errorf("localdolt: scan durable %s row for the contradiction probe: %w", sourceType, err)
		}
		candidate := contradictionCandidate{sourceType: sourceType, sourceID: id}
		if sourceType == "fact" {
			candidate.text = factSemanticText(first, second)
			candidate.label = fmt.Sprintf("%q (%s)", first, id)
		} else {
			candidate.text = decisionSemanticText(first, second, third)
			candidate.label = fmt.Sprintf("%q (%s)", strings.Join(strings.Fields(first), " "), id)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localdolt: read durable %s rows for the contradiction probe: %w", sourceType, err)
	}
	return candidates, nil
}

// merge is the body of PRD §6.3's protocol, inside the accept transaction.
func (s *Store) merge(ctx context.Context, tx *sql.Tx, proposal PendingProposal, reviewer store.Actor) (AcceptResult, error) {
	// Every revision this protocol resolves is either main or the proposal's
	// staging commit. The branch name is not resolved again anywhere below:
	// it would be a second lookup of something already read, and what came
	// back would not have to be what the deny-list scanned.
	base, err := mergeBase(ctx, tx, MainBranch, proposal.Commit)
	if err != nil {
		return AcceptResult{}, err
	}
	mainHead, err := branchHead(ctx, tx, MainBranch)
	if err != nil {
		return AcceptResult{}, err
	}

	if err := doltMerge(ctx, tx, proposal); err != nil {
		return AcceptResult{}, err
	}

	cleared, err := resolveOrBlock(ctx, tx, base, mainHead, proposal.Commit)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: merging %q into %q is blocked: %w",
			proposal.Branch, MainBranch, err)
	}
	// PRD §6.3: before concluding, re-query both surfaces and re-run
	// verification for every affected table. Clearing a record changes no
	// row, so this is a check that the clearing was complete rather than a
	// second chance to make it so.
	if err := requireSurfacesEmpty(ctx, tx); err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: merging %q into %q is blocked: %w",
			proposal.Branch, MainBranch, err)
	}
	for _, entry := range cleared {
		if err := requireConstraintHolds(ctx, tx, entry.Table, entry.Constraint); err != nil {
			return AcceptResult{}, fmt.Errorf("localdolt: merging %q into %q is blocked: %w",
				proposal.Branch, MainBranch, err)
		}
		s.logger.Info("cleared an already-satisfied constraint violation left by an accepted merge",
			"proposal", proposal.ID, "table", entry.Table, "constraint", entry.Constraint, "rows", entry.Rows)
	}

	message := fmt.Sprintf("review accept %s %s", proposal.Kind, proposal.ID)
	var hash string
	row := tx.QueryRowContext(ctx,
		"CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)", message, reviewer.String())
	if err := row.Scan(&hash); err != nil {
		return AcceptResult{}, fmt.Errorf("localdolt: commit the merge of %q: %w", proposal.Branch, err)
	}
	return AcceptResult{Proposal: proposal, Commit: hash, Cleared: cleared}, nil
}

// doltMerge runs the merge without concluding it. --no-ff is what gives the
// review action a commit of its own even where main has not moved; a
// fast-forward would move main onto the staging commit and leave dolt_log
// with no record that anyone reviewed anything.
//
// It merges the proposal's staging commit by hash. Naming the branch here
// instead would resolve it a second time, after requireOneCommit counted it
// and after the deny-list scanned the commit it pointed at, so a branch
// moved in between would merge rows nothing had checked — the window is
// small and the write is durable truth, which is the wrong trade. A hash
// resolves to itself. Measured on github.com/dolthub/driver v1.88.1,
// DOLT_MERGE takes a commit hash as a bound parameter exactly as it takes a
// branch name, and the merge commit's second parent is that hash
// (docs/spikes/m1-conflict-surfaces.md §8).
func doltMerge(ctx context.Context, tx *sql.Tx, proposal PendingProposal) error {
	var hash, message sql.NullString
	var fastForward, conflicts sql.NullInt64
	err := tx.QueryRowContext(ctx, "CALL DOLT_MERGE('--no-ff', '--no-commit', ?)", proposal.Commit).
		Scan(&hash, &fastForward, &conflicts, &message)
	if err != nil {
		return fmt.Errorf("localdolt: merge the staging commit %s of proposal %s into %q: %w",
			proposal.Commit, proposal.ID, MainBranch, err)
	}
	// A nonzero conflicts count is not an error here: it is the signal that
	// PRD §6.3's inspection is required, and the surfaces below are what
	// decide whether the merge may conclude.
	return nil
}

// resolveOrBlock is PRD §6.3 applied to whatever the merge left behind. It
// resolves nothing on its own: it either finds the surfaces clean, or finds
// records that constraint verification proves are already satisfied and
// removes exactly those, or refuses.
func resolveOrBlock(ctx context.Context, tx *sql.Tx, base, mainHead, branchHead string) ([]ClearedViolation, error) {
	// A data conflict is never resolved here. PRD §6.3 routes it to an
	// operator who picks base/ours/theirs, and choosing for them is the one
	// thing this lane must not do.
	conflicts, err := surfaceSummary(ctx, tx, "dolt_conflicts", "num_conflicts")
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf(
			"the merge left %s; a data conflict is resolved by an operator choosing between the "+
				"base, our and their rows (PRD §6.3), never automatically", describe(conflicts, "data conflict"))
	}

	violations, err := surfaceSummary(ctx, tx, "dolt_constraint_violations", "num_violations")
	if err != nil {
		return nil, err
	}
	var cleared []ClearedViolation
	for _, table := range slices.Sorted(maps.Keys(violations)) {
		entry, err := reviewViolations(ctx, tx, table, base, mainHead, branchHead)
		if err != nil {
			return nil, err
		}
		cleared = append(cleared, entry)
	}
	return cleared, nil
}

// reviewViolations applies PRD §6.3's constraint-violation policy to one
// table: refuse anything unknown or unattributed, re-run constraint
// verification, and clear the reviewed records only where that verification
// comes back clean.
func reviewViolations(ctx context.Context, tx *sql.Tx, table, base, mainHead, branchHead string) (ClearedViolation, error) {
	key, known := reviewedLaneTables[table]
	if !known {
		return ClearedViolation{}, fmt.Errorf(
			"the merge reported constraint violations in %q, which a proposal's commit does not write "+
				"(it writes %s); memdolt cannot attribute them",
			table, strings.Join(sortedReviewedTables(), ", "))
	}

	rows, err := readViolations(ctx, tx, table, key)
	if err != nil {
		return ClearedViolation{}, err
	}
	touched, err := rowsTouchedByMerge(ctx, tx, table, key, base, mainHead, branchHead)
	if err != nil {
		return ClearedViolation{}, err
	}
	constraint, ids, err := classifyViolations(table, rows, touched)
	if err != nil {
		return ClearedViolation{}, err
	}

	// Constraint verification, against the merged rows themselves. Dolt
	// v1.88.1 records a violation for every row of a unique index a merge
	// disturbed, including rows that no longer violate anything — which is
	// exactly what an accepted supersede does when it hands a fact key from
	// the superseded row to its replacement (docs/spikes/
	// m1-fact-key-uniqueness.md §6). Verification is what tells that case
	// apart from two machines writing the same live key, and only a clean
	// verification licenses removing a record.
	if err := requireConstraintHolds(ctx, tx, table, constraint); err != nil {
		return ClearedViolation{}, err
	}

	// The removal and any repair belong to one transaction: measured on
	// v1.88.1, a transaction that still ends with recorded violations is
	// rolled back whole (docs/spikes/m1-conflict-surfaces.md §3). This is
	// already inside the accept transaction, which is that transaction.
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	statement := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)",
		quoteIdentifier("dolt_constraint_violations_"+table), quoteIdentifier(key), placeholders)
	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
		return ClearedViolation{}, fmt.Errorf(
			"remove the reviewed constraint-violation records from %s: %w", table, err)
	}
	return ClearedViolation{Table: table, Constraint: constraint, Rows: ids}, nil
}

// violationRecord is one row of dolt_constraint_violations_<table>.
type violationRecord struct {
	// id identifies the offending row in the table itself.
	id string

	// kind is Dolt's violation_type.
	kind string

	// constraint is the index or constraint named in violation_info.
	constraint string
}

// classifyViolations is PRD §6.3's refusal of "unknown or unattributed
// rows", as a decision over records already read: every record must name a
// violation class this lane knows how to verify, every record must name the
// same constraint, and every record must belong to a row the merge actually
// touched. It reports the constraint to verify and the ids to clear once
// that verification passes.
func classifyViolations(table string, records []violationRecord, touched map[string]struct{}) (string, []string, error) {
	if len(records) == 0 {
		return "", nil, fmt.Errorf(
			"the merge reported constraint violations in %s but %s holds no rows describing them",
			table, "dolt_constraint_violations_"+table)
	}

	constraint := records[0].constraint
	ids := make([]string, 0, len(records))
	for _, record := range records {
		if record.kind != clearableViolation {
			return "", nil, fmt.Errorf(
				"the merge reported a %q violation on %s.%s; PRD §6.3 clears an already-satisfied "+
					"%q violation and nothing else",
				record.kind, table, record.id, clearableViolation)
		}
		if record.constraint != constraint {
			return "", nil, fmt.Errorf(
				"the merge reported violations of two constraints on %s (%q and %q); "+
					"memdolt verifies one constraint per table before clearing anything",
				table, constraint, record.constraint)
		}
		if _, ok := touched[record.id]; !ok {
			return "", nil, fmt.Errorf(
				"the merge reported a constraint violation on %s.%s, a row neither side of the merge "+
					"changed; memdolt cannot attribute it and will not clear it",
				table, record.id)
		}
		ids = append(ids, record.id)
	}
	slices.Sort(ids)
	return constraint, slices.Compact(ids), nil
}

// readViolations reads the per-table constraint-violation surface.
func readViolations(ctx context.Context, tx *sql.Tx, table, key string) ([]violationRecord, error) {
	// violation_info is a JSON document Dolt hands back as its own Go type
	// rather than as text, so it is cast in SQL before it crosses the
	// driver boundary.
	query := fmt.Sprintf("SELECT %s, violation_type, CAST(violation_info AS CHAR) FROM %s ORDER BY %s",
		quoteIdentifier(key), quoteIdentifier("dolt_constraint_violations_"+table), quoteIdentifier(key))
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read the constraint violations on %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var records []violationRecord
	for rows.Next() {
		var record violationRecord
		var info string
		if err := rows.Scan(&record.id, &record.kind, &info); err != nil {
			return nil, fmt.Errorf("scan a constraint violation on %s: %w", table, err)
		}
		var meta struct {
			Name    string   `json:"Name"`
			Columns []string `json:"Columns"`
		}
		if err := json.Unmarshal([]byte(info), &meta); err != nil {
			return nil, fmt.Errorf("read what constraint %s.%s violates (%q): %w", table, record.id, info, err)
		}
		if meta.Name == "" {
			return nil, fmt.Errorf("the constraint violation on %s.%s names no constraint (%q)", table, record.id, info)
		}
		record.constraint = meta.Name
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the constraint violations on %s: %w", table, err)
	}
	return records, nil
}

// requireConstraintHolds re-checks a unique index over the merged rows: it
// is the constraint verification PRD §6.3 runs before it will clear a
// violation record, and again before it concludes the merge.
//
// It asks the table rather than Dolt's own verifier. Measured against the
// embedded driver on this schema, neither form of CALL DOLT_VERIFY_
// CONSTRAINTS can answer the question: without --all it reports a
// violation for the already-satisfied supersede and the genuine same-key
// race alike, and with --all it fails outright on any table carrying a
// FULLTEXT key ("attempted to purge `dolt_facts_ft_facts_0_fts_position`
// during Full-Text merge but it could not be found") — which under PRD
// §6.1 is facts and decisions, but not proposals. The index's own
// question, asked of the merged rows, does discriminate.
func requireConstraintHolds(ctx context.Context, tx *sql.Tx, table, constraint string) error {
	columns, err := uniqueIndexColumns(ctx, tx, table, constraint)
	if err != nil {
		return err
	}

	// A unique index does not constrain rows carrying NULL in it, which is
	// the whole mechanism behind uk_fact_live_key: live_key is NULL on a
	// superseded row, so a key accumulates superseded rows while exactly
	// one stays live (PRD §6.1).
	// The values are rendered in SQL for the same reason tableChanges
	// projects its diff that way: the embedded driver panics rather than
	// erroring when it is handed a NULL ENUM, and a unique index may cover
	// any column type.
	selected := make([]string, 0, len(columns))
	grouped := make([]string, 0, len(columns))
	notNull := make([]string, 0, len(columns))
	for _, column := range columns {
		selected = append(selected, "CAST("+quoteIdentifier(column)+" AS CHAR)")
		grouped = append(grouped, quoteIdentifier(column))
		notNull = append(notNull, quoteIdentifier(column)+" IS NOT NULL")
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s GROUP BY %s HAVING COUNT(*) > 1 LIMIT 1",
		strings.Join(selected, ", "), quoteIdentifier(table),
		strings.Join(notNull, " AND "), strings.Join(grouped, ", "))

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("verify %s on %s: %w", constraint, table, err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		values := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("verify %s on %s: %w", constraint, table, err)
		}
		pairs := make([]string, 0, len(columns))
		for i, column := range columns {
			pairs = append(pairs, column+"="+values[i].String)
		}
		return fmt.Errorf(
			"constraint verification failed: %s on %s is violated by more than one row at %s, "+
				"so the reported violation is real and the merge stays blocked",
			constraint, table, strings.Join(pairs, " "))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify %s on %s: %w", constraint, table, err)
	}
	return nil
}

// uniqueIndexColumns reads the columns a named index covers, in order.
// Reading them from information_schema rather than from the JSON Dolt puts
// in violation_info is what makes them safe to build a query from: a name
// that is not a column of that table never reaches the SQL text.
func uniqueIndexColumns(ctx context.Context, tx *sql.Tx, table, constraint string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT column_name FROM information_schema.statistics "+
			"WHERE table_schema = ? AND table_name = ? AND index_name = ? AND non_unique = 0 "+
			"ORDER BY seq_in_index",
		DatabaseName, table, constraint)
	if err != nil {
		return nil, fmt.Errorf("look up the columns of %s on %s: %w", constraint, table, err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("look up the columns of %s on %s: %w", constraint, table, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("look up the columns of %s on %s: %w", constraint, table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf(
			"the merge named %q as the violated constraint on %s, but %s has no unique index by that name; "+
				"memdolt will not verify a constraint it cannot find", constraint, table, table)
	}
	return columns, nil
}

// rowsTouchedByMerge is the attribution set of PRD §6.3: the rows either
// side changed since the merge base. A violation record naming anything
// else describes a row this merge did not produce.
func rowsTouchedByMerge(ctx context.Context, tx *sql.Tx, table, key, base, mainHead, branchHead string) (map[string]struct{}, error) {
	touched := map[string]struct{}{}
	query := fmt.Sprintf("SELECT %s, %s FROM %s WHERE from_commit = ? AND to_commit = ?",
		quoteIdentifier("to_"+key), quoteIdentifier("from_"+key),
		quoteIdentifier("dolt_commit_diff_"+table))
	for _, head := range []string{mainHead, branchHead} {
		rows, err := tx.QueryContext(ctx, query, base, head)
		if err != nil {
			return nil, fmt.Errorf("diff %s over %s..%s: %w", table, base, head, err)
		}
		for rows.Next() {
			var to, from sql.NullString
			if err := rows.Scan(&to, &from); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan the %s diff over %s..%s: %w", table, base, head, err)
			}
			for _, id := range []sql.NullString{to, from} {
				if id.Valid {
					touched[id.String] = struct{}{}
				}
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, fmt.Errorf("diff %s over %s..%s: %w", table, base, head, err)
		}
	}
	return touched, nil
}

// requireSurfacesEmpty is the last gate before the merge commit: PRD §6.3
// concludes a merge only once both per-table surfaces are empty.
func requireSurfacesEmpty(ctx context.Context, tx *sql.Tx) error {
	conflicts, err := surfaceSummary(ctx, tx, "dolt_conflicts", "num_conflicts")
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("the merge still leaves %s", describe(conflicts, "data conflict"))
	}
	violations, err := surfaceSummary(ctx, tx, "dolt_constraint_violations", "num_violations")
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("the merge still leaves %s", describe(violations, "constraint violation"))
	}
	return nil
}

// surfaceSummary reads one of Dolt's two working-set summary tables as
// table name to row count.
func surfaceSummary(ctx context.Context, tx *sql.Tx, table, counter string) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf("SELECT %s, %s FROM %s", quoteIdentifier("table"), quoteIdentifier(counter), quoteIdentifier(table)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	summary := map[string]int{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		summary[name] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	return summary, nil
}

// describe renders a surface summary for an error message.
func describe(summary map[string]int, noun string) string {
	parts := make([]string, 0, len(summary))
	for _, table := range slices.Sorted(maps.Keys(summary)) {
		parts = append(parts, fmt.Sprintf("%d %s(s) in %s", summary[table], noun, table))
	}
	return strings.Join(parts, ", ")
}

// branchRecord is what dolt_branches knows about a proposal branch.
type branchRecord struct {
	name     string
	id       string
	commit   string
	stagedAt time.Time
	message  string
}

// bare describes a proposal by its branch alone, for the one caller that
// must keep going when the metadata row cannot be read.
func (b branchRecord) bare() PendingProposal {
	return PendingProposal{
		ID: b.id, Branch: b.name, Commit: b.commit, StagedAt: b.stagedAt, Message: b.message,
	}
}

// proposalBranches lists the proposal branches, ordered by name — which,
// ULIDs being lexicographically ordered by mint time, is oldest first.
func proposalBranches(ctx context.Context, q branchQuerier) ([]branchRecord, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT name, hash, latest_commit_date, latest_commit_message FROM dolt_branches ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("localdolt: list the proposal branches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var branches []branchRecord
	for rows.Next() {
		var record branchRecord
		if err := rows.Scan(&record.name, &record.commit, &record.stagedAt, &record.message); err != nil {
			return nil, fmt.Errorf("localdolt: scan a branch: %w", err)
		}
		id, ok := strings.CutPrefix(record.name, ProposalBranchPrefix)
		if !ok {
			continue
		}
		record.id = id
		branches = append(branches, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localdolt: list the proposal branches: %w", err)
	}
	return branches, nil
}

// findProposalBranch resolves one proposal branch, reporting a missing one
// as a missing proposal rather than as whatever Dolt says next.
func findProposalBranch(ctx context.Context, q branchQuerier, branch string) (branchRecord, error) {
	branches, err := proposalBranches(ctx, q)
	if err != nil {
		return branchRecord{}, err
	}
	for _, record := range branches {
		if record.name == branch {
			return record, nil
		}
	}
	return branchRecord{}, fmt.Errorf(
		"localdolt: there is no pending proposal %q because its branch %q does not exist (PRD §3.2)",
		strings.TrimPrefix(branch, ProposalBranchPrefix), branch)
}

func commitMergedIntoMain(ctx context.Context, q branchQuerier, commit string) (bool, error) {
	var base string
	if err := q.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?)", commit, MainBranch).Scan(&base); err != nil {
		return false, fmt.Errorf("localdolt: determine whether proposal commit %s is already on %q: %w", commit, MainBranch, err)
	}
	return base == commit, nil
}

// deleteProposalBranch performs the established best-effort cleanup for CLI
// review verbs and abandoned staging. The proposal mutex excludes
// memdolt-owned mutations, and the comparison catches a foreign change that
// precedes the read. Dolt exposes no expected-head delete, so a foreign change
// after the read can still race DOLT_BRANCH -D. Expected-commit acceptance
// therefore never calls this helper.
func deleteProposalBranch(ctx context.Context, q branchMutator, observed branchRecord, operation string) error {
	current, err := findProposalBranch(ctx, q, observed.name)
	if err != nil {
		return err
	}
	if current.commit != observed.commit {
		return fmt.Errorf(
			"localdolt: refusing to delete the %s proposal branch %q: its head changed from %s to %s after it was read; unseen branch content was retained",
			operation, observed.name, observed.commit, current.commit)
	}
	if _, err := q.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", observed.name); err != nil {
		return err
	}
	return nil
}

// requireOneCommit refuses to promote a proposal branch carrying anything
// but the one commit it was staged with (PRD §3.1: one branch, one commit).
//
// It is what makes the reads either side of it whole. Review renders a
// proposal as its head commit diffed against that commit's parent, and the
// deny-list re-scan reads the same diff, so on a branch carrying two commits
// both would describe the last one while the merge promoted both: rows
// nobody reviewed and prose no rule was matched against. memdolt's own
// staged-write lane cannot produce such a branch — stage() cuts from main,
// writes once and commits once, or deletes the branch — but a proposal
// branch is a branch in a data directory, and an agent with a shell and no
// owner holding the store can commit to it directly.
//
// The count is asked of the staging commit rather than of the branch name,
// for the same reason doltMerge merges that commit: it is the commit
// everything else in the accept is about. '--not main' names exactly the
// commits it carries past its merge base with main, so a proposal staged
// before main moved still counts one, and a branch cut from somewhere other
// than main counts everything back to where it really forked.
func requireOneCommit(ctx context.Context, q branchQuerier, proposal PendingProposal) error {
	var commits int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM DOLT_LOG(?, '--not', ?)", proposal.Commit, MainBranch).Scan(&commits)
	if err != nil {
		return fmt.Errorf("localdolt: count what proposal %s carries past %q: %w", proposal.ID, MainBranch, err)
	}
	if commits != 1 {
		return fmt.Errorf(
			"localdolt: refusing to promote proposal %s: its branch %q carries %d commits past its merge base "+
				"with %q, and a proposal is exactly one commit cut from %s (PRD §3.1); review shows and "+
				"deny-list-scans that one commit, so merging the branch would promote rows nobody reviewed",
			proposal.ID, proposal.Branch, commits, MainBranch, MainBranch)
	}
	return nil
}

// commitParent reports the commit a proposal branch was cut from, which is
// the other side of the single-commit diff that is the proposal's whole
// payload (PRD §3.1).
//
// A commit with one parent has one answer. A merge commit has two, and
// nothing in SQL orders the rows of dolt_commit_ancestors, so ORDER BY
// parent_index is what makes this the first parent every time rather than
// whichever row came back first — the difference between `review show`
// rendering one diff and rendering either of two. Both callers get it: the
// ordering is on this function, not on one of its call sites.
func commitParent(ctx context.Context, q branchQuerier, record branchRecord) (string, error) {
	var parent string
	err := q.QueryRowContext(ctx,
		"SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? ORDER BY parent_index LIMIT 1",
		record.commit).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("localdolt: the proposal commit %s on %q has no parent to diff against",
			record.commit, record.name)
	}
	if err != nil {
		return "", fmt.Errorf("localdolt: read the parent of proposal commit %s: %w", record.commit, err)
	}
	return parent, nil
}

// hydrate reads the PRD §6.2 metadata row a proposal branch carries.
func hydrate(ctx context.Context, q branchQuerier, record branchRecord) (PendingProposal, error) {
	// AS OF names a revision, which Dolt parses as a literal and will not
	// take as a bound parameter, so the branch name is formatted into the
	// query text. proposalBranchOf is what makes that safe, and it runs
	// here rather than only at the command line because a branch listed by
	// dolt_branches is as untrusted as an argument: anything may have put
	// a name under proposal/ in the data directory.
	branch, err := proposalBranchOf(record.id)
	if err != nil {
		return PendingProposal{}, err
	}
	if branch != record.name {
		return PendingProposal{}, fmt.Errorf(
			"localdolt: the branch %q is under %s but is not named for a proposal id", record.name, ProposalBranchPrefix)
	}
	// kind and target are ENUM columns, and the embedded driver panics
	// rather than erroring when it is handed a NULL one (see tableChanges),
	// so they are rendered in SQL and an unset one arrives as an empty
	// string the caller can see.
	query := fmt.Sprintf(
		"SELECT CAST(kind AS CHAR), rationale, actor, created_at, CAST(target AS CHAR) "+
			"FROM proposals AS OF '%s' WHERE id = ?", record.name)
	proposal := record.bare()
	var kind, target sql.NullString
	err = q.QueryRowContext(ctx, query, record.id).Scan(
		&kind, &proposal.Rationale, &proposal.Actor, &proposal.CreatedAt, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingProposal{}, fmt.Errorf(
			"localdolt: the branch %q carries no proposals row for %s; it is not a proposal memdolt staged "+
				"(PRD §6.2) and review can only expire or reject it", record.name, record.id)
	}
	if err != nil {
		return PendingProposal{}, fmt.Errorf("localdolt: read the proposal metadata on %q: %w", record.name, err)
	}
	proposal.Kind = ProposalKind(kind.String)
	proposal.Target = ProposalTarget(target.String)
	return proposal, nil
}

// tableChanges reads one table's rows out of a single commit's diff.
//
// It projects every column through CAST(... AS CHAR) rather than selecting
// them as they are. That is a workaround with a measured reason: asked for
// a NULL ENUM column, github.com/dolthub/driver v1.88.1 panics inside
// Rows.Next with "interface conversion: interface {} is nil, not uint16"
// rather than returning an error, and dolt_commit_diff_<table> is full of
// NULL enums — every from_ column of an added row, on tables like decisions
// and proposals that PRD §6.1 gives ENUM columns. Rendering in SQL keeps
// the driver off that path.
func tableChanges(ctx context.Context, q branchQuerier, table, from, to string) ([]ProposalChange, error) {
	columns, err := tableColumns(ctx, q, table)
	if err != nil {
		return nil, err
	}

	// to_<column>, from_<column>, and Dolt's own diff_type. The commit
	// columns of the diff table are deliberately left out: they describe
	// the diff, not the row.
	projected := make([]string, 0, 2*len(columns)+1)
	names := make([]string, 0, 2*len(columns)+1)
	for _, side := range []string{"to_", "from_"} {
		for _, column := range columns {
			projected = append(projected, "CAST("+quoteIdentifier(side+column)+" AS CHAR)")
			names = append(names, side+column)
		}
	}
	projected = append(projected, "diff_type")
	names = append(names, "diff_type")

	query := fmt.Sprintf("SELECT %s FROM %s WHERE from_commit = ? AND to_commit = ?",
		strings.Join(projected, ", "), quoteIdentifier("dolt_commit_diff_"+table))
	rows, err := q.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, fmt.Errorf("localdolt: diff %s over %s..%s: %w", table, from, to, err)
	}
	defer func() { _ = rows.Close() }()

	var changes []ProposalChange
	for rows.Next() {
		cells := make([]sql.NullString, len(names))
		targets := make([]any, len(names))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("localdolt: scan the %s diff: %w", table, err)
		}
		change := ProposalChange{Table: table, From: map[string]string{}, To: map[string]string{}}
		for i, name := range names {
			if !cells[i].Valid {
				continue
			}
			switch {
			case name == "diff_type":
				change.Type = cells[i].String
			case strings.HasPrefix(name, "to_"):
				change.To[strings.TrimPrefix(name, "to_")] = cells[i].String
			default:
				change.From[strings.TrimPrefix(name, "from_")] = cells[i].String
			}
		}
		if len(change.From) == 0 {
			change.From = nil
		}
		if len(change.To) == 0 {
			change.To = nil
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localdolt: diff %s over %s..%s: %w", table, from, to, err)
	}
	return changes, nil
}

// tableColumns names a table's columns in declaration order. They come
// from information_schema so that the only column names this file formats
// into SQL are ones the database itself just reported.
func tableColumns(ctx context.Context, q branchQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns "+
			"WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", DatabaseName, table)
	if err != nil {
		return nil, fmt.Errorf("localdolt: read the columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("localdolt: read the columns of %s: %w", table, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localdolt: read the columns of %s: %w", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("localdolt: the table %s has no columns; is the store migrated?", table)
	}
	return columns, nil
}

// mergeBase reports the commit two revisions diverged from.
func mergeBase(ctx context.Context, tx *sql.Tx, left, right string) (string, error) {
	var base string
	if err := tx.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?)", left, right).Scan(&base); err != nil {
		return "", fmt.Errorf("localdolt: find where %q and %q diverged: %w", left, right, err)
	}
	return base, nil
}

// branchHead reports the commit a branch points at.
func branchHead(ctx context.Context, q branchQuerier, branch string) (string, error) {
	var head string
	err := q.QueryRowContext(ctx, "SELECT hash FROM dolt_branches WHERE name = ?", branch).Scan(&head)
	if err != nil {
		return "", fmt.Errorf("localdolt: read the head of %q: %w", branch, err)
	}
	return head, nil
}

// proposalBranchOf turns a proposal id into its branch name, refusing
// anything that is not a ULID.
//
// The check is a boundary, not a formality. A proposal id arrives from the
// command line, and the branch built from it is formatted into the text of
// an AS OF clause, which Dolt parses as a literal and will not accept as a
// bound parameter. Establishing here that the id is 26 characters of
// Crockford base32 is what makes that formatting safe; every other use of
// the id stays a bound parameter.
func proposalBranchOf(id string) (string, error) {
	if len(id) != 26 {
		return "", fmt.Errorf(
			"localdolt: %q is not a proposal id: PRD §6.1 ids are 26-character ULIDs and this one is %d characters",
			id, len(id))
	}
	for i, char := range id {
		if !strings.ContainsRune(crockfordAlphabet, char) {
			return "", fmt.Errorf(
				"localdolt: %q is not a proposal id: %q at position %d is not in ULID's base32 alphabet",
				id, char, i)
		}
	}
	return ProposalBranch(id), nil
}

// sortedReviewedTables names the reviewed-lane tables in a stable order, so
// that a rendered diff and an error message do not reshuffle between runs.
func sortedReviewedTables() []string { return slices.Sorted(maps.Keys(reviewedLaneTables)) }

// branchQuerier is the read surface the review lane needs, so that it can
// ask the pool (*sql.DB) or one connection already checked out to a branch
// (*sql.Conn) the same questions.
type branchQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type branchMutator interface {
	branchQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
