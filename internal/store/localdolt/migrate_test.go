package localdolt_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// TestMigrateAppliesTheSchemaAsOneTaggedCommitPerMigration covers PRD
// §6.4's runner: every migration the binary ships is applied on main as
// exactly one Dolt commit tagged migration/<n>, meta.schema_version ends at
// the newest migration, and a second run is a no-op that adds nothing to
// the Dolt history.
func TestMigrateAppliesTheSchemaAsOneTaggedCommitPerMigration(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, baseDir(t), discardLogger())
	latest := store.LatestSchemaVersion()

	// A store with a database but no schema is version 0, and its history
	// holds only Dolt's own "Initialize data repository" commit.
	before, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version of a fresh store: %v", err)
	}
	if before != 0 {
		t.Fatalf("schema version of a fresh store = %d, want 0", before)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != 1 {
		t.Fatalf("fresh store has %d commits, want 1", got)
	}

	result, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if result.Version != latest {
		t.Fatalf("schema version after migrate = %d, want %d", result.Version, latest)
	}
	if len(result.Applied) != latest {
		t.Fatalf("migrate applied %d migrations, want %d", len(result.Applied), latest)
	}

	// PRD §6.4: migrations apply on main, one commit each, tagged.
	if got := scanString(t, st, "SELECT active_branch()"); got != localdolt.MainBranch {
		t.Fatalf("active branch = %q, want %q", got, localdolt.MainBranch)
	}
	for i, applied := range result.Applied {
		if applied.Version != i+1 {
			t.Fatalf("applied[%d].Version = %d, want %d", i, applied.Version, i+1)
		}
		if applied.Commit == "" {
			t.Fatalf("migration %d reported no commit hash", applied.Version)
		}
		tag := store.MigrationTag(applied.Version)
		if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", applied.Commit); got != 1 {
			t.Fatalf("migration %d's commit %s appears %d times in main's history, want 1",
				applied.Version, applied.Commit, got)
		}
		if got := scanString(t, st, "SELECT tag_hash FROM dolt_tags WHERE tag_name = ?", tag); got != applied.Commit {
			t.Fatalf("tag %q names commit %q, want migration %d's commit %q",
				tag, got, applied.Version, applied.Commit)
		}
	}

	// One commit per migration on top of the repository's own first commit,
	// and one tag per migration — no extra commit for the tagging itself.
	wantCommits := 1 + latest
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != wantCommits {
		t.Fatalf("history has %d commits after migrating, want %d", got, wantCommits)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_tags"); got != latest {
		t.Fatalf("history has %d tags after migrating, want %d", got, latest)
	}

	// meta.schema_version is the runner's key, and it is what a later run
	// reads back.
	if got := scanString(t, st, "SELECT v FROM meta WHERE k = ?", store.SchemaVersionKey); got != strconv.Itoa(latest) {
		t.Fatalf("meta.%s = %q, want %q", store.SchemaVersionKey, got, strconv.Itoa(latest))
	}
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version after migrate: %v", err)
	}
	if version != latest {
		t.Fatalf("schema version after migrate = %d, want %d", version, latest)
	}

	// Idempotence: a second run applies nothing and writes nothing.
	second, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Fatalf("second migrate applied %d migrations, want 0", len(second.Applied))
	}
	if second.Version != latest {
		t.Fatalf("second migrate reported version %d, want %d", second.Version, latest)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != wantCommits {
		t.Fatalf("second migrate changed the history to %d commits, want %d", got, wantCommits)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_tags"); got != latest {
		t.Fatalf("second migrate changed the tags to %d, want %d", got, latest)
	}

	// A tag-only failure is repaired in place: migration 1's tag is put
	// back on its original commit without replaying the migration.
	missingTag := store.MigrationTag(1)
	missingCommit := result.Applied[0].Commit
	execQuery(t, st, "CALL DOLT_TAG('-d', ?)", missingTag)
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_tags"); got != latest-1 {
		t.Fatalf("deleting %q left %d tags, want %d", missingTag, got, latest-1)
	}
	repaired, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate with missing tag: %v", err)
	}
	if len(repaired.Applied) != 0 {
		t.Fatalf("missing-tag repair applied %d migrations, want 0", len(repaired.Applied))
	}
	if got := scanString(t, st, "SELECT tag_hash FROM dolt_tags WHERE tag_name = ?", missingTag); got != missingCommit {
		t.Fatalf("repaired tag %q names commit %q, want original commit %q", missingTag, got, missingCommit)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log"); got != wantCommits {
		t.Fatalf("missing-tag repair changed the history to %d commits, want %d", got, wantCommits)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM dolt_tags"); got != latest {
		t.Fatalf("missing-tag repair left %d tags, want %d", got, latest)
	}
}

