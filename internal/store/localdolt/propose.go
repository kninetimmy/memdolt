package localdolt

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kninetimmy/memdolt/internal/store"
)

// ProposalBranchPrefix is the branch namespace every staged write lands in
// (PRD §3.1).
const ProposalBranchPrefix = "proposal/"

// ProposalBranch is the branch a proposal with this id is staged on. The
// branch is named for the proposals row it carries, so review moves between
// an id and its branch without a lookup table, and a proposal's status is
// the branch's existence and merge state rather than a column (PRD §3.2).
func ProposalBranch(id string) string { return ProposalBranchPrefix + id }

// ProposalKind is the kind column of PRD §6.2's proposals table: which of
// §11.1's three staged-write tools produced the proposal.
type ProposalKind string

// The staged-write kinds. They are the values proposals.kind admits.
const (
	KindFact      ProposalKind = "fact"
	KindDecision  ProposalKind = "decision"
	KindSupersede ProposalKind = "supersede"
)

// ProposalTarget is the scope a proposal is promoted into on accept
// (PRD §6.2's target column).
type ProposalTarget string

// The proposal targets. Both are stageable here; it is review that treats
// them differently, since promoting a global proposal is permanently
// CLI-only (PRD §7.2).
const (
	TargetRepo   ProposalTarget = "repo"
	TargetGlobal ProposalTarget = "global"
)

// stagedDecisionStatus is the status a staged decisions row carries.
// PRD §3.2 replaces memhub's pending-write status lifecycle with the branch
// itself — status is branch existence plus merge state — so the row is
// already 'active' on the proposal branch and merging it is what makes it
// durable. Nothing flips a status at accept time, and the enum's 'draft' is
// not this state.
const stagedDecisionStatus = "active"

// newID mints a row's primary key in the client, never in the database.
// PRD §6.1 keys every agent-writable table with a CHAR(26) ULID exactly so
// that two machines inserting between syncs cannot collide on a merge, the
// way an AUTO_INCREMENT would. internal/memory mints the direct lanes' ids
// the same way, from the operating system's cryptographic random source.
var idEntropy = &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(rand.Reader, 0)}

func newID() string { return newIDAt(time.Now()) }

func newIDAt(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), idEntropy).String()
}

// ErrFactKeyExists reports that a new fact would collide with the one live
// fact the schema permits under a key. The wrapping error names the key and
// the operator's available next steps.
var ErrFactKeyExists = errors.New("a live fact already exists under this key")

// Proposal is the review metadata a staged write records in PRD §6.2's
// proposals table.
type Proposal struct {
	// Rationale is why a reviewer should accept this proposal. On a
	// decision proposal it is not Decision.Rationale: that one is the
	// decision's own reasoning and belongs to the decisions row, which
	// outlives the review.
	Rationale string

	// Actor is the agent staging the write, normalized per PRD §3.1
	// (agent:claude-code, agent:codex, user). It authors the branch's
	// commit, fills the proposals row's actor column, and is recorded as the
	// proposed row's source (PRD §6.2).
	Actor store.Actor

	// Target is the scope the proposal is promoted into on accept.
	Target ProposalTarget
}

func (p Proposal) validate() error {
	if err := p.Actor.Validate(); err != nil {
		return fmt.Errorf("proposal actor: %w", err)
	}
	if strings.TrimSpace(p.Rationale) == "" {
		return errors.New("a proposal requires a rationale for the reviewer (PRD §6.2)")
	}
	switch p.Target {
	case TargetRepo, TargetGlobal:
	default:
		return fmt.Errorf("proposal target %q is neither %q nor %q", p.Target, TargetRepo, TargetGlobal)
	}
	return nil
}

