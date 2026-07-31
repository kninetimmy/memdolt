//go:build golden

package golden

import (
	"context"
	"database/sql"
	"fmt"
)

// fulltextGatherer is PRD §8.1 step 1's primary path: per-source-type
// `MATCH ... AGAINST (? IN NATURAL LANGUAGE MODE)` over the disposable Dolt
// store's FULLTEXT-keyed tables.
type fulltextGatherer struct {
	db *sql.DB
}

func newFulltextGatherer(db *sql.DB) *fulltextGatherer { return &fulltextGatherer{db: db} }

func (g *fulltextGatherer) Name() string { return "FULLTEXT" }

// fulltextTable maps a source type to its table and the FULLTEXT-keyed
// columns MATCH() searches, mirroring PRD §6.1's FULLTEXT KEY declarations.
var fulltextTable = map[string]struct {
	table   string
	columns string
}{
	"fact":      {"facts", "`key`, value"},
	"decision":  {"decisions", "title, rationale"},
	"task":      {"tasks", "title, notes"},
	"doc_chunk": {"doc_chunks", "heading_path, body"},
}

func (g *fulltextGatherer) Gather(ctx context.Context, sourceType, query string, limit int) ([]lexicalHit, error) {
	t, ok := fulltextTable[sourceType]
	if !ok {
		return nil, fmt.Errorf("fulltext: unknown source type %q", sourceType)
	}
	sqlText := fmt.Sprintf(
		"SELECT id, MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE) AS score "+
			"FROM %s WHERE MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE) "+
			"ORDER BY score DESC LIMIT ?",
		t.columns, t.table, t.columns)
	rows, err := g.db.QueryContext(ctx, sqlText, query, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fulltext: query %s: %w", t.table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []lexicalHit
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("fulltext: scan %s: %w", t.table, err)
		}
		out = append(out, lexicalHit{id: id, raw: score})
	}
	return out, rows.Err()
}
