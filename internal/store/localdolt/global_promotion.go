package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

// PromotionRecord is a captured committed row, including SQL NULLs. Its commit
// and original id are audit metadata, never an instruction to mutate the source.
type PromotionRecord struct {
	Kind   string     `json:"kind"`
	Commit string     `json:"commit"`
	Row    InteropRow `json:"row"`
}

func promotionTable(kind string) (string, error) {
	switch kind {
	case "fact":
		return "facts", nil
	case "decision":
		return "decisions", nil
	default:
		return "", errors.New("only facts and decisions can be promoted to global memory")
	}
}

// CapturePromotion uses the existing immutable-row reader under proposalMu.
// Owner IPC exposes this read only; the destination write stays human CLI-only.
func (s *Store) CapturePromotion(ctx context.Context, kind, ident string) (result PromotionRecord, err error) {
	table, err := promotionTable(kind)
	if err != nil {
		return result, err
	}
	if ident == "" {
		return result, errors.New("promotion identifier must not be empty; select an exact committed id or key")
	}
	if err := humanText("promotion identifier", ident, 255); err != nil {
		return result, err
	}
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	conn, head, err := s.initializedMainConn(ctx, "promotion capture")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := validateTransferSchema(ctx, conn, head); err != nil {
		return result, err
	}
	rows, err := readInteropRows(ctx, conn, head, table)
	if err != nil {
		return result, err
	}
	for _, row := range rows {
		if interopValue(row, "id") != ident && (kind != "fact" || interopValue(row, "key") != ident) {
			continue
		}
		if result.Row != nil {
			return PromotionRecord{}, errors.New("ambiguous promotion key; select the exact committed fact id")
		}
		result = PromotionRecord{Kind: kind, Commit: head, Row: row}
	}
	if result.Row == nil {
		return result, errors.New("no committed record with that exact id or key")
	}
	return result, validatePromotion(result)
}

func validatePromotion(record PromotionRecord) error {
	table, err := promotionTable(record.Kind)
	if err != nil {
		return err
	}
	if !transferHash.MatchString(record.Commit) {
		return errors.New("promotion requires a captured immutable source commit")
	}
	if err := validateInteropRow(table, record.Row); err != nil {
		return err
	}
	if record.Row["superseded_by"] != nil {
		return errors.New("promote the live replacement instead of a superseded record; a repository supersession link cannot reference global memory")
	}
	return nil
}

// PromoteGlobal copies one validated row with a fresh id, preserving nullable
// cells and timestamps. Existing live fact keys refuse; decision duplicates are
// retained and reported. No raw promotion write is registered in owner IPC/MCP.
func (s *Store) PromoteGlobal(ctx context.Context, record PromotionRecord, actor memory.Actor) (HumanMemoryResult, error) {
	if s.globalRepo == nil {
		return HumanMemoryResult{}, errors.New("promotion requires the explicitly opened global replica")
	}
	if err := s.globalWriteActor(actor); err != nil {
		return HumanMemoryResult{}, err
	}
	if err := validatePromotion(record); err != nil {
		return HumanMemoryResult{}, err
	}
	table, _ := promotionTable(record.Kind)
	row := maps.Clone(record.Row)
	sourceID := interopValue(row, "id")
	row["id"] = interopString(newID())
	text := []string{record.Commit, sourceID}
	for _, column := range interopColumns(table) {
		text = append(text, interopValue(row, column))
	}
	return s.humanMemoryWrite(ctx, actor, text, "promote "+record.Kind+" from "+record.Commit+"/"+sourceID, func(conn *sql.Conn) (HumanMemoryResult, []store.Statement, error) {
		result := HumanMemoryResult{Kind: record.Kind, ID: interopValue(row, "id"), Status: "promoted", SourceID: sourceID, SourceCommit: record.Commit}
		if record.Kind == "fact" {
			var existing string
			err := conn.QueryRowContext(ctx, "SELECT id FROM facts WHERE live_key = ?", interopValue(row, "key")).Scan(&existing)
			if err == nil {
				return result, nil, fmt.Errorf("global fact key already exists as %s; promotion never overwrites it; inspect fact list --global and use an explicit human fact add --global if replacement is intended", existing)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return result, nil, err
			}
		} else {
			var err error
			result.TitleCollisions, err = globalDecisionCollisions(ctx, conn, interopValue(row, "title"))
			if err != nil {
				return result, nil, err
			}
		}
		return result, []store.Statement{interopInsert(table, row)}, nil
	})
}

func globalDecisionCollisions(ctx context.Context, conn *sql.Conn, title string) (ids []string, err error) {
	// ponytail: scan title collisions at memory scale; a normal title index can
	// replace this if needed. The pinned optimizer misuses FULLTEXT for WHERE
	// equality even with IGNORE INDEX. Projection keeps SQL collation semantics.
	rows, err := conn.QueryContext(ctx, "SELECT id, COALESCE(title = ?, false) FROM decisions ORDER BY id", title)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id string
		var matches bool
		if err := rows.Scan(&id, &matches); err != nil {
			return nil, err
		}
		if matches {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
