package localdolt

import (
	"context"
	"fmt"

	"github.com/kninetimmy/memdolt/internal/store"
)

// EmbeddingSources returns every fact, decision, task, and document chunk in
// main's committed HEAD, rendered as PRD §8's embedding input. Proposal-branch
// rows and uncommitted working-set changes are deliberately ineligible.
//
// This is a typed read on LocalStore rather than a caller assembling SQL through
// Store.Query. It neither changes source rows nor writes a Dolt commit.
func (s *Store) EmbeddingSources(ctx context.Context) ([]store.EmbeddingSource, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("localdolt: acquire connection for embedding sources: %w", err)
	}
	defer func() { _ = conn.Close() }()
	branch, err := activeBranch(ctx, conn)
	if err != nil {
		return nil, err
	}
	if branch != MainBranch {
		return nil, fmt.Errorf("localdolt: embedding sources are read from committed %q only; the store is on %q", MainBranch, branch)
	}

	var sources []store.EmbeddingSource
	queries := []struct {
		sourceType string
		sql        string
		text       func(string, string, string) string
	}{
		{
			sourceType: "fact",
			sql:        "SELECT id, COALESCE(`key`, ''), COALESCE(value, ''), '' FROM facts AS OF 'HEAD' ORDER BY id",
			text: func(key, value, _ string) string {
				return key + ": " + value
			},
		},
		{
			sourceType: "decision",
			sql:        "SELECT id, COALESCE(title, ''), COALESCE(rationale, ''), COALESCE(summary, '') FROM decisions AS OF 'HEAD' ORDER BY id",
			text: func(title, rationale, summary string) string {
				if summary != "" {
					return summary + "\n\n" + title + "\n\n" + rationale
				}
				return title + "\n\n" + rationale
			},
		},
		{
			sourceType: "task",
			sql:        "SELECT id, COALESCE(title, ''), COALESCE(notes, ''), '' FROM tasks AS OF 'HEAD' ORDER BY id",
			text: func(title, notes, _ string) string {
				if notes != "" {
					return title + "\n\n" + notes
				}
				return title
			},
		},
		{
			sourceType: "doc_chunk",
			sql:        "SELECT id, COALESCE(heading_path, ''), COALESCE(body, ''), '' FROM doc_chunks AS OF 'HEAD' ORDER BY id",
			text: func(heading, body, _ string) string {
				if heading != "" {
					return heading + "\n\n" + body
				}
				return body
			},
		},
	}
	for _, query := range queries {
		rows, err := conn.QueryContext(ctx, query.sql)
		if err != nil {
			return nil, fmt.Errorf("localdolt: read %s embedding sources: %w", query.sourceType, err)
		}
		for rows.Next() {
			var id, first, second, third string
			if err := rows.Scan(&id, &first, &second, &third); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("localdolt: scan %s embedding source: %w", query.sourceType, err)
			}
			sources = append(sources, store.EmbeddingSource{
				SourceType: query.sourceType,
				SourceID:   id,
				Text:       query.text(first, second, third),
			})
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("localdolt: close %s embedding sources: %w", query.sourceType, err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("localdolt: read %s embedding sources: %w", query.sourceType, err)
		}
	}
	return sources, nil
}
