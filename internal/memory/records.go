package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

type FactRecord struct {
	ID           string     `json:"id"`
	Key          string     `json:"key"`
	Value        string     `json:"value"`
	Source       string     `json:"source"`
	Kind         string     `json:"kind,omitempty"`
	Evidence     string     `json:"evidence,omitempty"`
	VerifiedAt   *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	Stale        bool       `json:"stale"`
	SupersededBy string     `json:"supersededBy,omitempty"`
}

type DecisionRecord struct {
	ID                   string    `json:"id"`
	Title                string    `json:"title"`
	Rationale            string    `json:"rationale"`
	Summary              string    `json:"summary,omitempty"`
	AlternativesRejected string    `json:"alternativesRejected,omitempty"`
	Evidence             string    `json:"evidence,omitempty"`
	Status               string    `json:"status"`
	Source               string    `json:"source"`
	DecidedAt            time.Time `json:"decidedAt"`
	SupersededBy         string    `json:"supersededBy,omitempty"`
}

// ListFacts is the shared CLI/MCP committed-main reader. A literal dotted
// prefix includes superseded rows; its SQL wildcard is supplied only here.
func ListFacts(ctx context.Context, st store.Store, prefix string, limit int, staleAfterDays int64) (facts []FactRecord, err error) {
	prefix = strings.TrimSpace(prefix)
	if prefix != "" && !strings.HasSuffix(prefix, ".") {
		return nil, fmt.Errorf("fact prefix %q must end in '.'", prefix)
	}
	if limit < 0 {
		return nil, errors.New("fact list limit must not be negative")
	}
	query := "SELECT id, `key`, value, source, kind, evidence, verified_at, created_at, superseded_by FROM facts AS OF 'main'"
	args := []any{}
	if prefix != "" {
		query += " WHERE `key` LIKE ? ESCAPE '!'"
		args = append(args, strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(prefix)+"%")
	}
	query += " ORDER BY `key`, created_at, id"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := st.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list facts: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	now := time.Now().UTC()
	facts = []FactRecord{}
	for rows.Next() {
		var fact FactRecord
		var key, value, source, kind, evidence, superseded sql.NullString
		var verified, created sql.NullTime
		if err := rows.Scan(&fact.ID, &key, &value, &source, &kind, &evidence, &verified, &created, &superseded); err != nil {
			return nil, fmt.Errorf("list facts: %w", err)
		}
		fact.Key, fact.Value, fact.Source = key.String, value.String, source.String
		fact.Kind, fact.Evidence, fact.SupersededBy = kind.String, evidence.String, superseded.String
		if verified.Valid {
			fact.VerifiedAt = &verified.Time
		}
		if created.Valid {
			fact.CreatedAt = created.Time
		}
		fact.Stale = !verified.Valid || FactIsStale(verified.Time, now, staleAfterDays)
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list facts: %w", err)
	}
	return facts, nil
}

// FactIsStale is the shared recall/list day comparison. A floating-point
// horizon avoids overflowing a time.Duration for valid int64 day counts.
func FactIsStale(verifiedAt, now time.Time, staleAfterDays int64) bool {
	return max(now.Sub(verifiedAt.UTC()).Hours()/24, 0) > float64(staleAfterDays)
}

// ListDecisions retains the MCP status vocabulary and ordering. Zero means
// unbounded; the MCP handler retains its existing default limit.
func ListDecisions(ctx context.Context, st store.Store, status string, limit int) (decisions []DecisionRecord, err error) {
	status = strings.TrimSpace(status)
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "superseded" && status != "draft" && status != "all" {
		return nil, fmt.Errorf("unknown decision status %q, want active, superseded, draft, or all", status)
	}
	if limit < 0 {
		return nil, errors.New("decision list limit must not be negative")
	}
	query := "SELECT id, title, rationale, summary, alternatives_rejected, evidence, status, source, decided_at, superseded_by FROM decisions AS OF 'main'"
	args := []any{}
	if status != "all" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY decided_at DESC, id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := st.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	decisions = []DecisionRecord{}
	for rows.Next() {
		var decision DecisionRecord
		var title, rationale, summary, alternatives, evidence, rowStatus, source, superseded sql.NullString
		var decided sql.NullTime
		if err := rows.Scan(&decision.ID, &title, &rationale, &summary, &alternatives, &evidence,
			&rowStatus, &source, &decided, &superseded); err != nil {
			return nil, fmt.Errorf("list decisions: %w", err)
		}
		decision.Title, decision.Rationale = title.String, rationale.String
		decision.Summary, decision.AlternativesRejected = summary.String, alternatives.String
		decision.Evidence, decision.Status, decision.Source = evidence.String, rowStatus.String, source.String
		decision.SupersededBy = superseded.String
		if decided.Valid {
			decision.DecidedAt = decided.Time
		}
		decisions = append(decisions, decision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	return decisions, nil
}
