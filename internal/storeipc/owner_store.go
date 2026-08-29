package storeipc

import (
	"context"
	"database/sql"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

const (
	wireTypeKey  = "$memdoltType"
	wireValueKey = "value"
	wireTimeType = "time"
)

type wireTime struct {
	Type  string `json:"$memdoltType"`
	Value string `json:"value"`
}

// OwnerStore implements the shipped store surface over the authenticated IPC
// endpoint of the process that owns embedded Dolt.
type OwnerStore struct {
	client  *Client
	dataDir string
}

// DialOwnerStore finds the live owner of baseDir. It never opens embedded
// Dolt; callers may fall back to LocalStore only when ipc.Probe established
// that no live owner exists.
func DialOwnerStore(baseDir string) (*OwnerStore, error) {
	client, err := Dial(baseDir)
	if err != nil {
		return nil, err
	}
	paths, err := layout.New(baseDir)
	if err != nil {
		return nil, err
	}
	return &OwnerStore{client: client, dataDir: paths.DoltDataDir()}, nil
}

var _ store.Store = (*OwnerStore)(nil)
var _ Backend = (*OwnerStore)(nil)

// DataDir reports the owner's embedded data directory without opening it.
func (s *OwnerStore) DataDir() string { return s.dataDir }

// Open verifies the same schema-newer-than-binary boundary as LocalStore.
// DialOwnerStore has already established ownership and transport.
func (s *OwnerStore) Open(ctx context.Context) error {
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	return store.CheckSchemaVersion(version)
}

// Close releases no ownership: the remote process owns the store and its
// endpoint. The HTTP transport remains reusable until this short-lived CLI
// exits.
func (s *OwnerStore) Close() error { return nil }

// Commit preserves the caller-minted statements, author and deny-list text.
// Client.Commit submits once and never retries an unknown outcome.
func (s *OwnerStore) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	statements := make([]Statement, len(req.Statements))
	for i, statement := range req.Statements {
		statements[i] = Statement{SQL: statement.SQL, Args: wireArgs(statement.Args)}
	}
	result, err := s.client.Commit(ctx, CommitRequest{
		Statements: statements,
		Text:       req.Text,
		NoText:     req.NoText,
		Message:    req.Message,
		Author:     Actor{Name: req.Author.Name, Email: req.Author.Email},
	})
	if err != nil {
		return store.CommitResult{}, err
	}
	return store.CommitResult{Hash: result.Hash, RowsAffected: result.RowsAffected}, nil
}

func wireArgs(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	wire := make([]any, len(args))
	for i, arg := range args {
		if stamp, ok := arg.(time.Time); ok {
			wire[i] = wireTime{Type: wireTimeType, Value: stamp.Format(time.RFC3339Nano)}
			continue
		}
		wire[i] = arg
	}
	return wire
}

// Query returns the same scanner-shaped result as a direct Store.Query.
func (s *OwnerStore) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	result, err := s.client.Query(ctx, QueryRequest{SQL: query, Args: wireArgs(args)})
	if err != nil {
		return nil, err
	}
	return &ownerRows{columns: result.Columns, rows: result.Rows, index: -1}, nil
}

func (s *OwnerStore) operation(ctx context.Context, name string, args, result any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("storeipc: %s: encode arguments: %w", name, err)
	}
	if err := s.client.ipc.PostJSON(ctx, OperationPath, operationRequest{Operation: name, Args: raw}, result); err != nil {
		return fmt.Errorf("storeipc: %s: %w", name, err)
	}
	return nil
}

func (s *OwnerStore) SchemaVersion(ctx context.Context) (int, error) {
	var result int
	return result, s.operation(ctx, opSchemaVersion, nil, &result)
}

func (s *OwnerStore) EmbeddingSources(ctx context.Context) ([]store.EmbeddingSource, error) {
	var result []store.EmbeddingSource
	return result, s.operation(ctx, opEmbeddingSources, nil, &result)
}

func (s *OwnerStore) RecallSources(ctx context.Context) ([]store.RecallSource, error) {
	var result []store.RecallSource
	return result, s.operation(ctx, opRecallSources, nil, &result)
}

func (s *OwnerStore) RecallFTS(ctx context.Context, query string, sourceTypes []string) ([]store.LexicalHit, error) {
	var result []store.LexicalHit
	err := s.operation(ctx, opRecallFTS, recallFTSArgs{Query: query, SourceTypes: sourceTypes}, &result)
	return result, err
}

func (s *OwnerStore) SearchDecisions(ctx context.Context, query string, limit int) ([]store.DecisionSearchHit, error) {
	var result []store.DecisionSearchHit
	err := s.operation(ctx, opSearchDecisions, searchDecisionsArgs{Query: query, Limit: limit}, &result)
	return result, err
}

func (s *OwnerStore) LastChanged(ctx context.Context, sourceType, sourceID string) (*store.CommitProvenance, error) {
	var result *store.CommitProvenance
	err := s.operation(ctx, opLastChanged, lastChangedArgs{SourceType: sourceType, SourceID: sourceID}, &result)
	return result, err
}

func (s *OwnerStore) ProposeFact(ctx context.Context, proposal localdolt.Proposal, fact localdolt.Fact) (localdolt.StagedProposal, error) {
	var result localdolt.StagedProposal
	err := s.operation(ctx, opProposeFact, proposeFactArgs{Proposal: proposal, Fact: fact}, &result)
	return result, err
}