// Fact is the facts row a proposal stages: the columns an agent supplies.
// The rest are memdolt's to fill — id is minted here, source is the staging
// actor (PRD §6.2), created_at is the staging time, and verified_at and
// superseded_by belong to lanes that run after a proposal is accepted.
type Fact struct {
	// Key is the fact's dotted namespace key (PRD §6.1: build.*,
	// convention.*, env.*, gotcha.* and similar).
	Key string

	// Value is what the fact asserts.
	Value string

	// Kind is an optional free-form classifier. Empty means NULL.
	Kind string

	// Evidence is an optional pointer — a path, file:line, commit hash, PR
	// number or URL — that the claim can later be re-verified against
	// (PRD §6.1). Empty means NULL.
	Evidence string
}

func (f Fact) validate() error {
	if strings.TrimSpace(f.Key) == "" {
		return errors.New("a fact requires a key")
	}
	if strings.TrimSpace(f.Value) == "" {
		return errors.New("a fact requires a value")
	}
	if strings.ContainsAny(f.Key, "\n\r") {
		return fmt.Errorf("fact key %q must not contain a line break; it is part of the commit message (PRD §3.1)", f.Key)
	}
	return nil
}

// text is what this fact says, for the deny-list to scan (PRD §11.3).
func (f Fact) text() []string { return []string{f.Key, f.Value, f.Kind, f.Evidence} }

// Decision is the decisions row a proposal stages: the columns an agent
// supplies. id, status, source and decided_at are memdolt's to fill.
type Decision struct {
	// Title is the choice, stated as one line.
	Title string

	// Rationale is why this choice was made — the decision's own reasoning,
	// not the note to the reviewer, which is Proposal.Rationale.
	Rationale string

	// Summary is an optional condensed form. It is deliberately outside
	// ft_decisions (PRD §6.1: summaries are rerank food, not match
	// targets). Empty means NULL.
	Summary string

	// AlternativesRejected optionally records what was considered and
	// passed over (PRD §6.1). Empty means NULL.
	AlternativesRejected string

	// Evidence is the optional re-verification pointer of PRD §6.1. Empty
	// means NULL.
	Evidence string
}

func (d Decision) validate() error {
	if strings.TrimSpace(d.Title) == "" {
		return errors.New("a decision requires a title")
	}
	if strings.TrimSpace(d.Rationale) == "" {
		return errors.New("a decision requires a rationale")
	}
	if strings.ContainsAny(d.Title, "\n\r") {
		return fmt.Errorf("decision title %q must not contain a line break; it is part of the commit message (PRD §3.1)", d.Title)
	}
	return nil
}

// text is what this decision says, for the deny-list to scan (PRD §11.3).
func (d Decision) text() []string {
	return []string{d.Title, d.Rationale, d.Summary, d.AlternativesRejected, d.Evidence}
}

// StagedProposal describes the branch a staged write created.
type StagedProposal struct {
	// ID is the proposal's ULID, the primary key of its proposals row.
	ID string

	// Branch is the branch the proposal lives on, ProposalBranch(ID).
	Branch string

	// Kind is what was staged.
	Kind ProposalKind

	// RowID is the ULID of the facts or decisions row the branch proposes.
	// For a supersede proposal it is the replacement row, which is also the
	// value written into the superseded row's superseded_by. For an overwrite
	// it is the existing row whose reviewed fields change.
	RowID string

	// Commit is the hash of the branch's single commit.
	Commit string
}

// ProposeFact stages a new fact for review (PRD §6.2).
//
// It creates one branch, proposal/<ulid>, cut from main, carrying exactly
// one commit: the facts row plus its proposals metadata row. main does not
// move, and nothing is durable until review merges the branch (PRD §7).
//
// The fact's key must not already be live on main. That collision is what
// PRD §11.1's fact-key elicitation is for — overwrite, supersede, keep-both
// under a distinct key, or cancel — so it is reported here rather than
// resolved: a staged write that quietly became something else would defeat
// the review gate. ProposeFactOverwrite and ProposeSupersede are the two
// same-key answers that stage a change.
func (s *Store) ProposeFact(ctx context.Context, p Proposal, f Fact) (StagedProposal, error) {
	if err := f.validate(); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: propose fact: %w", err)
	}
	now := time.Now().UTC()
	rowID := newID()
	return s.stage(ctx, stagedWrite{
		kind:       KindFact,
		proposal:   p,
		rowID:      rowID,
		now:        now,
		message:    "propose fact " + f.Key,
		statements: []store.Statement{insertFact(rowID, f, p.Actor, now)},
		text:       f.text(),
		newFactKey: f.Key,
	})
}

