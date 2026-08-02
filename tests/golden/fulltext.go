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
	"fact":      {"facts", "value, `key`"},
	"decision":  {"decisions", "title, rationale"},
	"task":      {"tasks", "title, notes"},
	"doc_chunk": {"doc_chunks", "heading_path, body"},
}

// Gather queries Dolt's FULLTEXT index in natural-language mode and dedupes
// the result before applying limit.
//
// A `MATCH ... AGAINST` filter in a WHERE clause does not return one row
// per matching *document* — it returns one row per matching *index term*,
// so a row whose FULLTEXT-keyed columns contain several of the query's
// words comes back several times, once per matched word, each copy
// carrying the identical accumulated relevancy score (confirmed empirically
// during PR #16's review: 31 rows / 14 distinct ids on one decision-type
// query in this rig's own fixture — see docs/spikes/m0-rig3.md §4). A bare
// `LIMIT 50` therefore caps *result rows*, not distinct candidates — at 21
// seeded rows the fan-out never starved the pool, but at a real project's
// row count it would silently drop distinct rows whose match happened to
// land past the row-not-candidate limit. The GROUP BY below collapses the
// fan-out to one row per id (keeping the max score, which is also the only
// score — every duplicate carries the same one) before LIMIT ever applies.
func (g *fulltextGatherer) Gather(ctx context.Context, sourceType, query string, limit int) ([]lexicalHit, error) {
	t, ok := fulltextTable[sourceType]
	if !ok {
		return nil, fmt.Errorf("fulltext: unknown source type %q", sourceType)
	}
	sqlText := fmt.Sprintf(
		"SELECT id, MAX(score) AS score FROM ("+
			"SELECT id, MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE) AS score "+
			"FROM %s WHERE MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE)"+
			") AS matches "+
			"GROUP BY id ORDER BY score DESC LIMIT ?",
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
