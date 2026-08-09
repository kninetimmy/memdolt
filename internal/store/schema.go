package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// SchemaVersionKey is the meta row the migration runner is keyed on
// (PRD §6.4). Its value is the decimal version of the newest migration
// applied to a store.
const SchemaVersionKey = "schema_version"

// Migration is one step of memdolt's schema history: the statements that
// take a store from Version-1 to Version, applied as a unit.
//
// A migration carries schema changes only. The runner is what records
// meta.schema_version and what turns the applied statements into a single
// Dolt commit tagged migration/<Version> (PRD §6.4), so a migration here
// never writes that row itself.
type Migration struct {
	// Version is the schema version this migration produces. Versions are
	// 1-based and contiguous: migration n is applied to a store at n-1.
	Version int

	// Name is a short label for the commit message and CLI output.
	Name string

	// Statements are applied in order, in one transaction.
	Statements []Statement
}

// migrations is memdolt's schema history, oldest first. Migrations are
// append-only: an applied migration is never edited, because a store that
// already ran it will never run it again.
var migrations = []Migration{
	{Version: 1, Name: "initial schema", Statements: initialSchema},
}

// Migrations returns the schema history the binary ships, oldest first.
func Migrations() []Migration { return slices.Clone(migrations) }

// LatestSchemaVersion is the newest schema version this binary knows how to
// produce — the version a freshly initialized store ends up at.
func LatestSchemaVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}

// MigrationTag is the Dolt tag name a migration's commit carries
// (PRD §6.4). It is the one place that spelling is decided.
func MigrationTag(version int) string { return "migration/" + strconv.Itoa(version) }

// ErrSchemaTooNew reports a store written by a newer memdolt than this one.
// PRD §6.4: the client refuses to operate on such a store rather than
// reading or writing memory through a schema it does not know.
var ErrSchemaTooNew = errors.New("store schema is newer than this memdolt binary")

// CheckSchemaVersion reports whether this binary may operate on a store at
// storeVersion. A store older than the binary is fine — the migration
// runner brings it forward. A store newer than the binary is refused:
// nothing here can know what a later migration changed.
func CheckSchemaVersion(storeVersion int) error {
	latest := LatestSchemaVersion()
	if storeVersion > latest {
		return fmt.Errorf(
			"%w: store is at schema version %d, this binary knows %d; run \"memdolt upgrade\"",
			ErrSchemaTooNew, storeVersion, latest)
	}
	return nil
}