// ProposeFactOverwrite stages a reviewed in-place replacement of one live
// fact. It is the overwrite answer to PRD §11.1's fact-key conflict dialog;
// supersession remains a separate link-plus-replacement proposal.
func (s *Store) ProposeFactOverwrite(ctx context.Context, p Proposal, overwrittenID string, replacement Fact) (StagedProposal, error) {
	if strings.TrimSpace(overwrittenID) == "" {
		return StagedProposal{}, errors.New("localdolt: propose fact overwrite: the id of the fact to overwrite is required")
	}
	if err := replacement.validate(); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: propose fact overwrite: %w", err)
	}
	now := time.Now().UTC()
	return s.stage(ctx, stagedWrite{
		kind:           KindFact,
		proposal:       p,
		rowID:          overwrittenID,
		now:            now,
		message:        "propose overwrite fact " + replacement.Key,
		text:           replacement.text(),
		overwrittenID:  overwrittenID,
		overwrittenKey: replacement.Key,
		statements: []store.Statement{{
			SQL: "UPDATE facts SET value = ?, source = ?, kind = ?, evidence = ?, verified_at = NULL " +
				"WHERE id = ? AND `key` = ? AND superseded_by IS NULL",
			Args: []any{
				replacement.Value, p.Actor.Name, nullable(replacement.Kind), nullable(replacement.Evidence),
				overwrittenID, replacement.Key,
			},
		}},
	})
}

// ProposeDecision stages a new decision for review (PRD §6.2), on its own
// branch and in one commit, exactly as ProposeFact does.
func (s *Store) ProposeDecision(ctx context.Context, p Proposal, d Decision) (StagedProposal, error) {
	if err := d.validate(); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: propose decision: %w", err)
	}
	now := time.Now().UTC()
	rowID := newID()
	return s.stage(ctx, stagedWrite{
		kind:       KindDecision,
		proposal:   p,
		rowID:      rowID,
		now:        now,
		message:    "propose decision " + d.Title,
		statements: []store.Statement{insertDecision(rowID, d, p.Actor, now)},
		text:       d.text(),
	})
}

// ProposeSupersede stages a replacement for the live fact supersededID, on
// its own branch and in one commit, as the other two do.
//
// The two statements are ordered, and the order is the whole reason this
// works: the superseded row's link is written first and the replacement row
// second. facts.live_key is a generated column carrying key while a row is
// live, under a unique index, so the replacement collides with its
// predecessor until that predecessor is superseded. The check is per
// statement and a transaction does not relax it — measured on Dolt v1.88.1,
// insert-then-link fails inside one transaction while link-then-insert
// succeeds (PRD §6.1, docs/spikes/m1-fact-key-uniqueness.md §4 steps C
// and D). Superseding keeps the old row: it stays in the table and stays
// retrievable, penalized rather than deleted (PRD §3.2, §8.1).
//
// The order is not owed to the generated column in particular. A unique
// index is checked per statement whatever fills it, so any lane that hands
// a unique value from one row to another owes the same order — measured
// without a generated column at all in the same spike (§6, control 2).
//
// It supersedes facts. Decisions carry a superseded_by column too, but no
// uniqueness constraint orders their writes and no lane asks for it yet.
func (s *Store) ProposeSupersede(ctx context.Context, p Proposal, supersededID string, replacement Fact) (StagedProposal, error) {
	if strings.TrimSpace(supersededID) == "" {
		return StagedProposal{}, errors.New("localdolt: propose supersede: the id of the fact to supersede is required")
	}
	if err := replacement.validate(); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: propose supersede: %w", err)
	}
	now := time.Now().UTC()
	rowID := newID()
	return s.stage(ctx, stagedWrite{
		kind:         KindSupersede,
		proposal:     p,
		rowID:        rowID,
		now:          now,
		message:      "propose supersede fact " + replacement.Key,
		text:         replacement.text(),
		supersededID: supersededID,
		statements: []store.Statement{
			{
				SQL:  "UPDATE facts SET superseded_by = ? WHERE id = ?",
				Args: []any{rowID, supersededID},
			},
			insertFact(rowID, replacement, p.Actor, now),
		},
	})
}

