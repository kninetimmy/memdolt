package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

const recallFTSLimit = 50

// RecallSources returns the committed rows recall may hydrate. Like
// EmbeddingSources, this named reader is pinned to main's HEAD and never sees
// proposal branches or uncommitted working-set changes.
func (s *Store) RecallSources(ctx context.Context) ([]store.RecallSource, error) {
	conn, err := s.committedMainConn(ctx, "recall sources")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	var sources []store.RecallSource
	if err := appendFactSources(ctx, conn, &sources); err != nil {
		return nil, err
	}
	if err := appendDecisionSources(ctx, conn, &sources); err != nil {
		return nil, err
	}
	if err := appendTaskSources(ctx, conn, &sources); err != nil {
		return nil, err
	}
	if err := appendDocumentSources(ctx, conn, &sources); err != nil {
		return nil, err
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].SourceType != sources[j].SourceType {
			return sources[i].SourceType < sources[j].SourceType
		}
		return sources[i].SourceID < sources[j].SourceID
	})
	return sources, nil
}

// RecallFTS gathers at most 50 distinct rows per requested source type. The
// GROUP BY is before LIMIT because Dolt FULLTEXT returns one row per matched
// index term, not one row per matching document (PRD §8.1).
func (s *Store) RecallFTS(ctx context.Context, query string, sourceTypes []string) ([]store.LexicalHit, error) {
	conn, err := s.committedMainConn(ctx, "recall full-text gather")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	type fulltextTable struct{ table, columns string }
	tables := map[string]fulltextTable{
		"fact":      {"facts", "value, `key`"},
		"decision":  {"decisions", "title, rationale"},
		"task":      {"tasks", "title, notes"},
		"doc_chunk": {"doc_chunks", "heading_path, body"},
	}
	seenTypes := map[string]bool{}
	var hits []store.LexicalHit
	for _, sourceType := range sourceTypes {
		if seenTypes[sourceType] {
			continue
		}
		seenTypes[sourceType] = true
		table, ok := tables[sourceType]
		if !ok {
			return nil, fmt.Errorf("localdolt: unsupported recall source type %q", sourceType)
		}
		statement := fmt.Sprintf(
			"SELECT id, MAX(score) AS score FROM ("+
				"SELECT id, MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE) AS score "+
				"FROM %s AS OF 'HEAD' WHERE MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE)"+
				") AS matches GROUP BY id ORDER BY score DESC, id LIMIT ?",
			table.columns, table.table, table.columns)
		rows, err := conn.QueryContext(ctx, statement, query, query, recallFTSLimit)
		if err != nil {
			return nil, fmt.Errorf("localdolt: full-text gather %s: %w", sourceType, err)
		}
		for rows.Next() {
			var hit store.LexicalHit
			hit.SourceType = sourceType
			if err := rows.Scan(&hit.SourceID, &hit.Score); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("localdolt: scan full-text %s hit: %w", sourceType, err)
			}
			hits = append(hits, hit)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("localdolt: read full-text %s hits: %w", sourceType, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("localdolt: close full-text %s hits: %w", sourceType, err)
		}
	}
	return hits, nil
}

// LastChanged returns optional row-level provenance from the matching
// dolt_blame table. The table switch is the allowlist; no caller text is ever
// interpolated into SQL.
func (s *Store) LastChanged(ctx context.Context, sourceType, sourceID string) (*store.CommitProvenance, error) {
	table, ok := map[string]string{
		"fact":      "dolt_blame_facts",
		"decision":  "dolt_blame_decisions",
		"task":      "dolt_blame_tasks",
		"doc_chunk": "dolt_blame_doc_chunks",
	}[sourceType]
	if !ok {
		return nil, fmt.Errorf("localdolt: unsupported provenance source type %q", sourceType)
	}
	conn, err := s.committedMainConn(ctx, "recall provenance")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var changed store.CommitProvenance
	err = conn.QueryRowContext(ctx,
		"SELECT commit, committer, commit_date FROM "+table+" WHERE id = ?", sourceID).
		Scan(&changed.Hash, &changed.Author, &changed.Date)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localdolt: read %s/%s provenance: %w", sourceType, sourceID, err)
	}
	return &changed, nil
}

func (s *Store) committedMainConn(ctx context.Context, purpose string) (*sql.Conn, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("localdolt: acquire connection for %s: %w", purpose, err)
	}
	branch, err := activeBranch(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if branch != MainBranch {
		_ = conn.Close()
		return nil, fmt.Errorf("localdolt: %s is read from committed %q only; the store is on %q", purpose, MainBranch, branch)
	}
	return conn, nil
}

