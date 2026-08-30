package mcpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const elicitationStateSchema = `CREATE TABLE pending_elicitations (
  state_hash BLOB PRIMARY KEY,
  repository TEXT NOT NULL,
  actor_name TEXT NOT NULL,
  actor_raw TEXT NOT NULL,
  proposal_ids TEXT NOT NULL,
  proposal_commits TEXT NOT NULL,
  queue_position INTEGER NOT NULL,
  action TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  accepted TEXT NOT NULL,
  skipped TEXT NOT NULL,
  fact TEXT NOT NULL
)`

var errElicitationStateNotFound = errors.New("requestState is missing, forged, or already used")

// elicitationStateStore is a process-local relational store. A real row makes
// requestState consumption atomic, and authorization insert/consume failures
// fail before promotion. A continuation progress update may instead fail after
// an authorized merge; the caller reports that accepted prefix and stops.
// In-memory SQLite keeps ephemeral approval material out of Dolt history and
// the embedding side-store.
type elicitationStateStore struct {
	db *sql.DB

	closeOnce sync.Once
	closeErr  error
}

func newElicitationStateStore() (*elicitationStateStore, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open elicitation state store: %w", err)
	}
	// SQLite :memory: databases belong to one connection. One is enough for
	// short state-row operations and prevents the pool from opening an empty
	// second database under concurrent tool calls.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(elicitationStateSchema); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize elicitation state store: %w", err), db.Close())
	}
	return &elicitationStateStore{db: db}, nil
}

func (s *elicitationStateStore) insert(ctx context.Context, state string, row pendingElicitation) error {
	proposalIDs, proposalCommits, accepted, skipped, fact, err := encodePendingElicitation(row)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin requestState insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_elicitations WHERE expires_at <= ?", time.Now().UTC().UnixNano()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prune expired requestState rows: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pending_elicitations
	    (state_hash, repository, actor_name, actor_raw, proposal_ids, proposal_commits, queue_position, action, expires_at, accepted, skipped, fact)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		requestStateHash(state), row.Repository, row.Actor.Name, row.Actor.Raw, proposalIDs, proposalCommits,
		row.Position, row.Action, row.ExpiresAt.UnixNano(), accepted, skipped, fact)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store requestState row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit requestState row: %w", err)
	}
	return nil
}

func (s *elicitationStateStore) consume(ctx context.Context, state string) (pendingElicitation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return pendingElicitation{}, fmt.Errorf("begin requestState consumption: %w", err)
	}
	row, err := scanPendingElicitation(tx.QueryRowContext(ctx, pendingElicitationSelect, requestStateHash(state)))
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return pendingElicitation{}, errElicitationStateNotFound
		}
		return pendingElicitation{}, fmt.Errorf("read requestState row: %w", err)
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM pending_elicitations WHERE state_hash = ?", requestStateHash(state))
	if err != nil {
		_ = tx.Rollback()
		return pendingElicitation{}, fmt.Errorf("consume requestState row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return pendingElicitation{}, fmt.Errorf("read requestState consumption result: %w", err)
	}
	if affected != 1 {
		_ = tx.Rollback()
		return pendingElicitation{}, fmt.Errorf("consume requestState row: affected %d rows, want 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return pendingElicitation{}, fmt.Errorf("commit requestState consumption: %w", err)
	}
	return row, nil
}

func (s *elicitationStateStore) updateProgress(ctx context.Context, state string, accepted []localdolt.AcceptResult, skipped []string) (bool, error) {
	acceptedJSON, err := json.Marshal(accepted)
	if err != nil {
		return false, fmt.Errorf("encode accepted review progress: %w", err)
	}
	skippedJSON, err := json.Marshal(skipped)
	if err != nil {
		return false, fmt.Errorf("encode skipped review progress: %w", err)
	}
	result, err := s.db.ExecContext(ctx,
		"UPDATE pending_elicitations SET accepted = ?, skipped = ? WHERE state_hash = ? AND expires_at > ?",
		string(acceptedJSON), string(skippedJSON), requestStateHash(state), time.Now().UTC().UnixNano())
	if err != nil {
		return false, fmt.Errorf("update requestState progress: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read requestState progress result: %w", err)
	}
	return affected == 1, nil
}

func (s *elicitationStateStore) discard(ctx context.Context, state string) error {
	if state == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM pending_elicitations WHERE state_hash = ?", requestStateHash(state)); err != nil {
		return fmt.Errorf("discard requestState row: %w", err)
	}
	return nil
}

