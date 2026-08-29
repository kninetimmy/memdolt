// Package localdolt implements the M0 subset of memdolt's Store interface
// over the embedded Dolt driver (PRD §5.1's LocalStore, topologies A and C).
//
// # Single ownership
//
// The store takes memdolt's single-owner lock (PRD §5.2) before the driver
// is allowed near the data directory, so a second memdolt process is
// turned away with store.ErrLocked instead of discovering the problem
// later. That is not belt and braces: measured against
// github.com/dolthub/driver v1.88.1, a second OS process that opens an
// already-open data directory succeeds and is silently downgraded to
// read-only, and its first write fails with "cannot update manifest:
// database is read only". A ping does not detect it. See
// internal/singleowner for what the lock does and does not guarantee — in
// particular, it binds memdolt processes only, not a foreign `dolt` CLI or
// `dolt sql-server` pointed at the same directory.
//
// The driver's own open-retry (Config.BackOff) is deliberately left off.
// It exists to wait out lock contention, and waiting is the opposite of
// what §5.2's single-owner rule wants from a second memdolt process;
// enabling it would also mean depending on the backoff package the driver
// exposes in that API. The M0 rig can revisit that for foreign processes.
//
// # Build requirements
//
// The embedded driver requires cgo (github.com/dolthub/gozstd, which
// dolt's block store imports unconditionally) and the gms_pure_go build
// tag, which swaps go-mysql-server's ICU-backed REGEXP implementation for
// the standard library's. Without the tag, building also needs system ICU
// development headers, which are not available on all three platforms
// memdolt supports. See the repository's CLAUDE.md.
package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	// Registers the "dolt" database/sql driver.
	_ "github.com/dolthub/driver"
	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/kninetimmy/memdolt/internal/denylist"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
)

// DatabaseName is the Dolt database memdolt keeps a project's memory in
// (PRD §5.3).
const DatabaseName = "memory"

const driverName = "dolt"

// Config configures a local store.
type Config struct {
	// BaseDir is the repository the store belongs to. The Dolt data
	// directory is <BaseDir>/.memdolt/dolt (PRD §5.3). It is supplied by
	// the caller; nothing here consults the working directory.
	BaseDir string

	// Actor is the commit identity the embedded engine is configured with.
	// Individual commits override it per request (PRD §3.1), but the
	// driver requires one up front.
	Actor store.Actor

	// Logger receives the loud stale-lock warning of PRD §5.2.3. Defaults
	// to slog.Default().
	Logger *slog.Logger
}

// Store is PRD §5.1's LocalStore: the M0 subset of store.Store backed by
// the embedded Dolt driver.
type Store struct {
	cfg    Config
	paths  layout.Paths
	logger *slog.Logger

	mu       sync.Mutex
	acceptMu sync.Mutex
	lock     *singleowner.Lock
	db       *sql.DB
	opened   bool
	closed   bool
}

var _ store.Store = (*Store)(nil)

// New validates cfg and returns a store that is not yet open. Call Open to
// take ownership of the data directory.
func New(cfg Config) (*Store, error) {
	paths, err := layout.New(cfg.BaseDir)
	if err != nil {
		return nil, err
	}
	if err := cfg.Actor.Validate(); err != nil {
		return nil, fmt.Errorf("localdolt: commit identity: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{cfg: cfg, paths: paths, logger: logger}, nil
}

// DataDir returns the Dolt data directory this store is rooted at.
func (s *Store) DataDir() string { return s.paths.DoltDataDir() }

// LockFile returns the path of the store's single-owner lock file.
func (s *Store) LockFile() string { return s.paths.LockFile() }

// Open creates the data directory if needed, takes the single-owner lock,
// opens the embedded engine and ensures the memory database exists.
//
// If another live process owns the store, Open fails immediately with an
// error matching errors.Is(err, store.ErrLocked). If the lock file was
// left behind by a process that died, Open clears it, logs the removal
// with the stale pid at warn level, and continues (PRD §5.2.3).
//
// Open does not apply migrations; a store it opens may be older than the
// schema this binary ships, and Migrate is what brings it forward. It does
// refuse a store that is *newer* than this binary, with an error matching
// errors.Is(err, store.ErrSchemaTooNew) (PRD §6.4), releasing the engine
// and the lock rather than handing back a usable store.
func (s *Store) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("localdolt: store has been closed")
	}
	if s.opened {
		return errors.New("localdolt: store is already open")
	}

	dataDir := s.paths.DoltDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("localdolt: create dolt data directory %s: %w", dataDir, err)
	}

	lock, err := singleowner.Acquire(s.paths.LockFile(), singleowner.Options{Logger: s.logger})
	if err != nil {
		return fmt.Errorf("localdolt: take store lock: %w", err)
	}

	if err := s.openEngine(ctx, dataDir); err != nil {
		return errors.Join(err, lock.Release())
	}

	// PRD §6.4's version-skew refusal. It runs here, before the store is
	// usable, so that a store written by a newer memdolt is turned away
	// with an error matching errors.Is(err, store.ErrSchemaTooNew) and no
	// caller ever gets a handle through which memory data could be read or
	// written under a schema this binary does not know.
	if err := s.guardSchemaVersion(ctx); err != nil {
		db := s.db
		s.db = nil
		return errors.Join(err, db.Close(), lock.Release())
	}

	s.lock = lock
	s.opened = true
	return nil
}

