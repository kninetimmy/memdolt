//go:build golden

package golden

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaDDL is PRD §6.1's schema, trimmed to the tables this rig's golden
// queries actually touch (facts, decisions, tasks, session_notes,
// documents, doc_chunks — the five FULLTEXT-bearing source types plus
// doc_chunks' documents parent). One deviation from §6.1, explicitly
// licensed by this issue's acceptance criteria: a `scope` column on facts
// and decisions, so a global-store row can be seeded as a scope-tagged row
// in this same disposable store instead of standing up real global-store
// plumbing — M0 gates the retrieval math, not that plumbing.
var schemaDDL = []string{
	`CREATE TABLE facts (
		id CHAR(26) PRIMARY KEY,
		` + "`key`" + ` VARCHAR(255),
		value TEXT,
		source VARCHAR(64) DEFAULT 'user',
		kind VARCHAR(64) NULL,
		evidence VARCHAR(1024) NULL,
		scope VARCHAR(16) NOT NULL DEFAULT 'repo',
		verified_at DATETIME NULL,
		created_at DATETIME,
		superseded_by CHAR(26) NULL,
		UNIQUE KEY uk_fact_key (` + "`key`" + `),
		FULLTEXT KEY ft_facts (` + "`key`" + `, value)
	)`,
	`CREATE TABLE decisions (
		id CHAR(26) PRIMARY KEY,
		title VARCHAR(512),
		rationale TEXT,
		summary TEXT NULL,
		alternatives_rejected TEXT NULL,
		evidence VARCHAR(1024) NULL,
		status ENUM('active','superseded','draft'),
		source VARCHAR(64) NULL,
		scope VARCHAR(16) NOT NULL DEFAULT 'repo',
		decided_at DATETIME,
		superseded_by CHAR(26) NULL,
		FULLTEXT KEY ft_decisions (title, rationale)
	)`,
	`CREATE TABLE tasks (
		id CHAR(26) PRIMARY KEY,
		title VARCHAR(512),
		status ENUM('open','done','blocked'),
		notes TEXT NULL,
		created_at DATETIME,
		updated_at DATETIME,
		FULLTEXT KEY ft_tasks (title, notes)
	)`,
	`CREATE TABLE session_notes (
		id CHAR(26) PRIMARY KEY,
		actor VARCHAR(64),
		actor_raw VARCHAR(255),
		text TEXT,
		created_at DATETIME,
		FULLTEXT KEY ft_notes (text)
	)`,
	`CREATE TABLE documents (
		id CHAR(26) PRIMARY KEY,
		path VARCHAR(1024),
		title VARCHAR(512),
		content_hash CHAR(64),
		byte_len BIGINT,
		source VARCHAR(64),
		ingested_at DATETIME,
		UNIQUE KEY uk_doc_path (path)
	)`,
	`CREATE TABLE doc_chunks (
		id CHAR(26) PRIMARY KEY,
		doc_id CHAR(26),
		ord INT,
		heading_path VARCHAR(1024),
		body TEXT,
		UNIQUE KEY uk_chunk (doc_id, ord),
		FULLTEXT KEY ft_chunks (heading_path, body),
		FOREIGN KEY (doc_id) REFERENCES documents(id) ON DELETE CASCADE
	)`,
}

// createSchema runs schemaDDL against db, one statement per Exec (Dolt's
// embedded driver, like most database/sql drivers, does not accept
// multi-statement Exec calls).
func createSchema(ctx context.Context, db *sql.DB) error {
	for i, stmt := range schemaDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("golden: schema statement %d: %w", i, err)
		}
	}
	return nil
}
