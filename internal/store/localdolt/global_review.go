package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

type globalAcceptHooks struct {
	afterCapture  func()
	afterStage    func()
	afterMerge    func()
	beforeMerge   func(*Store)
	finalizeStage func(*sql.Tx) error
	finalizeMerge func(*sql.Tx) error
}

// Accept already holds the repository proposalMu. OpenGlobal takes its file
// lock without waiting, then the private global Store takes proposalMu. Recall
// may hold the global lock before requesting a repository capture: acceptance
// immediately refuses that lock, so it cannot form a cross-store wait cycle.
// Neither foreign Dolt sessions nor filesystem writers share these mutexes.
func (s *Store) acceptGlobalProposal(ctx context.Context, source *sql.Conn, record branchRecord, reviewer store.Actor, options AcceptOptions, hooks globalAcceptHooks) (result AcceptResult, err error) {
	if reviewer.Name != memory.UserActor.Name || s.globalRepo != nil || options.ExpectedCommit != "" {
		return result, errors.New("global acceptance requires terminal review by the trusted user in the source repository")
	}
	if err := requireOneCommit(ctx, source, record.bare()); err != nil {
		return result, err
	}
	parents, err := globalCommitParents(ctx, source, record.commit)
	if err != nil || len(parents) != 1 {
		return result, errors.Join(errors.New("global review requires a single-parent proposal commit"), err)
	}
	payload, err := captureInteropProposal(ctx, source, record)
	if err != nil {
		return result, err
	}
	if err := validateGlobalPayload(ctx, source, payload.Parent, payload); err != nil {
		return result, err
	}
	result.Proposal = record.bare()
	result.Proposal.Kind = ProposalKind(interopValue(payload.Metadata, "kind"))
	result.Proposal.Target = TargetGlobal
	result.Proposal.Actor = interopValue(payload.Metadata, "actor")
	result.Proposal.Rationale = interopValue(payload.Metadata, "rationale")
	result.Proposal.CreatedAt, _ = time.Parse(time.DateTime, interopValue(payload.Metadata, "created_at"))
	result.SourceRetained = true
	for _, change := range payload.Changes {
		result.RowIDs = append(result.RowIDs, interopValue(change.To, "id"))
	}
	result.SourceMainCommit, err = branchHead(ctx, source, MainBranch)
	if err != nil {
		return result, err
	}
	defer func() {
		result.Unknown = errors.Is(err, store.ErrCommitUnknown)
		err = result.RecoveryError(err)
	}()
	if err := requireTransferClean(ctx, source); err != nil {
		return result, fmt.Errorf("global review source: %w", err)
	}
	var author store.Actor
	if err := source.QueryRowContext(ctx, "SELECT committer, email FROM DOLT_LOG(?) WHERE commit_hash = ?", record.commit, record.commit).Scan(&author.Name, &author.Email); err != nil {
		return result, err
	}
	if err := author.Validate(); err != nil {
		return result, err
	}
	global, err := OpenGlobal(ctx, s.paths.Base())
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, global.Close()) }()
	global.proposalMu.Lock()
	defer global.proposalMu.Unlock()
	conn, head, err := global.initializedMainConn(ctx, "global review")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := requireTransferClean(ctx, conn); err != nil {
		return result, fmt.Errorf("global review destination: %w", err)
	}
	if _, err := statusSchema(ctx, conn, head); err != nil {
		return result, err
	}
	if hooks.afterCapture != nil {
		hooks.afterCapture()
	}
	// Existing review's diff scan stays separate from CommitRequest. For global
	// copying, scan every persisted new/changed cell, including nullable source
	// metadata, since the source and destination are different durable stores.
	text := append(globalProposalText(payload), author.Name, author.Email)
	if err := global.checkDenyList(text); err != nil {
		return result, err
	}
	merge, stage, err := priorGlobalAcceptance(ctx, conn, head, payload, author)
	if err != nil {
		return result, err
	}
	if merge != "" {
		result.Commit, result.GlobalStageCommit, result.AlreadyAccepted = merge, stage, true
		return result, errors.Join(checkGlobalSource(ctx, source, record, result.SourceMainCommit), checkGlobalHead(ctx, conn, head))
	}
	if err := validateGlobalDestination(ctx, conn, head, payload); err != nil {
		return result, err
	}
	if err := probeGlobalProposal(ctx, conn, result.Proposal, payload, options); err != nil {
		return result, err
	}
	if err := checkGlobalSource(ctx, source, record, result.SourceMainCommit); err != nil {
		return result, err
	}
	if err := checkGlobalHead(ctx, conn, head); err != nil {
		return result, err
	}
	branches, err := proposalBranches(ctx, conn)
	if err != nil {
		return result, err
	}
	for _, candidate := range branches {
		if candidate.name == record.name {
			if err := verifyGlobalStage(ctx, conn, candidate.commit, payload, author); err != nil {
				return result, fmt.Errorf("global proposal branch already exists with different or incomplete content; retain and inspect it: %w", err)
			}
			result.GlobalStageCommit = candidate.commit
		}
	}
	if result.GlobalStageCommit == "" {
		w, err := interopStagedWrite(payload, author)
		if err != nil {
			return result, err
		}
		w.proposal.Target, w.globalReview = TargetGlobal, true
		w.message, w.text, w.finalizeCommit = globalStageMessage(payload), text, hooks.finalizeStage
		staged, err := global.stageLocked(ctx, w)
		result.GlobalStageCommit = staged.Commit
		if err != nil {
			return result, err
		}
	}
	if hooks.afterStage != nil {
		hooks.afterStage()
	}
	if hooks.beforeMerge != nil {
		hooks.beforeMerge(global)
	}
	// A paused/retried stage is not authority to overwrite a changed destination.
	// Check the complete before-images again before the native merge protocol.
	if err := checkGlobalSource(ctx, source, record, result.SourceMainCommit); err != nil {
		return result, err
	}
	if err := checkGlobalHead(ctx, conn, head); err != nil {
		return result, err
	}
	if err := global.checkDenyList(text); err != nil {
		return result, err
	}
	if err := requireTransferClean(ctx, conn); err != nil {
		return result, err
	}
	if err := verifyGlobalStage(ctx, conn, result.GlobalStageCommit, payload, author); err != nil {
		return result, err
	}
	staged := result.Proposal
	staged.Commit = result.GlobalStageCommit
	if err := requireOneCommit(ctx, conn, staged); err != nil {
		return result, err
	}
	merged, err := global.mergeGlobalProposal(ctx, conn, head, staged, reviewer, hooks.finalizeMerge)
	result.Commit, result.Cleared = merged.Commit, merged.Cleared
	if err != nil {
		return result, err
	}
	if hooks.afterMerge != nil {
		hooks.afterMerge()
	}
	// Dolt has no expected-head delete. Keep both source and destination refs;
	// never risk deleting foreign content between a head check and deletion.
	return result, checkGlobalSource(ctx, source, record, result.SourceMainCommit)
}