// initialSchema is PRD §6.1's schema, verbatim in content and load-bearing
// in detail:
//
//   - Agent-writable tables take CHAR(26) ULID primary keys rather than
//     AUTO_INCREMENT ids, so concurrent inserts on two machines can never
//     collide on a merge. That is every table here except commands, keyed
//     by its kind enum, and meta, keyed by its k string; neither is
//     agent-writable and neither takes a ULID.
//   - facts.live_key is a STORED generated column carrying key while the
//     row is live and NULL once it is superseded, so uk_fact_live_key
//     scopes uniqueness to live rows (supersession stays possible; §6.1).
//     STORED is not a preference: measured on Dolt v1.88.1, a VIRTUAL
//     generated column enforces the same uniqueness but silently makes
//     MATCH … AGAINST return nothing on the same table
//     (docs/spikes/m1-fact-key-uniqueness.md). That measurement is of
//     facts.live_key, the only generated column in this schema; it does
//     not establish how VIRTUAL behaves for generated columns in general,
//     so a future one needs its own check rather than this precedent.
//   - Every FULLTEXT key's column order is the measured compatibility
//     contract of §6.1, not a relevance preference: on Dolt v1.88.1 a
//     standalone equality or prefix filter on a FULLTEXT key's *leading*
//     column fails with Error 1105, so ft_facts leads with value to keep
//     the supported dotted `key` prefix filter usable. That restriction
//     belongs to the leading column of every FULLTEXT key here — ft_facts,
//     ft_decisions, ft_tasks, ft_notes, ft_chunks — not to facts.value
//     alone, and a B-tree on the same column does not lift it. Reordering
//     a key here changes which filters the client may issue.
//
// Statements run in order, so documents precedes the doc_chunks foreign key
// that references it.
var initialSchema = []Statement{
	{SQL: "CREATE TABLE facts (\n" +
		"  id CHAR(26) PRIMARY KEY,\n" +
		"  `key` VARCHAR(255),\n" +
		"  value TEXT,\n" +
		"  source VARCHAR(64) DEFAULT 'user',\n" +
		"  kind VARCHAR(64) NULL,\n" +
		"  evidence VARCHAR(1024) NULL,\n" +
		"  verified_at DATETIME NULL,\n" +
		"  created_at DATETIME,\n" +
		"  superseded_by CHAR(26) NULL,\n" +
		"  live_key VARCHAR(255) GENERATED ALWAYS AS (IF(superseded_by IS NULL, `key`, NULL)) STORED,\n" +
		"  KEY idx_fact_key (`key`),\n" +
		"  UNIQUE KEY uk_fact_live_key (live_key),\n" +
		"  FULLTEXT KEY ft_facts (value, `key`)\n" +
		")"},
	{SQL: `CREATE TABLE decisions (
  id CHAR(26) PRIMARY KEY,
  title VARCHAR(512),
  rationale TEXT,
  summary TEXT NULL,
  alternatives_rejected TEXT NULL,
  evidence VARCHAR(1024) NULL,
  status ENUM('active','superseded','draft'),
  source VARCHAR(64) NULL,
  decided_at DATETIME,
  superseded_by CHAR(26) NULL,
  FULLTEXT KEY ft_decisions (title, rationale)
)`},
	{SQL: `CREATE TABLE tasks (
  id CHAR(26) PRIMARY KEY,
  title VARCHAR(512),
  status ENUM('open','done','blocked'),
  notes TEXT NULL,
  created_at DATETIME,
  updated_at DATETIME,
  FULLTEXT KEY ft_tasks (title, notes)
)`},
	{SQL: `CREATE TABLE session_notes (
  id CHAR(26) PRIMARY KEY,
  actor VARCHAR(64),
  actor_raw VARCHAR(255),
  text TEXT,
  created_at DATETIME,
  FULLTEXT KEY ft_notes (text)
)`},
	{SQL: `CREATE TABLE commands (
  kind ENUM('build','test','run','lint','other') PRIMARY KEY,
  cmdline TEXT,
  last_exit_code INT,
  last_run_at DATETIME,
  success_count INT,
  fail_count INT
)`},
	{SQL: `CREATE TABLE project_state (
  id CHAR(26) PRIMARY KEY,
  body TEXT,
  actor VARCHAR(64),
  actor_raw VARCHAR(255),
  created_at DATETIME
)`},
	{SQL: `CREATE TABLE project_arch (
  id CHAR(26) PRIMARY KEY,
  body TEXT,
  actor VARCHAR(64),
  actor_raw VARCHAR(255),
  created_at DATETIME
)`},
	{SQL: `CREATE TABLE documents (
  id CHAR(26) PRIMARY KEY,
  path VARCHAR(1024),
  title VARCHAR(512),
  content_hash CHAR(64),
  byte_len BIGINT,
  source VARCHAR(64),
  ingested_at DATETIME,
  UNIQUE KEY uk_doc_path (path)
)`},
	{SQL: `CREATE TABLE doc_chunks (
  id CHAR(26) PRIMARY KEY,
  doc_id CHAR(26),
  ord INT,
  heading_path VARCHAR(1024),
  body TEXT,
  UNIQUE KEY uk_chunk (doc_id, ord),
  FULLTEXT KEY ft_chunks (heading_path, body),
  FOREIGN KEY (doc_id) REFERENCES documents(id) ON DELETE CASCADE
)`},
	{SQL: `CREATE TABLE meta (
  k VARCHAR(64) PRIMARY KEY,
  v TEXT
)`},
}
