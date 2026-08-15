package localdolt

import (
	"context"
	"fmt"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

// RunOnBranch runs statements on one session checked out to branch, and puts
// that session back on main before it releases it.
//
// It exists for this package's external test, which has to build the branch
// shapes memdolt's own lanes cannot: a proposal branch carrying two commits,
// or one whose head is a merge commit. Store.Query cannot do it. Before its
// read-only boundary was enforced, the driver behavior below prevented the
// branch-spanning sequence; now Query also rejects the required writes before
// they reach the driver. Measured against github.com/dolthub/driver v1.88.1,
// the driver never reuses a connection — DoltConn.IsValid reports false and
// DoltConn.ResetSession
// returns driver.ErrBadConn, "it's simpler to just throw the session away" —
// so every pooled statement gets a fresh session on main, a CALL
// DOLT_CHECKOUT cannot span two of them, and a branch-qualified write
// (INSERT INTO `memory/proposal/<id>`.facts …) is discarded with the session
// that made it rather than landing in that branch's working set. The lanes
// that write off main hold their own connection for exactly this reason; see
// stage.
//
// It is declared in a _test.go file, so it is compiled into this package's
// test binary and into nothing that ships.
func (s *Store) RunOnBranch(ctx context.Context, branch string, statements ...store.Statement) error {
	db, err := s.handle()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("localdolt: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := checkoutBranch(ctx, conn, branch); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			discardConn(conn)
			return fmt.Errorf("localdolt: run %q on %q: %w", statement.SQL, branch, err)
		}
	}
	if err := checkoutBranch(ctx, conn, MainBranch); err != nil {
		discardConn(conn)
		return err
	}
	return nil
}

// StageUntilCanceled drives stage past branch creation and then blocks in its
// write until ctx is canceled. It exposes the otherwise hard-to-reach failed
// restore cleanup path to the external regression test without adding a
// production hook.
func (s *Store) StageUntilCanceled(ctx context.Context) error {
	_, err := s.stage(ctx, stagedWrite{
		kind:     KindFact,
		proposal: Proposal{Rationale: "cleanup probe", Actor: store.Actor{Name: "agent:test", Email: "test@memdolt.invalid"}, Target: TargetRepo},
		rowID:    newID(),
		now:      time.Now().UTC(),
		message:  "probe canceled proposal cleanup",
		statements: []store.Statement{{
			SQL: "SELECT SLEEP(1)",
		}},
		text: []string{"cleanup probe"},
	})
	return err
}
