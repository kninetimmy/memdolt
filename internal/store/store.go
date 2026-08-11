// Package store defines the storage abstraction that every memdolt
// surface routes through (PRD §5.1).
//
// One interface, two implementations: LocalStore over the embedded Dolt
// driver (topologies A and C, package store/localdolt) and, from M4,
// RemoteStore speaking MySQL to the hub (topology B). CLI and MCP server
// both go through this interface so that neither can quietly read a
// different database than the other — the r2 §7.5 lesson the PRD makes a
// birth requirement. locate and the code index bypass it by design; they
// describe the local working tree.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kninetimmy/memdolt/internal/denylist"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

// Store is the M0 subset of memdolt's storage interface.
//
// It carries exactly the four operations M0 needs to prove the embedded
// driver's single-owner concurrency model (PRD §5.2): open, close, a
// version-controlled write that produces a Dolt commit, and a read. It is
// deliberately not the shape of the finished interface.
//
// M1 extends it with the typed memory operations PRD §5.1 sketches —
// memory CRUD, proposals, review, recall gathering, doc ops and render
// reads — at which point Query's raw SQL, which exists so the M0 rig can
// interrogate dolt_log and its own probe tables, gives way to typed calls.
// Nothing outside M0 should be built against Query.
type Store interface {
	// Open acquires the store and makes it usable. It has two refusals,
	// and they bind different sets of implementations.
	//
	// Every implementation refuses a store whose schema_version is newer
	// than the migrations the binary ships, with an error matching
	// errors.Is(err, ErrSchemaTooNew), and reads or writes no memory data
	// on it. PRD §6.4 states that refusal for a clone *or* a hub, so it is
	// a contract of this interface rather than a property of the embedded
	// store: LocalStore owes it today, and the RemoteStore of §5.1 owes it
	// against a hub database the same way. CheckSchemaVersion is the shared
	// decision; only the read of meta.schema_version is per-implementation.
	//
	// ErrLocked is narrower. Opening a store that a live process already
	// owns fails with an error matching errors.Is(err, ErrLocked) rather
	// than blocking (PRD §5.2.1) — but that is the single-owner rule of an
	// implementation that owns a data directory. An implementation with no
	// data directory to own has no lock to fail on and never returns it.
	Open(ctx context.Context) error

	// Commit applies a write and records it as one Dolt commit, returning
	// the commit hash. Everything in the request lands in a single commit
	// or none of it does.
	//
	// Before any of it is applied, the request's Text is matched against
	// the repository's configured deny-list (PRD §11.3). A write that
	// matches is refused with an error matching
	// errors.Is(err, ErrDenied) that names the rule, and so is a write
	// whose deny-list is configured but cannot be evaluated; either way
	// no row and no commit is left behind, because the check runs before
	// the transaction opens. A repository with no deny-list configured is
	// unaffected.
	//
	// A request that declares neither Text nor NoText is refused before
	// any of that, so that a lane cannot skip the deny-list by omission.
	Commit(ctx context.Context, req CommitRequest) (CommitResult, error)

	// Query runs exactly one read-only SELECT or SHOW statement. Every Store
	// implementation rejects every other statement before the database
	// executes it; this is an interface-wide boundary, not a LocalStore-only
	// property. Before this boundary was enforced, the embedded implementation
	// passed raw SQL through unchanged, so a caller could leave an uncommitted
	// write for a later version-controlled commit to include. The caller closes
	// the returned rows.
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// Close releases the store, including its single-owner lock. It is
	// idempotent.
	Close() error
}

// ErrLocked reports that another live process owns the store. It is the
// same sentinel the single-owner lock uses, re-exported so that callers
// programming against Store need not know how ownership is enforced.
var ErrLocked = singleowner.ErrLocked

// ErrDenied reports a write refused by a configured deny-list rule
// (PRD §11.3), re-exported for the same reason as ErrLocked. The error it
// matches names the rule; *denylist.DeniedError carries the detail.
var ErrDenied = denylist.ErrDenied

// ErrNotOpen reports an operation on a store that is not open.
var ErrNotOpen = errors.New("store is not open")

// Actor is the normalized identity a commit is attributed to (PRD §3.1):
// agent:claude-code, agent:codex, user. Commit metadata is load-bearing —
// dolt_log and dolt_blame answer provenance queries off it with no
// application code — so memdolt sets it explicitly on every commit.
type Actor struct {
	Name  string
	Email string
}