func appendFactSources(ctx context.Context, conn *sql.Conn, out *[]store.RecallSource) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, COALESCE(`+"`key`"+`, ''), COALESCE(value, ''),
COALESCE(source, ''), COALESCE(kind, ''), verified_at, created_at, COALESCE(superseded_by, '')
FROM facts AS OF 'HEAD' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("localdolt: read fact recall sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source store.RecallSource
		var verified, created sql.NullTime
		source.SourceType = "fact"
		if err := rows.Scan(&source.SourceID, &source.Title, &source.Body, &source.Source,
			&source.Kind, &verified, &created, &source.SupersededBy); err != nil {
			return fmt.Errorf("localdolt: scan fact recall source: %w", err)
		}
		source.VerifiedAt = timePointer(verified)
		source.CreatedAt = timeValue(created)
		*out = append(*out, source)
	}
	return rows.Err()
}

func appendDecisionSources(ctx context.Context, conn *sql.Conn, out *[]store.RecallSource) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, COALESCE(title, ''), COALESCE(rationale, ''),
COALESCE(summary, ''), COALESCE(source, ''), COALESCE(status, ''), decided_at, COALESCE(superseded_by, '')
FROM decisions AS OF 'HEAD' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("localdolt: read decision recall sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source store.RecallSource
		var decided sql.NullTime
		source.SourceType = "decision"
		if err := rows.Scan(&source.SourceID, &source.Title, &source.Body, &source.Summary,
			&source.Source, &source.Status, &decided, &source.SupersededBy); err != nil {
			return fmt.Errorf("localdolt: scan decision recall source: %w", err)
		}
		source.CreatedAt = timeValue(decided)
		*out = append(*out, source)
	}
	return rows.Err()
}

func appendTaskSources(ctx context.Context, conn *sql.Conn, out *[]store.RecallSource) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, COALESCE(title, ''), COALESCE(notes, ''),
COALESCE(status, ''), created_at, updated_at FROM tasks AS OF 'HEAD' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("localdolt: read task recall sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source store.RecallSource
		var created, updated sql.NullTime
		source.SourceType = "task"
		if err := rows.Scan(&source.SourceID, &source.Title, &source.Body, &source.Status,
			&created, &updated); err != nil {
			return fmt.Errorf("localdolt: scan task recall source: %w", err)
		}
		source.CreatedAt = timeValue(created)
		source.UpdatedAt = timePointer(updated)
		*out = append(*out, source)
	}
	return rows.Err()
}

func appendDocumentSources(ctx context.Context, conn *sql.Conn, out *[]store.RecallSource) error {
	type document struct {
		title, source string
		ingested      time.Time
	}
	documents := map[string]document{}
	rows, err := conn.QueryContext(ctx, `SELECT id, COALESCE(title, ''), COALESCE(source, ''), ingested_at
FROM documents AS OF 'HEAD' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("localdolt: read recall documents: %w", err)
	}
	for rows.Next() {
		var id string
		var doc document
		var ingested sql.NullTime
		if err := rows.Scan(&id, &doc.title, &doc.source, &ingested); err != nil {
			_ = rows.Close()
			return fmt.Errorf("localdolt: scan recall document: %w", err)
		}
		doc.ingested = timeValue(ingested)
		documents[id] = doc
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("localdolt: read recall documents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("localdolt: close recall documents: %w", err)
	}

	rows, err = conn.QueryContext(ctx, `SELECT id, doc_id, COALESCE(heading_path, ''), COALESCE(body, '')
FROM doc_chunks AS OF 'HEAD' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("localdolt: read document-chunk recall sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source store.RecallSource
		var docID sql.NullString
		var heading string
		source.SourceType = "doc_chunk"
		if err := rows.Scan(&source.SourceID, &docID, &heading, &source.Body); err != nil {
			return fmt.Errorf("localdolt: scan document-chunk recall source: %w", err)
		}
		var doc document
		if docID.Valid {
			var ok bool
			doc, ok = documents[docID.String]
			if !ok {
				return fmt.Errorf("localdolt: document chunk %s refers to missing committed document %s", source.SourceID, docID.String)
			}
		}
		switch {
		case doc.title == "":
			source.Title = heading
		case heading == "":
			source.Title = doc.title
		default:
			source.Title = doc.title + " — " + heading
		}
		source.Source = doc.source
		source.CreatedAt = doc.ingested
		*out = append(*out, source)
	}
	return rows.Err()
}

func timePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func timeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}