func globalStageMessage(p InteropProposal) string {
	return "propose global " + interopValue(p.Metadata, "kind") + " " + p.ID + " from " + p.Head
}

func globalProposalText(p InteropProposal) []string {
	var text []string
	for _, column := range interopColumns("proposals") {
		text = append(text, interopValue(p.Metadata, column))
	}
	for _, change := range p.Changes {
		for _, column := range interopColumns(change.Table) {
			if !equalStringPointers(change.From[column], change.To[column]) {
				text = append(text, interopValue(change.To, column))
			}
		}
	}
	return text
}

func globalProposalView(ctx context.Context, conn *sql.Conn, head string) (map[string]map[string]InteropRow, error) {
	view := map[string]map[string]InteropRow{}
	for _, table := range []string{"facts", "decisions"} {
		rows, err := readInteropRows(ctx, conn, head, table)
		if err != nil {
			return nil, err
		}
		view[table] = map[string]InteropRow{}
		for _, row := range rows {
			view[table][interopValue(row, "id")] = row
		}
	}
	return view, nil
}

func validateGlobalPayload(ctx context.Context, conn *sql.Conn, head string, p InteropProposal) error {
	if interopValue(p.Metadata, "target") != string(TargetGlobal) {
		return errors.New("captured proposal does not target global memory")
	}
	view, err := globalProposalView(ctx, conn, head)
	if err != nil {
		return err
	}
	if err := validateProposalPayload(p, view); err != nil {
		return fmt.Errorf("global review payload/before-image: %w", err)
	}
	if ProposalKind(interopValue(p.Metadata, "kind")) == KindSupersede {
		if interopValue(p.Changes[0].To, "key") != interopValue(p.Changes[1].To, "key") {
			return errors.New("global supersede must retain the exact fact key")
		}
	}
	return validateInteropFactKeys(ctx, conn, InteropBundle{Tables: map[string][]InteropRow{"facts": slices.Collect(maps.Values(view["facts"]))}, Proposals: []InteropProposal{p}})
}