// openEngine opens the embedded engine and ensures the database exists. On
// failure it leaves s.db nil and releases whatever it opened.
func (s *Store) openEngine(ctx context.Context, dataDir string) error {
	dsn, err := buildDSN(dataDir, s.cfg.Actor, DatabaseName)
	if err != nil {
		return fmt.Errorf("localdolt: %w", err)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return fmt.Errorf("localdolt: open embedded dolt engine at %s: %w", dataDir, err)
	}

	// sql.Open is lazy; ping so that a data directory memdolt cannot open
	// fails here rather than at the first write.
	if err := db.PingContext(ctx); err != nil {
		return errors.Join(fmt.Errorf("localdolt: open embedded dolt engine at %s: %w", dataDir, err), db.Close())
	}

	// The database is a directory inside the data dir; the driver creates
	// it on demand. "memory" is a reserved word to the SQL parser, so the
	// identifier is always quoted.
	create := "CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(DatabaseName)
	if _, err := db.ExecContext(ctx, create); err != nil {
		return errors.Join(fmt.Errorf("localdolt: create database %q: %w", DatabaseName, err), db.Close())
	}

	s.db = db
	return nil
}

// Commit applies req's statements and records them as a single Dolt
// commit, attributed to req.Author (PRD §3.1). The statements and the
// commit share one connection and one transaction, so a failure part-way
// through leaves nothing behind.
//
// req.Text is matched against the repository's deny-list first; see
// checkDenyList.
//
// A request that changes nothing fails: Dolt reports "nothing to commit"
// rather than creating an empty commit.
func (s *Store) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	db, err := s.handle()
	if err != nil {
		return store.CommitResult{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return store.CommitResult{}, fmt.Errorf("localdolt: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	return s.commitConn(ctx, conn, req)
}

// commitConn is Commit's body, minus the choice of connection: it applies
// req to whatever branch conn is checked out to. The staged-write lane
// (propose.go) needs that separation, because its connection is checked out
// to a proposal branch rather than taken fresh from the pool.
//
// It is where the request is validated and the deny-list applied, rather
// than Commit, because it is the point every write that builds a
// store.CommitRequest passes through. A check in Commit alone would leave
// the staged-write lane unscanned — and a proposal is the lane an untrusted
// agent writes through (PRD §7).
//
// Before the review lane landed that was every write in this package, and
// this comment said so. It is now one of two: review.go's accept promotes a
// proposal by merging its branch, which stages the rows itself, so it
// concludes with a DOLT_COMMIT of its own and never reaches here. Those are
// the only two DOLT_COMMIT sites in this package outside tests. The accept
// path therefore carries its own call to checkDenyList, over prose it reads
// out of the proposal branch's diff; the columns it reads are review.go's
// scannedColumns, which has to stay in step with Fact.text, Decision.text,
// and the proposal rationale and actor in propose.go, because those are
// what the same rows were scanned as when they were staged.
func (s *Store) commitConn(ctx context.Context, conn *sql.Conn, req store.CommitRequest) (store.CommitResult, error) {
	if err := req.Validate(); err != nil {
		return store.CommitResult{}, fmt.Errorf("localdolt: invalid commit request: %w", err)
	}
	if err := s.checkDenyList(req.Text); err != nil {
		return store.CommitResult{}, err
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return store.CommitResult{}, fmt.Errorf("localdolt: begin transaction: %w", err)
	}

	result, err := commitTx(ctx, tx, req)
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return store.CommitResult{}, errors.Join(err, fmt.Errorf("localdolt: rollback: %w", rollbackErr))
		}
		return store.CommitResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return store.CommitResult{}, fmt.Errorf("localdolt: commit transaction: %w", err)
	}
	return result, nil
}

