package localdolt_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestFactKeyPrefixAndFulltextContract(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, baseDir(t), discardLogger())

	_, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: `CREATE TABLE facts (
				id CHAR(26) PRIMARY KEY,
				` + "`key`" + ` VARCHAR(255),
				value TEXT,
				superseded_by CHAR(26) NULL,
				live_key VARCHAR(255) GENERATED ALWAYS AS
					(IF(superseded_by IS NULL, ` + "`key`" + `, NULL)) STORED,
				UNIQUE KEY uk_fact_live_key (live_key),
				FULLTEXT KEY ft_facts (value, ` + "`key`" + `)
			)`},
			{SQL: `CREATE TABLE decisions (
				id CHAR(26) PRIMARY KEY,
				title VARCHAR(512),
				rationale TEXT,
				status ENUM('active','superseded','draft'),
				FULLTEXT KEY ft_decisions (title, rationale)
			)`},
			{SQL: `CREATE TABLE tasks (
				id CHAR(26) PRIMARY KEY,
				title VARCHAR(512),
				notes TEXT,
				FULLTEXT KEY ft_tasks (title, notes)
			)`},
			{SQL: `CREATE TABLE session_notes (
				id CHAR(26) PRIMARY KEY,
				text TEXT,
				FULLTEXT KEY ft_notes (text)
			)`},
			{SQL: `CREATE TABLE doc_chunks (
				id CHAR(26) PRIMARY KEY,
				heading_path VARCHAR(1024),
				body TEXT,
				FULLTEXT KEY ft_chunks (heading_path, body)
			)`},
			{SQL: `INSERT INTO facts (id, ` + "`key`" + `, value, superseded_by) VALUES
				('fact-build-old', 'build.command', 'legacy cargo command', 'fact-build-new'),
				('fact-build-new', 'build.command', 'current cargo command', NULL),
				('fact-build-cache', 'build.cache', 'reuse compiled artifacts', NULL),
				('fact-env-shell', 'env.shell', 'PowerShell', NULL)`},
			{SQL: `INSERT INTO decisions (id, title, rationale, status)
				VALUES ('decision-cargo', 'Choose Cargo', 'cargo is repeatable', 'active')`},
			{SQL: `INSERT INTO tasks (id, title, notes)
				VALUES ('task-cargo', 'Run Cargo', 'cargo test details')`},
			{SQL: `INSERT INTO session_notes (id, text)
				VALUES ('note-cargo', 'cargo session')`},
			{SQL: `INSERT INTO doc_chunks (id, heading_path, body)
				VALUES ('chunk-cargo', 'Build > Cargo', 'cargo reference')`},
		},
		Message: "seed FULLTEXT contract probe",
		Author:  store.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	})
	if err != nil {
		t.Fatalf("creating and seeding contract schema: %v", err)
	}

	requireIDs(t, st, []string{"fact-build-new", "fact-build-old"},
		"SELECT id FROM facts WHERE `key` = ? ORDER BY id", "build.command")
	requireIDs(t, st, []string{"fact-build-cache", "fact-build-new", "fact-build-old"},
		"SELECT id FROM facts WHERE `key` LIKE ? ORDER BY id", "build.%")
	requireIDs(t, st, []string{"fact-build-new", "fact-build-old"},
		`SELECT id FROM facts
		 WHERE MATCH(value, `+"`key`"+`) AGAINST (? IN NATURAL LANGUAGE MODE)
		 GROUP BY id ORDER BY id`, "cargo")

	for _, filter := range []struct {
		query      string
		arg        string
		expression string
	}{
		{"SELECT id FROM facts WHERE value = ?", "current cargo command", "*expression.Equals"},
		{"SELECT id FROM facts WHERE value LIKE ?", "current%", "*expression.GreaterThanOrEqual"},
		{"SELECT id FROM decisions WHERE title = ?", "Choose Cargo", "*expression.Equals"},
		{"SELECT id FROM decisions WHERE title LIKE ?", "Choose%", "*expression.GreaterThanOrEqual"},
		{"SELECT id FROM tasks WHERE title = ?", "Run Cargo", "*expression.Equals"},
		{"SELECT id FROM tasks WHERE title LIKE ?", "Run%", "*expression.GreaterThanOrEqual"},
		{"SELECT id FROM session_notes WHERE text = ?", "cargo session", "*expression.Equals"},
		{"SELECT id FROM session_notes WHERE text LIKE ?", "cargo%", "*expression.GreaterThanOrEqual"},
		{"SELECT id FROM doc_chunks WHERE heading_path = ?", "Build > Cargo", "*expression.Equals"},
		{"SELECT id FROM doc_chunks WHERE heading_path LIKE ?", "Build%", "*expression.GreaterThanOrEqual"},
	} {
		requireFulltextFilterError(t, st, filter.expression, filter.query, filter.arg)
	}

	requireIDs(t, st, []string{"decision-cargo"},
		"SELECT id FROM decisions WHERE rationale = ?", "cargo is repeatable")
	requireIDs(t, st, []string{"decision-cargo"},
		"SELECT id FROM decisions WHERE status = ?", "active")
}

func requireIDs(t *testing.T, st *localdolt.Store, want []string, query string, args ...any) {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", query, err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: ids = %v, want %v", query, got, want)
	}
}

func requireFulltextFilterError(t *testing.T, st *localdolt.Store, expression, query string, arg any) {
	t.Helper()
	rows, err := st.Query(context.Background(), query, arg)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
		}
		err = rows.Err()
	}
	if err == nil {
		t.Fatalf("%s: succeeded; want Dolt Error 1105", query)
	}
	want := "Error 1105: Full-Text index found in filter with unknown expression: " + expression
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error = %v, want one containing %q", query, err, want)
	}
}