func validateGlobalDestination(ctx context.Context, conn *sql.Conn, head string, p InteropProposal) error {
	if _, err := statusSchema(ctx, conn, head); err != nil {
		return err
	}
	rows, err := readInteropRows(ctx, conn, head, "proposals")
	if err != nil {
		return err
	}
	for _, row := range rows {
		if interopValue(row, "id") == p.ID {
			return errors.New("global proposal identity already exists without matching native acceptance history; inspect global rows and history, never replay or overwrite")
		}
	}
	return validateGlobalPayload(ctx, conn, head, p)
}

func probeGlobalProposal(ctx context.Context, conn *sql.Conn, proposal PendingProposal, p InteropProposal, options AcceptOptions) error {
	if proposal.Kind == KindSupersede || options.Force {
		return nil // only reached after validating the complete destination shape
	}
	if options.ValidateContradictionConfig == nil {
		return errors.New("global review contradiction probe configuration validator is unavailable")
	}
	if err := options.ValidateContradictionConfig(); err != nil {
		return fmt.Errorf("global review contradiction probe configuration: %w", err)
	}
	var claims []contradictionClaim
	for _, change := range p.Changes {
		row := change.To
		if change.Table == "facts" {
			claims = append(claims, contradictionClaim{sourceType: "fact", text: factSemanticText(interopValue(row, "key"), interopValue(row, "value"))})
		} else {
			claims = append(claims, contradictionClaim{sourceType: "decision", text: decisionSemanticText(interopValue(row, "title"), interopValue(row, "rationale"), interopValue(row, "summary"))})
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	err = checkContradictionClaims(ctx, tx, claims, proposal, options.OpenContradictionScorer)
	return errors.Join(err, tx.Rollback())
}

func checkGlobalSource(ctx context.Context, conn *sql.Conn, record branchRecord, main string) error {
	current, err := findProposalBranch(ctx, conn, record.name)
	if err != nil || current.commit != record.commit {
		return errors.Join(errors.New("source proposal changed during global review; source branch retained; inspect its current commit before any manual rejection"), err)
	}
	return checkGlobalHead(ctx, conn, main)
}

func checkGlobalHead(ctx context.Context, conn *sql.Conn, expected string) error {
	head, err := branchHead(ctx, conn, MainBranch)
	if err != nil || head != expected {
		return errors.Join(errors.New("main changed during global review; inspect both stores before retrying"), err)
	}
	return nil
}

func (s *Store) mergeGlobalProposal(ctx context.Context, conn *sql.Conn, head string, proposal PendingProposal, reviewer store.Actor, finalize func(*sql.Tx) error) (AcceptResult, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return AcceptResult{}, err
	}
	current, err := branchHead(ctx, tx, MainBranch)
	if err != nil || current != head {
		return AcceptResult{}, errors.Join(errors.New("global main changed before the merge transaction; inspect before retrying"), err, tx.Rollback())
	}
	result, err := s.merge(ctx, tx, proposal, reviewer)
	if err != nil {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
		return result, err
	}
	if finalize == nil {
		finalize = (*sql.Tx).Commit
	}
	return result, finalize(tx)
}

func globalCommitParents(ctx context.Context, conn *sql.Conn, hash string) (parents []string, err error) {
	rows, err := conn.QueryContext(ctx, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? ORDER BY parent_index", hash)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	return parents, rows.Err()
}

func sameGlobalPayload(a, b InteropProposal) bool {
	return a.ID == b.ID && sameInteropRow(a.Metadata, b.Metadata) && slices.EqualFunc(a.Changes, b.Changes, func(a, b InteropChange) bool {
		return a.Table == b.Table && sameInteropRow(a.From, b.From) && sameInteropRow(a.To, b.To)
	})
}

func verifyGlobalStage(ctx context.Context, conn *sql.Conn, hash string, source InteropProposal, author store.Actor) error {
	var message, name, email string
	if err := conn.QueryRowContext(ctx, "SELECT message, committer, email FROM DOLT_LOG(?) WHERE commit_hash = ?", hash, hash).Scan(&message, &name, &email); err != nil {
		return err
	}
	if message != globalStageMessage(source) || name != author.Name || email != author.Email {
		return errors.New("global staging commit does not identify the exact source commit and author")
	}
	parents, err := globalCommitParents(ctx, conn, hash)
	if err != nil || len(parents) != 1 {
		return errors.Join(errors.New("global staging commit must have one parent"), err)
	}
	captured, err := captureInteropProposal(ctx, conn, branchRecord{id: source.ID, commit: hash})
	if err != nil || !sameGlobalPayload(captured, source) {
		return errors.Join(errors.New("global staging payload differs from the captured source proposal"), err)
	}
	return nil
}

// Native history is the receipt. Verify the two-parent review commit, its
// source-bound staging parent, and both complete diffs; matching row text alone
// never proves acceptance. Later edits to durable rows do not authorize replay.
func priorGlobalAcceptance(ctx context.Context, conn *sql.Conn, head string, source InteropProposal, author store.Actor) (merge, stage string, err error) {
	rows, err := conn.QueryContext(ctx, "SELECT commit_hash FROM DOLT_LOG(?) WHERE message = ? AND committer = ?", head, "review accept "+interopValue(source.Metadata, "kind")+" "+source.ID, memory.UserActor.Name)
	if err != nil {
		return "", "", err
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return "", "", errors.Join(err, rows.Close())
		}
		hashes = append(hashes, hash)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", "", err
	}
	for _, hash := range hashes {
		parents, err := globalCommitParents(ctx, conn, hash)
		if err != nil || len(parents) != 2 {
			return "", "", errors.Join(errors.New("ambiguous native global acceptance history"), err)
		}
		if err := verifyGlobalStage(ctx, conn, parents[1], source, author); err != nil {
			return "", "", err
		}
		captured, err := captureInteropProposal(ctx, conn, branchRecord{id: source.ID, commit: hash})
		if err != nil || !sameGlobalPayload(captured, source) || merge != "" {
			return "", "", errors.Join(errors.New("global acceptance merge does not contain exactly the reviewed payload once"), err)
		}
		merge, stage = hash, parents[1]
	}
	return merge, stage, nil
}

// RecoveryError retains observed hashes through native, owner, close and
// output errors. It never upgrades an unobserved outcome to confirmed success.
func (r AcceptResult) RecoveryError(err error) error {
	if err == nil || r.Proposal.Target != TargetGlobal {
		return err
	}
	return fmt.Errorf("global review %s source %s (repository main %s), global staging %q, acceptance %q; source branch retained; inspect `memdolt review show %s --dir <repository>`, global rows and native history before retrying; do not automatically replay: %w", r.Proposal.ID, r.Proposal.Commit, r.SourceMainCommit, r.GlobalStageCommit, r.Commit, r.Proposal.ID, err)
}