func (s *OwnerStore) ProposeDecision(ctx context.Context, proposal localdolt.Proposal, decision localdolt.Decision) (localdolt.StagedProposal, error) {
	var result localdolt.StagedProposal
	err := s.operation(ctx, opProposeDecision, proposeDecisionArgs{Proposal: proposal, Decision: decision}, &result)
	return result, err
}

func (s *OwnerStore) ProposeSupersede(ctx context.Context, proposal localdolt.Proposal, supersededID string, replacement localdolt.Fact) (localdolt.StagedProposal, error) {
	var result localdolt.StagedProposal
	err := s.operation(ctx, opProposeSupersede, proposeSupersedeArgs{
		Proposal: proposal, SupersededID: supersededID, Replacement: replacement,
	}, &result)
	return result, err
}

func (s *OwnerStore) PendingProposals(ctx context.Context) ([]localdolt.PendingProposal, error) {
	var result []localdolt.PendingProposal
	return result, s.operation(ctx, opPendingProposals, nil, &result)
}

func (s *OwnerStore) ProposalDiff(ctx context.Context, id string) (localdolt.ProposalDiff, error) {
	var result localdolt.ProposalDiff
	return result, s.operation(ctx, opProposalDiff, proposalIDArgs{ID: id}, &result)
}

func (s *OwnerStore) RejectProposal(ctx context.Context, id string) (localdolt.PendingProposal, error) {
	var result localdolt.PendingProposal
	return result, s.operation(ctx, opRejectProposal, proposalIDArgs{ID: id}, &result)
}

func (s *OwnerStore) ExpireProposals(ctx context.Context, before time.Time) ([]localdolt.PendingProposal, error) {
	var result []localdolt.PendingProposal
	return result, s.operation(ctx, opExpireProposals, expireProposalsArgs{Before: before}, &result)
}

func (s *OwnerStore) AcceptProposal(ctx context.Context, id string, reviewer store.Actor) (localdolt.AcceptResult, error) {
	var result localdolt.AcceptResult
	return result, s.operation(ctx, opAcceptProposal, acceptProposalArgs{ID: id, Reviewer: reviewer}, &result)
}

type ownerRows struct {
	columns []string
	rows    [][]*string
	index   int
	closed  bool
}

func (r *ownerRows) Columns() ([]string, error) { return append([]string(nil), r.columns...), nil }

func (r *ownerRows) Next() bool {
	if r.closed {
		return false
	}
	if r.index+1 >= len(r.rows) {
		r.index = len(r.rows)
		return false
	}
	r.index++
	return true
}

func (r *ownerRows) Scan(dest ...any) error {
	if r.closed {
		return errors.New("storeipc: scan rows after close")
	}
	if r.index < 0 || r.index >= len(r.rows) {
		return errors.New("storeipc: Scan called without a current row")
	}
	row := r.rows[r.index]
	if len(dest) != len(row) {
		return fmt.Errorf("storeipc: Scan got %d destinations for %d columns", len(dest), len(row))
	}
	for i := range row {
		if err := assignCell(dest[i], row[i]); err != nil {
			return fmt.Errorf("storeipc: scan column %d: %w", i, err)
		}
	}
	return nil
}

func (r *ownerRows) Err() error { return nil }

func (r *ownerRows) Close() error {
	r.closed = true
	return nil
}

func assignCell(dest any, cell *string) error {
	if cell == nil {
		switch target := dest.(type) {
		case *any:
			*target = nil
		case *[]byte:
			*target = nil
		case *sql.NullString:
			*target = sql.NullString{}
		case *sql.NullTime:
			*target = sql.NullTime{}
		case *sql.NullInt64:
			*target = sql.NullInt64{}
		case *sql.NullFloat64:
			*target = sql.NullFloat64{}
		case *sql.NullBool:
			*target = sql.NullBool{}
		default:
			return fmt.Errorf("cannot assign NULL to %T", dest)
		}
		return nil
	}

	value := *cell
	switch target := dest.(type) {
	case *any:
		*target = value
	case *string:
		*target = value
	case *[]byte:
		*target = []byte(value)
	case *sql.NullString:
		*target = sql.NullString{String: value, Valid: true}
	case *int:
		parsed, err := strconv.ParseInt(value, 10, 0)
		if err != nil {
			return err
		}
		*target = int(parsed)
	case *int64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		*target = parsed
	case *sql.NullInt64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		*target = sql.NullInt64{Int64: parsed, Valid: true}
	case *float64:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		*target = parsed
	case *sql.NullFloat64:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		*target = sql.NullFloat64{Float64: parsed, Valid: true}
	case *bool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		*target = parsed
	case *sql.NullBool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		*target = sql.NullBool{Bool: parsed, Valid: true}
	case *time.Time:
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return err
		}
		*target = parsed
	case *sql.NullTime:
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return err
		}
		*target = sql.NullTime{Time: parsed, Valid: true}
	case encoding.TextUnmarshaler:
		return target.UnmarshalText([]byte(value))
	default:
		return fmt.Errorf("unsupported Scan destination %T", dest)
	}
	return nil
}
