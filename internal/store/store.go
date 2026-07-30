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
	// Open acquires the store and makes it usable. Opening a store that a
	// live process already owns fails with an error matching
	// errors.Is(err, ErrLocked) rather than blocking (PRD §5.2.1).
	Open(ctx context.Context) error

	// Commit applies a write and records it as one Dolt commit, returning
	// the commit hash. Everything in the request lands in a single commit
	// or none of it does.
	Commit(ctx context.Context, req CommitRequest) (CommitResult, error)

	// Query runs a read-only statement. The caller closes the returned
	// rows.
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// Close releases the store, including its single-owner lock. It is
	// idempotent.
	Close() error
}

// ErrLocked reports that another live process owns the store. It is the
// same sentinel the single-owner lock uses, re-exported so that callers
// programming against Store need not know how ownership is enforced.
var ErrLocked = singleowner.ErrLocked

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

	// Message is the commit message. PRD §3.1 wants a structured
	// one-liner: "propose fact msrv=1.24", "note batch (3)",
	// "review accept #42".
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