// TestMigratedSchemaMatchesThePRDSchema covers PRD §6.1: the tables the
// schema ships, the CHAR(26) ULID primary keys on the agent-writable ones,
// facts' STORED generated live_key with its unique key, and the measured
// column order of every FULLTEXT key.
func TestMigratedSchemaMatchesThePRDSchema(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, baseDir(t), discardLogger())
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A CHAR(26) ULID primary key on every agent-writable table (PRD §6.1).
	// commands is keyed by its kind enum and meta by its k string; neither
	// takes a ULID, and both are listed below with the key they do carry.
	const ulidPK = "id char(26) NOT NULL"
	if got := scanString(t, st,
		"SELECT column_type FROM information_schema.columns WHERE table_schema = ? AND table_name = 'facts' AND column_name = 'key'",
		localdolt.DatabaseName); got != "varchar(255)" {
		t.Fatalf("facts.key type = %q, want varchar(255)", got)
	}

	for _, want := range []struct {
		table     string
		fragments []string
	}{
		{"facts", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"value text",
			"source varchar(64) DEFAULT 'user'",
			"kind varchar(64)",
			"evidence varchar(1024)",
			"verified_at datetime",
			"created_at datetime",
			"superseded_by char(26)",
			// STORED, not VIRTUAL: a VIRTUAL generated column enforces the
			// same uniqueness but silently empties MATCH … AGAINST on the
			// table (docs/spikes/m1-fact-key-uniqueness.md).
			"live_key varchar(255) GENERATED ALWAYS AS (if(superseded_by IS NULL,key,NULL)) STORED",
			"UNIQUE KEY uk_fact_live_key (live_key)",
			"KEY idx_fact_key (key)",
			// Leading column value: the measured Dolt v1.88.1 order that
			// keeps dotted `key` prefix filters usable (PRD §6.1).
			"FULLTEXT KEY ft_facts (value,key)",
		}},
		{"decisions", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"title varchar(512)",
			"rationale text",
			"summary text",
			"alternatives_rejected text",
			"evidence varchar(1024)",
			"status enum('active','superseded','draft')",
			"source varchar(64)",
			"decided_at datetime",
			"superseded_by char(26)",
			"FULLTEXT KEY ft_decisions (title,rationale)",
		}},
		{"tasks", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"title varchar(512)",
			"status enum('open','done','blocked')",
			"notes text",
			"created_at datetime",
			"updated_at datetime",
			"FULLTEXT KEY ft_tasks (title,notes)",
		}},
		{"session_notes", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"actor varchar(64)",
			"actor_raw varchar(255)",
			"text text",
			"session_id varchar(255)",
			"agent_id varchar(255)",
			"provider_id varchar(255)",
			"model_id varchar(255)",
			"variant varchar(255)",
			"created_at datetime",
			"FULLTEXT KEY ft_notes (text)",
		}},
		{"commands", []string{
			"kind enum('build','test','run','lint','other') NOT NULL",
			"PRIMARY KEY (kind)",
			"cmdline text",
			"last_exit_code int",
			"last_run_at datetime",
			"success_count int",
			"fail_count int",
		}},
		{"project_state", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"body text",
			"actor varchar(64)",
			"actor_raw varchar(255)",
			"created_at datetime",
		}},
		{"project_arch", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"body text",
			"actor varchar(64)",
			"actor_raw varchar(255)",
			"created_at datetime",
		}},
		{"documents", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"path varchar(1024)",
			"title varchar(512)",
			"content_hash char(64)",
			"byte_len bigint",
			"source varchar(64)",
			"ingested_at datetime",
			"UNIQUE KEY uk_doc_path (path)",
		}},
		{"doc_chunks", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"doc_id char(26)",
			"ord int",
			"heading_path varchar(1024)",
			"body text",
			"UNIQUE KEY uk_chunk (doc_id,ord)",
			"FULLTEXT KEY ft_chunks (heading_path,body)",
			"FOREIGN KEY (doc_id) REFERENCES documents (id) ON DELETE CASCADE",
		}},
		{"meta", []string{
			"k varchar(64) NOT NULL",
			"PRIMARY KEY (k)",
			"v text",
		}},
		// PRD §6.2's review metadata, added by migration 2 rather than by
		// §6.1's DDL block. Its id is the proposal's ULID and the suffix of
		// the branch that carries it (PRD §3.1).
		{"proposals", []string{
			ulidPK,
			"PRIMARY KEY (id)",
			"kind enum('fact','decision','supersede')",
			"rationale text NOT NULL",
			"actor varchar(64) NOT NULL",
			"created_at datetime",
			"target enum('repo','global') NOT NULL",
		}},
	} {
		ddl := showCreateTable(t, st, want.table)
		for _, fragment := range want.fragments {
			if !strings.Contains(ddl, normalizeDDL(fragment)) {
				t.Errorf("table %s is missing %q; its schema is:\n%s", want.table, fragment, ddl)
			}
		}
	}
}