// stagedWrite is one proposal's payload, assembled by the Propose* methods
// and executed by stage.
type stagedWrite struct {
	kind     ProposalKind
	proposal Proposal

	// rowID is the ULID of the facts or decisions row being proposed.
	rowID string

	// now stamps the proposed row and the proposals row, so the two agree on
	// when the proposal was made. An overwrite preserves the existing row's
	// created_at and uses now only for its proposals row.
	now time.Time

	// message is the commit's structured one-liner (PRD §3.1). It names the
	// fact key or decision title and not the value: a value is free TEXT and
	// a commit message is one line.
	message string

	// statements are the row writes, already in the order §6.1 requires.
	statements []store.Statement

	// text is what the proposed rows say, for the deny-list to match
	// against (PRD §11.3). Every caller-originated string is in it, evidence
	// pointers included, along with the proposal rationale and normalized
	// actor; a URL is as good a place to leak a token as a value is, and a
	// proposal is written by the untrusted writer of PRD §7.
	text []string

	// supersededID, when set, is the live fact the first statement links.
	// stage verifies it is live on the branch before anything is written.
	supersededID string

	// overwrittenID, when set, is the live same-key row an overwrite proposal
	// updates in place. stage verifies the id and key before writing.
	overwrittenID  string
	overwrittenKey string

	// newFactKey is set only by ProposeFact. A plain fact insert must not
	// collide with a live key; supersede deliberately transfers that key and
	// therefore does not set it.
	newFactKey string

	// afterCheckout synchronizes the cleanup regression after this write's
	// session is definitely on its proposal branch. Production writes leave it nil.
	afterCheckout func()
}

// stage is the staged-write lane of PRD §3.1: one proposal, one branch cut
// from main, one commit, and main left where it was.
//
// All of it runs on one connection taken from the pool, because a branch
// checkout is session state. The session is put back on the branch it
// started on before that connection is released, or the connection is
// thrown away — the store serves its other callers from the same pool, and
// one of them is PRD §6.4's migrations-on-main guard, which asks
// active_branch() where it is.
//
// A staging failure takes its branch with it. An empty proposal/<ulid>
// branch left behind would be indistinguishable to review from a pending
// proposal that has something in it.
func (s *Store) stage(ctx context.Context, w stagedWrite) (StagedProposal, error) {
	db, err := s.handle()
	if err != nil {
		return StagedProposal{}, err
	}
	if err := w.proposal.validate(); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: propose %s: %w", w.kind, err)
	}

	// The reviewer's rationale and normalized actor ride in the same commit
	// as the row it explains, and an agent supplied both, so the deny-list
	// scans them too.
	w.text = append(slices.Clone(w.text), w.proposal.Rationale, w.proposal.Actor.Name)

	id := newID()
	branch := ProposalBranch(id)
	statements := append(slices.Clone(w.statements), store.Statement{
		SQL: "INSERT INTO proposals (id, kind, rationale, actor, created_at, target) VALUES (?, ?, ?, ?, ?, ?)",
		Args: []any{
			id, string(w.kind), w.proposal.Rationale,
			w.proposal.Actor.Name, datetime(w.now), string(w.proposal.Target),
		},
	})

	conn, err := db.Conn(ctx)
	if err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	origin, err := activeBranch(ctx, conn)
	if err != nil {
		return StagedProposal{}, err
	}
	if err := requireCleanWorkingSet(ctx, conn, "stage a proposal"); err != nil {
		return StagedProposal{}, err
	}

	// Cut from main whatever branch the session is on: main is durable
	// truth and a proposal is a diff against it (PRD §3.1).
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH(?, ?)", branch, MainBranch); err != nil {
		return StagedProposal{}, fmt.Errorf("localdolt: create proposal branch %q from %q: %w", branch, MainBranch, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		return StagedProposal{}, errors.Join(
			fmt.Errorf("localdolt: check out proposal branch %q: %w", branch, err),
			deleteBranch(ctx, conn, branch))
	}
	if w.afterCheckout != nil {
		w.afterCheckout()
	}

	result, stageErr := s.stageOnBranch(ctx, conn, w, statements)

	if restoreErr := checkoutBranch(ctx, conn, origin); restoreErr != nil {
		// The session is stranded on the proposal branch. Whatever else
		// happened, this connection must not go back into the pool, or the
		// next caller reads and writes a proposal branch believing it is on
		// main.
		discardConn(conn)
		return StagedProposal{}, errors.Join(stageErr,
			fmt.Errorf("localdolt: the session that staged %q could not return to %q: %w", branch, origin, restoreErr),
			deleteBranchFromPool(context.WithoutCancel(ctx), db, branch))
	}
	if stageErr != nil {
		return StagedProposal{}, errors.Join(stageErr, deleteBranch(context.WithoutCancel(ctx), conn, branch))
	}

	return StagedProposal{
		ID:     id,
		Branch: branch,
		Kind:   w.kind,
		RowID:  w.rowID,
		Commit: result.Hash,
	}, nil
}

