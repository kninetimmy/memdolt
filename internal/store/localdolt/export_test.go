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

// StageUntilCanceled drives stage past branch creation and then holds it
// there until ctx is canceled, so the write lane runs on a dead context and
// the restore back to origin fails with it. It exposes the otherwise
// hard-to-reach failed restore cleanup path to the external regression test;
// checkedOut closes only after DOLT_CHECKOUT has completed.
func (s *Store) StageUntilCanceled(ctx context.Context, checkedOut chan<- struct{}) error {
	_, err := s.stage(ctx, stagedWrite{
		kind:     KindFact,
		proposal: Proposal{Rationale: "cleanup probe", Actor: store.Actor{Name: "agent:test", Email: "test@memdolt.invalid"}, Target: TargetRepo},
		rowID:    newID(),
		now:      time.Now().UTC(),
		message:  "probe canceled proposal cleanup",
		text:     []string{"cleanup probe"},
		afterCheckout: func() {
			close(checkedOut)
			<-ctx.Done()
		},
	})
	return err
}

// AcceptProposalAfterExpectedCommit exposes the exact post-validation seam to
// deterministic external mutation and review-verb serialization regressions.
func (s *Store) AcceptProposalAfterExpectedCommit(
	ctx context.Context,
	id string,
	reviewer store.Actor,
	options AcceptOptions,
	after func(),
) (AcceptResult, error) {
	return s.acceptProposal(ctx, id, reviewer, options, acceptHooks{afterExpectedCommit: after})
}

// AcceptProposalBeforeCleanup exposes the final post-merge boundary so a test
// can prove expected-commit acceptance never deletes a foreign commit created
// where eager cleanup used to race it.
func (s *Store) AcceptProposalBeforeCleanup(
	ctx context.Context,
	id string,
	reviewer store.Actor,
	options AcceptOptions,
	before func(),
) (AcceptResult, error) {
	return s.acceptProposal(ctx, id, reviewer, options, acceptHooks{beforeCleanup: before})
}

// AcceptProposalWithCleanupError leaves the already-merged branch in place and
// proves the established populated-result-plus-error cleanup contract.
func (s *Store) AcceptProposalWithCleanupError(
	ctx context.Context,
	id string,
	reviewer store.Actor,
	options AcceptOptions,
	err error,
) (AcceptResult, error) {
	return s.acceptProposal(ctx, id, reviewer, options, acceptHooks{cleanupError: func() error { return err }})
}