func TestProposalMetadataNullabilityIsEnforcedOnFreshAndUpgradedStores(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(*testing.T) *localdolt.Store
	}{
		{name: "fresh", store: migratedStore},
		{name: "upgraded from migration 2", store: storeAtMigrationTwoThenUpgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.store(t)
			for _, column := range []string{"rationale", "actor", "target"} {
				values := map[string]any{
					"rationale": "why",
					"actor":     "agent:test",
					"target":    "repo",
				}
				values[column] = nil
				_, err := st.Commit(context.Background(), store.CommitRequest{
					Statements: []store.Statement{{
						SQL:  "INSERT INTO proposals (id, kind, rationale, actor, created_at, target) VALUES (?, 'fact', ?, ?, '2026-08-15 12:00:00', ?)",
						Args: []any{"01J000000000000000000NULL", values["rationale"], values["actor"], values["target"]},
					}},
					NoText: true, Message: "probe proposal nullability", Author: stagingActor,
				})
				if err == nil {
					t.Errorf("proposals.%s accepted NULL", column)
				}
			}
		})
	}
}

func TestSessionNoteProvenanceUpgradesAnOlderStoreWithoutChangingNotes(t *testing.T) {
	ctx := context.Background()
	st := storeAtMigration(t, 3)
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO session_notes (id, actor, actor_raw, text, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{"01J0000000000000000000000", "agent:codex", "codex", "legacy note", "2026-08-30 10:00:00"},
		}},
		Text: []string{"codex", "legacy note"}, Message: "seed a pre-provenance note", Author: stagingActor,
	}); err != nil {
		t.Fatalf("seed migration-3 note: %v", err)
	}

	result, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade migration-3 store: %v", err)
	}
	if len(result.Applied) != 1 || result.Applied[0].Version != 4 {
		t.Fatalf("upgrade applied %+v, want migration 4 only", result.Applied)
	}
	if got := scanString(t, st, "SELECT text FROM session_notes WHERE id = ?", "01J0000000000000000000000"); got != "legacy note" {
		t.Fatalf("legacy note text = %q, want unchanged", got)
	}
	if got := scanString(t, st, "SELECT actor FROM session_notes WHERE id = ?", "01J0000000000000000000000"); got != "agent:codex" {
		t.Fatalf("legacy note actor = %q, want unchanged", got)
	}
	if got := scanString(t, st, "SELECT actor_raw FROM session_notes WHERE id = ?", "01J0000000000000000000000"); got != "codex" {
		t.Fatalf("legacy note actor_raw = %q, want unchanged", got)
	}
	if got := scanInt(t, st, "SELECT COUNT(*) FROM session_notes WHERE session_id IS NULL AND agent_id IS NULL AND provider_id IS NULL AND model_id IS NULL AND variant IS NULL"); got != 1 {
		t.Fatalf("legacy note has %d all-NULL provenance rows, want 1", got)
	}

	actor := memory.Actor{Name: "agent:opencode", Raw: "cli"}
	written, _, err := memory.New(st, actor).LogNoteWithProvenance(ctx, "verified note", memory.NoteProvenance{
		SessionID: "ses_current", AgentID: "build", ProviderID: "openai",
		ModelID: "gpt-5.6", Variant: "xhigh",
	})
	if err != nil {
		t.Fatalf("write a provenance note: %v", err)
	}
	listed, err := memory.New(st, actor).Notes(ctx, 2)
	if err != nil {
		t.Fatalf("list provenance notes: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != written.ID || listed[0].SessionID != "ses_current" ||
		listed[0].AgentID != "build" || listed[0].ProviderID != "openai" ||
		listed[0].ModelID != "gpt-5.6" || listed[0].Variant != "xhigh" {
		t.Fatalf("listed provenance = %+v, want exact verified fields", listed)
	}

	commits := scanInt(t, st, "SELECT COUNT(*) FROM dolt_log")
	second, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	if len(second.Applied) != 0 || scanInt(t, st, "SELECT COUNT(*) FROM dolt_log") != commits {
		t.Fatalf("second upgrade = %+v and changed history; want idempotent no-op", second)
	}
}

func storeAtMigrationTwoThenUpgrade(t *testing.T) *localdolt.Store {
	t.Helper()
	st := storeAtMigration(t, 2)
	result, err := st.Migrate(context.Background())
	if err != nil {
		t.Fatalf("upgrade migration-2 store: %v", err)
	}
	if len(result.Applied) != store.LatestSchemaVersion()-2 || result.Applied[0].Version != 3 ||
		result.Applied[len(result.Applied)-1].Version != store.LatestSchemaVersion() {
		t.Fatalf("upgrade applied %+v, want every migration after 2", result.Applied)
	}
	return st
}

func storeAtMigration(t *testing.T, version int) *localdolt.Store {
	t.Helper()
	ctx := context.Background()
	st := openStore(t, baseDir(t), discardLogger())
	for _, migration := range store.Migrations()[:version] {
		statements := append([]store.Statement{}, migration.Statements...)
		version := strconv.Itoa(migration.Version)
		statements = append(statements, store.Statement{
			SQL:  "INSERT INTO meta (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = ?",
			Args: []any{store.SchemaVersionKey, version, version},
		})
		commit, err := st.Commit(ctx, store.CommitRequest{
			Statements: statements, NoText: true,
			Message: fmt.Sprintf("migrate schema to v%d (%s)", migration.Version, migration.Name), Author: stagingActor,
		})
		if err != nil {
			t.Fatalf("apply migration %d: %v", migration.Version, err)
		}
		execQuery(t, st, "CALL DOLT_TAG(?, ?)", store.MigrationTag(migration.Version), commit.Hash)
	}
	return st
}

// TestOpenRefusesAStoreNewerThanTheBinary covers PRD §6.4's version-skew
// refusal: a store whose meta.schema_version is past the newest migration
// this binary ships is refused with an upgrade instruction, and the caller
// is left with no handle through which memory data could be read or
// written.
func TestOpenRefusesAStoreNewerThanTheBinary(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Exactly what a newer memdolt would leave behind: a schema version
	// past everything this binary knows how to read.
	newer := strconv.Itoa(store.LatestSchemaVersion() + 1)
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "UPDATE meta SET v = ? WHERE k = ?", Args: []any{newer, store.SchemaVersionKey}},
		},
		NoText:  true,
		Message: "record a schema version a newer memdolt migrated to",
		Author:  store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("record a newer schema version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := newStore(t, base)
	err := second.Open(ctx)
	if !errors.Is(err, store.ErrSchemaTooNew) {
		_ = second.Close()
		t.Fatalf("open error = %v, want one matching store.ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), "memdolt upgrade") {
		t.Fatalf("refusal %q does not tell the operator to upgrade", err)
	}

	// Refused means refused: there is no handle to read or write memory
	// data with, not even the migration runner's.
	if _, queryErr := second.Query(ctx, "SELECT COUNT(*) FROM facts"); !errors.Is(queryErr, store.ErrNotOpen) {
		t.Fatalf("query after a refused open = %v, want one matching store.ErrNotOpen", queryErr)
	}
	if _, commitErr := second.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{SQL: "INSERT INTO facts (id) VALUES ('01J000000000000000000000')"}},
		NoText:     true,
		Message:    "write to a store this binary must not touch",
		Author:     store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); !errors.Is(commitErr, store.ErrNotOpen) {
		t.Fatalf("commit after a refused open = %v, want one matching store.ErrNotOpen", commitErr)
	}
	if _, migrateErr := second.Migrate(ctx); !errors.Is(migrateErr, store.ErrNotOpen) {
		t.Fatalf("migrate after a refused open = %v, want one matching store.ErrNotOpen", migrateErr)
	}

	// The refusal releases the single-owner lock rather than stranding it:
	// the next attempt is refused for being too new, not for being locked.
	third := newStore(t, base)
	err = third.Open(ctx)
	if !errors.Is(err, store.ErrSchemaTooNew) {
		_ = third.Close()
		t.Fatalf("second open error = %v, want one matching store.ErrSchemaTooNew", err)
	}
	if errors.Is(err, store.ErrLocked) {
		t.Fatalf("the refused open left the store locked: %v", err)
	}
}

