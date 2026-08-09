package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kninetimmy/memdolt/internal/store"
)

// MainBranch is the branch memdolt's durable memory lives on (PRD §3.1) and
// the only branch migrations are applied on (PRD §6.4).
const MainBranch = "main"

// metaTableName is the plumbing table meta.schema_version lives in
// (PRD §6.1).
const metaTableName = "meta"

// AppliedMigration describes one migration a Migrate call applied.
type AppliedMigration struct {
	// Version is the schema version the migration produced.
	Version int

	// Name is the migration's label, as it appears in the commit message.
	Name string

	// Commit is the hash of the single Dolt commit the migration produced;
	// it carries the tag store.MigrationTag(Version).
	Commit string
}

// MigrationResult reports what a Migrate call did.
type MigrationResult struct {
	// Version is the store's schema version afterwards.
	Version int

	// Applied lists the migrations this call applied, oldest first. An
	// empty Applied means the store was already at Version: Migrate wrote
	// nothing and added no commit to the Dolt history.
	Applied []AppliedMigration
}

// Migrate brings the store's schema up to the newest migration this binary
// ships and reports what it did (PRD §6.4).
//
// It is idempotent and keyed on meta.schema_version: every migration newer
// than that value is applied in order, each as exactly one Dolt commit
// tagged migration/<n>, and a store already at the newest version is left
// untouched — no statement runs, no commit is created.
//
// Two refusals, both loud:
//
//   - A store whose schema_version is newer than this binary knows is
//     refused with an error matching errors.Is(err, store.ErrSchemaTooNew).
//   - Migrations apply on main only, so a store whose session is on another
//     branch is refused rather than migrated where it would not be seen.
//     The check reads the branch the store's own connections are on; it
//     binds this process, not a foreign `dolt` CLI holding the data
//     directory open on another branch (the same boundary as the
//     single-owner lock, PRD §5.2).
func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	db, err := s.handle()
	if err != nil {
		return MigrationResult{}, err
	}

	branch, err := activeBranch(ctx, db)
	if err != nil {
		return MigrationResult{}, err
	}
	if branch != MainBranch {
		return MigrationResult{}, fmt.Errorf(
			"localdolt: migrations apply on %q only (PRD §6.4); the store at %s is on %q",
			MainBranch, s.paths.DoltDataDir(), branch)
	}

	current, err := schemaVersion(ctx, db)
	if err != nil {
		return MigrationResult{}, err
	}
	if err := store.CheckSchemaVersion(current); err != nil {
		return MigrationResult{Version: current}, fmt.Errorf("localdolt: %s: %w", s.paths.DoltDataDir(), err)
	}

	result := MigrationResult{Version: current}
	for _, migration := range store.Migrations() {
		if migration.Version <= current {
			continue
		}
		applied, err := s.applyMigration(ctx, db, migration)
		if err != nil {
			return result, err
		}
		result.Applied = append(result.Applied, applied)
		result.Version = migration.Version
	}
	return result, nil
}

// SchemaVersion reports the store's schema version — the highest migration
// applied to it. A store that has no meta table, or no schema_version row
// in it, is uninitialized and reports 0.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	db, err := s.handle()
	if err != nil {
		return 0, err
	}
	return schemaVersion(ctx, db)
}