// stageOnBranch writes the proposal on the branch conn is checked out to.
func (s *Store) stageOnBranch(ctx context.Context, conn *sql.Conn, w stagedWrite, statements []store.Statement) (store.CommitResult, error) {
	if w.newFactKey != "" {
		if err := requireUnusedFactKey(ctx, conn, w.newFactKey); err != nil {
			return store.CommitResult{}, err
		}
	}
	if w.supersededID != "" {
		if err := requireLiveFact(ctx, conn, w.supersededID); err != nil {
			return store.CommitResult{}, err
		}
	}
	if w.overwrittenID != "" {
		if err := requireLiveFactWithKey(ctx, conn, w.overwrittenID, w.overwrittenKey); err != nil {
			return store.CommitResult{}, err
		}
	}
	return s.commitConn(ctx, conn, store.CommitRequest{
		Statements: statements,
		Text:       w.text,
		Message:    w.message,
		Author:     w.proposal.Actor,
	})
}

func requireUnusedFactKey(ctx context.Context, conn *sql.Conn, key string) error {
	var live int
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM facts WHERE live_key = ?", key).Scan(&live); err != nil {
		return fmt.Errorf("localdolt: look for a live fact under key %q: %w", key, err)
	}
	if live > 0 {
		return fmt.Errorf("localdolt: fact key %q: %w; choose supersede, keep both under a different key, or cancel",
			key, ErrFactKeyExists)
	}
	return nil
}

// requireCleanWorkingSet refuses an operation that would commit uncommitted work.
// DOLT_COMMIT('-A') stages every table, so anything sitting in the working
// set would ride into the proposal's one commit and be promoted with it on
// accept — the commit has to hold the proposed row and its metadata and
// nothing else (PRD §6.2).
//
// The staged-write and accept paths both call this guard because their
// DOLT_COMMIT('-A') must contain only the reviewed payload. The restriction
// binds those two call sites, not every Store.Commit: a direct-lane commit
// still owns the statements it passes to the store.
//
// The future M3 note batch is intentionally not permission to relax this
// guard. It must commit only its accumulated session_notes rows through a
// narrowed commit seam; LogNote remains the CLI's one-note/one-commit API.
func requireCleanWorkingSet(ctx context.Context, conn *sql.Conn, operation string) error {
	var dirty int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status").Scan(&dirty); err != nil {
		return fmt.Errorf("localdolt: read the working set: %w", err)
	}
	if dirty > 0 {
		return fmt.Errorf(
			"localdolt: refusing to %s over %d uncommitted table change(s); "+
				"the commit stages every table and would carry them along", operation, dirty)
	}
	return nil
}