// TestMigrateRefusesToRunOffMain covers PRD §6.4's "migrations applied on
// main only": a store whose sessions land on another branch is refused
// rather than migrated where main would never see it.
func TestMigrateRefusesToRunOffMain(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)

	// A Dolt database's default branch is a process-global setting keyed by
	// database name, and every store in this package uses the same name, so
	// leaving it set would send later tests' sessions to a branch their own
	// repositories do not have. Registered before any store is opened so
	// that it runs last (t.Cleanup is LIFO) — by then every store below is
	// closed and this one can take the single-owner lock.
	t.Cleanup(func() {
		st := newStore(t, base)
		if err := st.Open(ctx); err != nil {
			t.Errorf("reopen to restore the default branch: %v", err)
			return
		}
		defer func() { _ = st.Close() }()
		execQuery(t, st, "SET @@GLOBAL.memory_default_branch = 'main'")
	})

	onMain := openStore(t, base, discardLogger())
	execQuery(t, onMain, "CALL DOLT_BRANCH('migration-probe')")
	execQuery(t, onMain, "SET @@GLOBAL.memory_default_branch = 'migration-probe'")
	if err := onMain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	offMain := openStore(t, base, discardLogger())
	if got := scanString(t, offMain, "SELECT active_branch()"); got != "migration-probe" {
		t.Fatalf("active branch = %q, want %q", got, "migration-probe")
	}

	_, err := offMain.Migrate(ctx)
	if err == nil {
		t.Fatal("migrate ran off main")
	}
	for _, want := range []string{localdolt.MainBranch, "migration-probe"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}

	// Refused before it wrote anything: no tables, no commit.
	if got := scanInt(t, offMain,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?", localdolt.DatabaseName); got != 0 {
		t.Fatalf("the refused migration created %d tables, want 0", got)
	}
	if got := scanInt(t, offMain, "SELECT COUNT(*) FROM dolt_log"); got != 1 {
		t.Fatalf("the refused migration left %d commits, want 1", got)
	}
}