func (s *elicitationStateStore) peek(ctx context.Context, state string) (pendingElicitation, error) {
	return scanPendingElicitation(s.db.QueryRowContext(ctx, pendingElicitationSelect, requestStateHash(state)))
}

func (s *elicitationStateStore) expire(ctx context.Context, state string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE pending_elicitations SET expires_at = 0 WHERE state_hash = ?", requestStateHash(state))
	return err
}

func (s *elicitationStateStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

const pendingElicitationSelect = `SELECT repository, actor_name, actor_raw, proposal_ids, proposal_commits,
  queue_position, action, expires_at, accepted, skipped, fact
  FROM pending_elicitations WHERE state_hash = ?`

type rowScanner interface {
	Scan(...any) error
}

func scanPendingElicitation(scanner rowScanner) (pendingElicitation, error) {
	var row pendingElicitation
	var proposalIDs, proposalCommits, accepted, skipped, fact string
	var expiresAt int64
	if err := scanner.Scan(
		&row.Repository, &row.Actor.Name, &row.Actor.Raw, &proposalIDs, &proposalCommits, &row.Position,
		&row.Action, &expiresAt, &accepted, &skipped, &fact,
	); err != nil {
		return pendingElicitation{}, err
	}
	row.ExpiresAt = time.Unix(0, expiresAt).UTC()
	if err := json.Unmarshal([]byte(proposalIDs), &row.ProposalIDs); err != nil {
		return pendingElicitation{}, fmt.Errorf("decode requestState proposal ids: %w", err)
	}
	if err := json.Unmarshal([]byte(proposalCommits), &row.ProposalCommits); err != nil {
		return pendingElicitation{}, fmt.Errorf("decode requestState proposal commits: %w", err)
	}
	if err := json.Unmarshal([]byte(accepted), &row.Accepted); err != nil {
		return pendingElicitation{}, fmt.Errorf("decode requestState accepted progress: %w", err)
	}
	if err := json.Unmarshal([]byte(skipped), &row.Skipped); err != nil {
		return pendingElicitation{}, fmt.Errorf("decode requestState skipped progress: %w", err)
	}
	if err := json.Unmarshal([]byte(fact), &row.Fact); err != nil {
		return pendingElicitation{}, fmt.Errorf("decode requestState fact conflict: %w", err)
	}
	return row, nil
}

func encodePendingElicitation(row pendingElicitation) (proposalIDs, proposalCommits, accepted, skipped, fact string, err error) {
	values := []struct {
		name  string
		value any
		out   *string
	}{
		{name: "proposal ids", value: row.ProposalIDs, out: &proposalIDs},
		{name: "proposal commits", value: row.ProposalCommits, out: &proposalCommits},
		{name: "accepted progress", value: row.Accepted, out: &accepted},
		{name: "skipped progress", value: row.Skipped, out: &skipped},
		{name: "fact conflict", value: row.Fact, out: &fact},
	}
	for _, value := range values {
		encoded, encodeErr := json.Marshal(value.value)
		if encodeErr != nil {
			return "", "", "", "", "", fmt.Errorf("encode requestState %s: %w", value.name, encodeErr)
		}
		*value.out = string(encoded)
	}
	return proposalIDs, proposalCommits, accepted, skipped, fact, nil
}

func requestStateHash(state string) []byte {
	hash := sha256.Sum256([]byte(state))
	return hash[:]
}