// Validate rejects actors that cannot be rendered as Dolt commit
// authorship. Name and Email are passed to Dolt as bound parameters, never
// interpolated into SQL, but "Name <Email>" is itself a parsed format and
// must not contain its own delimiters.
func (a Actor) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("actor name is required")
	}
	if strings.TrimSpace(a.Email) == "" {
		return errors.New("actor email is required")
	}
	for label, value := range map[string]string{"name": a.Name, "email": a.Email} {
		if strings.ContainsAny(value, "<>\n\r") {
			return fmt.Errorf("actor %s %q must not contain '<', '>' or a line break", label, value)
		}
	}
	return nil
}

// String renders the actor as Dolt's author format, "Name <email>".
func (a Actor) String() string { return a.Name + " <" + a.Email + ">" }

// Statement is one SQL statement and its bound arguments. Arguments are
// always bound, never formatted into the SQL text.
type Statement struct {
	SQL  string
	Args []any
}

// CommitRequest is a version-controlled write: the statements to apply and
// the commit metadata to record them under.
type CommitRequest struct {
	// Statements are applied in order, on one connection, inside one
	// transaction.
	Statements []Statement

	// Text is the memory this write records, in the words it was given
	// in: a fact's key and value, a decision's title and rationale, a
	// task's or a note's text, a persisted actor value, a narrative body.
	// It is what the repository's configured deny-list is matched against
	// (PRD §11.3).
	//
	// It is no longer the only thing that is. Until the review lane
	// landed, this field was the deny-list's whole input, because every
	// write to main was a CommitRequest — before: one field, one seam,
	// and Validate below made forgetting it impossible. After: review
	// accept (internal/store/localdolt/review.go) promotes a proposal by
	// merging its branch, so the rows are already staged and it concludes
	// with DOLT_COMMIT and no CommitRequest at all. It scans the prose it
	// is about to make durable itself, out of the proposal branch's own
	// diff, and the columns it reads are listed in that file's
	// scannedColumns.
	//
	// So the guarantee this field carries is narrower than it reads:
	// filling it in is the obligation of every lane that records prose
	// *through a CommitRequest*, and Validate's tripwire binds this type,
	// not every write path. Leaving it empty is still not a way to opt
	// out — Validate refuses a request that declares neither text nor
	// NoText, so such a lane goes unscanned only by saying out loud that
	// it has nothing to scan — but a lane that never builds a
	// CommitRequest is outside that tripwire's reach and owes its own
	// scan.
	//
	// Nothing here is sent to the database. Statements carry the values
	// actually written; this is the same text in the one form a rule can
	// be matched against without parsing SQL.
	Text []string

	// NoText declares that this write records no memory prose, and is the
	// only way to commit without Text.
	//
	// It exists because the alternative — treating an empty Text as
	// "nothing to scan" — makes the deny-list fail open per lane: a new
	// write path that never filled Text in would pass the deny-list
	// without being evaluated, and would do it silently, at a zero value
	// nobody wrote. Requiring the declaration turns that into a refusal
	// the first time the lane runs.
	//
	// The migration runner is the case it exists for. Its commits carry
	// DDL and a schema_version row, no prose an agent supplied, and
	// scanning them would put a broken deny-list config between an
	// operator and `memdolt init` — the one command that has to work
	// before anything else can.
	NoText bool

	// Message is the commit message. PRD §3.1 wants a structured
	// one-liner: "propose fact msrv=1.24", "note batch (3)",
	// "review accept #42". It is commit metadata, not automatic deny-list
	// input. Caller-originated text copied into Message must also be
	// declared in Text.
	Message string

	// Author is attributed in dolt_log and dolt_blame.
	Author Actor
}

// Validate reports whether the request can be committed.
func (r CommitRequest) Validate() error {
	if len(r.Statements) == 0 {
		return errors.New("commit requires at least one statement")
	}
	for i, stmt := range r.Statements {
		if strings.TrimSpace(stmt.SQL) == "" {
			return fmt.Errorf("statement %d is empty", i)
		}
	}
	if len(r.Text) == 0 && !r.NoText {
		return errors.New("commit must declare the text the deny-list scans, or set NoText to declare it has none (PRD §11.3)")
	}
	if len(r.Text) > 0 && r.NoText {
		return errors.New("commit declares both Text and NoText")
	}
	if strings.TrimSpace(r.Message) == "" {
		return errors.New("commit requires a message")
	}
	if strings.ContainsAny(r.Message, "\n\r") {
		return errors.New("commit message must be a single line (PRD §3.1)")
	}
	if err := r.Author.Validate(); err != nil {
		return fmt.Errorf("commit author: %w", err)
	}
	return nil
}

// CommitResult describes the Dolt commit a write produced.
type CommitResult struct {
	// Hash is the Dolt commit hash, as it appears in dolt_log.
	Hash string

	// RowsAffected totals the rows reported by the request's statements.
	RowsAffected int64
}