// checkDenyList applies the repository's `[deny_list]` (PRD §11.3) to the
// memory text a write declares.
//
// It runs before the transaction opens, so a refused write leaves no row
// and no commit behind — nothing was ever started. A refused proposal has
// its branch deleted by stage, the way any other staging failure does, so
// it leaves nothing behind either.
//
// It fails closed in both directions the PRD §14 conventions ask for. A
// deny-list that is configured but cannot be evaluated — an unreadable
// config file, TOML that does not parse, a `[deny_list]` table whose
// patterns key is misspelled, a pattern that is not a valid regular
// expression — refuses the write rather than allowing it. A repository
// with no deny-list configured refuses nothing, so the unconfigured case
// costs a write only the stat that finds no config file.
//
// A request that declared NoText is not scanned at all, and does not read
// the config file: a write carrying no prose has nothing a rule could
// match. That is what keeps a broken deny-list from blocking the migration
// runner, whose commits are the only such writes today. Validate has
// already refused anything that neither declared text nor declared none,
// so an empty text here is a decision somebody wrote down.
//
// The config file is read per write rather than cached at Open, so an
// edited deny-list binds the next write instead of the next process.
// ponytail: one small read and one compile per write; cache it on the
// store if writes ever run hot enough for that to show.
func (s *Store) checkDenyList(text []string) error {
	if len(text) == 0 {
		return nil
	}
	list, err := denylist.Load(s.paths.ConfigFile())
	if err != nil {
		return fmt.Errorf("localdolt: refusing the write: %w", err)
	}
	if err := list.Check(text...); err != nil {
		return fmt.Errorf("localdolt: %w", err)
	}
	return nil
}

// commitTx runs the statements and the Dolt commit inside tx.
func commitTx(ctx context.Context, tx *sql.Tx, req store.CommitRequest) (store.CommitResult, error) {
	var affected int64
	for i, stmt := range req.Statements {
		res, err := tx.ExecContext(ctx, stmt.SQL, stmt.Args...)
		if err != nil {
			return store.CommitResult{}, fmt.Errorf("localdolt: statement %d: %w", i, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return store.CommitResult{}, fmt.Errorf("localdolt: statement %d rows affected: %w", i, err)
		}
		affected += n
	}

	// DOLT_COMMIT stages every table (-A) and writes the commit. The
	// message and author are bound parameters, never interpolated.
	var hash string
	row := tx.QueryRowContext(ctx,
		"CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
		req.Message, req.Author.String())
	if err := row.Scan(&hash); err != nil {
		return store.CommitResult{}, fmt.Errorf("localdolt: dolt commit: %w", err)
	}

	return store.CommitResult{Hash: hash, RowsAffected: affected}, nil
}

// Query enforces store.Store's one-SELECT-or-SHOW boundary against the memory
// database. Before this check, Query passed caller SQL to the driver unchanged,
// so a write could remain uncommitted in Dolt's working set; now every other
// statement is rejected before the database executes it. The caller closes the
// returned rows.
func (s *Store) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	db, err := s.handle()
	if err != nil {
		return nil, err
	}
	statement, err := sqlparser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("localdolt: query must be exactly one read-only SELECT or SHOW statement: %w", err)
	}
	switch statement := statement.(type) {
	case sqlparser.SelectStatement:
		if statement.GetInto() == nil {
			break
		}
		return nil, errors.New("localdolt: query must be exactly one read-only SELECT or SHOW statement")
	case *sqlparser.Show:
	default:
		return nil, errors.New("localdolt: query must be exactly one read-only SELECT or SHOW statement")
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("localdolt: query: %w", err)
	}
	return rows, nil
}

// Close shuts the embedded engine down and releases the single-owner lock.
// It is idempotent. A closed store cannot be reopened; construct a new one
// with New.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.opened = false

	var errs []error
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("localdolt: close embedded dolt engine: %w", err))
		}
		s.db = nil
	}
	if s.lock != nil {
		if err := s.lock.Release(); err != nil {
			errs = append(errs, fmt.Errorf("localdolt: release store lock: %w", err))
		}
		s.lock = nil
	}
	return errors.Join(errs...)
}

func (s *Store) handle() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opened || s.db == nil {
		return nil, fmt.Errorf("localdolt: %w", store.ErrNotOpen)
	}
	return s.db, nil
}