// requireLiveFact refuses a supersede proposal whose target is missing or
// already superseded. Without it the UPDATE would match nothing and the
// commit would quietly stage an ordinary insert under the name of a
// supersede — and no database constraint would object, because PRD §6.1
// gives superseded_by no foreign key and leaves chain well-formedness to
// the application.
func requireLiveFact(ctx context.Context, conn *sql.Conn, id string) error {
	var live int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM facts WHERE id = ? AND superseded_by IS NULL", id).Scan(&live)
	if err != nil {
		return fmt.Errorf("localdolt: look up the fact %q to supersede: %w", id, err)
	}
	if live == 0 {
		return fmt.Errorf("localdolt: no live fact with id %q to supersede", id)
	}
	return nil
}

func requireLiveFactWithKey(ctx context.Context, conn *sql.Conn, id, key string) error {
	var live int
	err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM facts WHERE id = ? AND live_key = ?", id, key).Scan(&live)
	if err != nil {
		return fmt.Errorf("localdolt: look up the fact %q to overwrite: %w", id, err)
	}
	if live == 0 {
		return fmt.Errorf("localdolt: no live fact with id %q and key %q to overwrite", id, key)
	}
	return nil
}

func checkoutBranch(ctx context.Context, conn *sql.Conn, branch string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("localdolt: check out %q: %w", branch, err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
		return fmt.Errorf("localdolt: check out %q: %w", branch, err)
	}
	return nil
}

// deleteBranch removes a proposal branch that staging did not fill. -D
// rather than -d because the branch may hold a commit that main does not,
// and a rejected proposal is deleted unmerged by design (PRD §3.1).
func deleteBranch(ctx context.Context, conn *sql.Conn, branch string) error {
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", branch); err != nil {
		return fmt.Errorf("localdolt: delete the abandoned proposal branch %q: %w", branch, err)
	}
	return nil
}

// deleteBranchFromPool removes an abandoned branch through a fresh session.
// It is used when the staging session cannot return to its origin and is no
// longer trustworthy; deleting the branch from the session still checked out
// to it would fail because a branch cannot delete itself.
func deleteBranchFromPool(ctx context.Context, db *sql.DB, branch string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("localdolt: acquire a fresh session to clean up %q: %w", branch, err)
	}
	defer func() { _ = conn.Close() }()
	return deleteBranch(ctx, conn, branch)
}

// discardConn marks a connection unusable so the pool closes it instead of
// lending it out again. database/sql documents Raw as leaving the Conn
// unusable when the function it runs returns driver.ErrBadConn, which is
// the only handle a caller has on a session it can no longer trust.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// insertFact builds the facts insert of PRD §6.2: the proposed row, with
// the staging actor recorded as its source.
func insertFact(id string, f Fact, actor store.Actor, now time.Time) store.Statement {
	return store.Statement{
		SQL: "INSERT INTO facts (id, `key`, value, source, kind, evidence, created_at) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		Args: []any{id, f.Key, f.Value, actor.Name, nullable(f.Kind), nullable(f.Evidence), datetime(now)},
	}
}

// insertDecision builds the decisions insert of PRD §6.2, likewise sourced
// to the staging actor. decided_at is the staging time: it is the only time
// the client has, and the review that promotes the row is recorded in the
// merge commit rather than in a column.
func insertDecision(id string, d Decision, actor store.Actor, now time.Time) store.Statement {
	return store.Statement{
		SQL: "INSERT INTO decisions " +
			"(id, title, rationale, summary, alternatives_rejected, evidence, status, source, decided_at) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		Args: []any{
			id, d.Title, d.Rationale, nullable(d.Summary), nullable(d.AlternativesRejected),
			nullable(d.Evidence), stagedDecisionStatus, actor.Name, datetime(now),
		},
	}
}

// nullable maps an unset optional column to SQL NULL, so an absent evidence
// pointer reads as absent rather than as an empty string.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// datetime renders a timestamp for a DATETIME column. PRD §6.1's DATETIME
// columns carry no zone, so memdolt writes UTC and says so here rather than
// leaving a driver's local-time conversion to decide what a row means.
func datetime(t time.Time) string { return t.UTC().Format(time.DateTime) }