// applyMigration runs one migration as a single Dolt commit and tags that
// commit migration/<n> (PRD §6.4).
//
// The schema_version row is written by the same transaction as the
// migration's own statements, so a committed migration and the version it
// claims are one commit or neither.
//
// The transaction alone does not make a *failed* migration disappear:
// measured on Dolt v1.88.1, rolling back a transaction that ran DDL leaves
// the created table in the working set. Left there it would break the
// re-run this runner's idempotence promises — migration 1 creating facts a
// second time fails with "table already exists" — and the next write's
// DOLT_COMMIT('-A') would sweep the orphan into an unrelated commit. So a
// failed migration is followed by discardUncommitted, which is what makes
// the all-or-nothing claim true for DDL as well.
//
// The tag is a separate call, because a Dolt tag names a commit that has to
// exist first; it adds no commit of its own.
func (s *Store) applyMigration(ctx context.Context, db *sql.DB, migration store.Migration) (AppliedMigration, error) {
	version := strconv.Itoa(migration.Version)
	statements := append(slices.Clone(migration.Statements), store.Statement{
		SQL:  "INSERT INTO " + metaTableName + " (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = ?",
		Args: []any{store.SchemaVersionKey, version, version},
	})

	result, err := s.Commit(ctx, store.CommitRequest{
		Statements: statements,
		Message:    fmt.Sprintf("migrate schema to v%s (%s)", version, migration.Name),
		Author:     s.cfg.Actor,
	})
	if err != nil {
		return AppliedMigration{}, errors.Join(
			fmt.Errorf("localdolt: apply migration %s (%s): %w", version, migration.Name, err),
			discardUncommitted(ctx, db))
	}

	tag := store.MigrationTag(migration.Version)
	if _, err := db.ExecContext(ctx, "CALL DOLT_TAG(?, ?)", tag, result.Hash); err != nil {
		return AppliedMigration{}, fmt.Errorf("localdolt: tag migration commit %s as %q: %w", result.Hash, tag, err)
	}

	return AppliedMigration{Version: migration.Version, Name: migration.Name, Commit: result.Hash}, nil
}

// discardUncommitted throws away everything uncommitted on the current
// branch, which is how the tables a failed migration created are undone —
// a transaction rollback does not undo DDL.
//
// It is safe here because it runs on a branch the caller has just failed to
// write and nothing else in the process is writing: the single-owner lock
// (PRD §5.2) makes this process the only writer, and a migration runs
// before any other work is in flight. It is not a general-purpose repair —
// called while another goroutine of this process held uncommitted work, it
// would discard that too.
//
// Two calls, for the same reason git needs two: measured on Dolt v1.88.1,
// DOLT_RESET('--hard') restores tracked tables but reports success while
// leaving a table the failed migration created — that table is new, not
// modified — and DOLT_CLEAN is what removes it.
func discardUncommitted(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{"CALL DOLT_RESET('--hard')", "CALL DOLT_CLEAN()"} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("localdolt: discard the failed migration's uncommitted changes (%s): %w", statement, err)
		}
	}
	return nil
}

// guardSchemaVersion is PRD §6.4's refusal, applied while the store is
// being opened: a store written by a newer memdolt is refused before
// anything can read or write memory through a schema this binary does not
// know. meta.schema_version is plumbing (PRD §6.1), not memory data.
//
// It runs inside Open, with s.mu already held and s.opened still false, so
// it reads s.db directly rather than through handle.
func (s *Store) guardSchemaVersion(ctx context.Context) error {
	version, err := schemaVersion(ctx, s.db)
	if err != nil {
		return err
	}
	if err := store.CheckSchemaVersion(version); err != nil {
		return fmt.Errorf("localdolt: %s: %w", s.paths.DoltDataDir(), err)
	}
	return nil
}

// schemaVersion reads meta.schema_version, reporting 0 for a store that has
// not been initialized yet. The meta table's absence is detected through
// information_schema rather than by matching the text of Dolt's "table not
// found" error.
func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var tables int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?",
		DatabaseName, metaTableName).Scan(&tables)
	if err != nil {
		return 0, fmt.Errorf("localdolt: look for the %s table: %w", metaTableName, err)
	}
	if tables == 0 {
		return 0, nil
	}

	var raw string
	err = db.QueryRowContext(ctx,
		"SELECT v FROM "+metaTableName+" WHERE k = ?", store.SchemaVersionKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("localdolt: read %s.%s: %w", metaTableName, store.SchemaVersionKey, err)
	}

	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("localdolt: %s.%s is %q, which is not a version number",
			metaTableName, store.SchemaVersionKey, raw)
	}
	if version < 0 {
		return 0, fmt.Errorf("localdolt: %s.%s is %d, which is not a version number",
			metaTableName, store.SchemaVersionKey, version)
	}
	return version, nil
}

// rowQuerier is the one method activeBranch needs, so that it can be asked
// of the pool (*sql.DB, which answers for whatever connection it hands out)
// or of one connection (*sql.Conn, the only way to ask a session that the
// staged-write lane has checked out to a proposal branch).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// activeBranch reports the branch q is on.
func activeBranch(ctx context.Context, q rowQuerier) (string, error) {
	var branch string
	if err := q.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		return "", fmt.Errorf("localdolt: read the active branch: %w", err)
	}
	return branch, nil
}