// newStore builds an unopened store rooted at base. Unlike openStore it
// neither opens the store nor registers a cleanup, so a test can observe
// what Open itself does.
func newStore(t *testing.T, base string) *localdolt.Store {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   store.Actor{Name: "user", Email: "user@memdolt.invalid"},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return st
}

// showCreateTable returns table's DDL, normalized so that assertions can be
// written in readable SQL rather than against Dolt's exact spacing and
// quoting.
func showCreateTable(t *testing.T, st *localdolt.Store, table string) string {
	t.Helper()
	// table is one of this test's own literals, never external input.
	rows, err := st.Query(context.Background(), fmt.Sprintf("SHOW CREATE TABLE `%s`", table))
	if err != nil {
		t.Fatalf("show create table %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("table %s does not exist (err %v)", table, rows.Err())
	}
	var name, ddl string
	if err := rows.Scan(&name, &ddl); err != nil {
		t.Fatalf("scan show create table %s: %v", table, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("show create table %s: %v", table, err)
	}
	return normalizeDDL(ddl)
}

// normalizeDDL strips the quoting and layout of a CREATE TABLE statement,
// leaving the parts an assertion is about: identifiers, types, and the
// column order of each key.
func normalizeDDL(ddl string) string {
	ddl = strings.ReplaceAll(ddl, "`", "")
	ddl = strings.Join(strings.Fields(ddl), " ")
	return strings.ReplaceAll(ddl, ", ", ",")
}

func execQuery(t *testing.T, st *localdolt.Store, query string, args ...any) {
	t.Helper()
	if err := st.RunOnBranch(context.Background(), localdolt.MainBranch, store.Statement{SQL: query, Args: args}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func scanInt(t *testing.T, st *localdolt.Store, query string, args ...any) int {
	t.Helper()
	var value int
	scanOne(t, st, &value, query, args...)
	return value
}

func scanString(t *testing.T, st *localdolt.Store, query string, args ...any) string {
	t.Helper()
	var value string
	scanOne(t, st, &value, query, args...)
	return value
}

func scanOne(t *testing.T, st *localdolt.Store, dest any, query string, args ...any) {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("%s: no rows (err %v)", query, rows.Err())
	}
	if err := rows.Scan(dest); err != nil {
		t.Fatalf("%s: scan: %v", query, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", query, err)
	}
}
