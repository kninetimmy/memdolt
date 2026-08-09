//go:build soak

package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// roleEnv puts the test binary into helper mode. The rig has to run real
// operating-system processes: PRD §5.2's whole subject is what happens
// between processes, and goroutines pretending to be processes would share
// the one thing — the Dolt data directory's in-process state — whose
// sharing is the question.
const roleEnv = "MEMDOLT_SOAK_ROLE"

const (
	roleOwner  = "owner"
	roleClient = "client"
)

// tableName is the rig's own probe table. It is not memdolt's schema; M1
// brings that (PRD §6.1).
const tableName = "soak_writes"

const createTableSQL = "CREATE TABLE IF NOT EXISTS " + tableName + ` (
	id VARCHAR(64) PRIMARY KEY,
	writer VARCHAR(64) NOT NULL,
	seq INT NOT NULL,
	payload VARCHAR(255) NOT NULL,
	digest CHAR(64) NOT NULL
)`

const (
	insertSQL    = "INSERT INTO " + tableName + " (id, writer, seq, payload, digest) VALUES (?, ?, ?, ?, ?)"
	selectSQL    = "SELECT payload, digest FROM " + tableName + " WHERE id = ?"
	countSQL     = "SELECT COUNT(*) FROM " + tableName
	selectAllSQL = "SELECT id, payload, digest FROM " + tableName
)

// roleConfig is what a helper process is told to do. It travels as JSON in
// roleEnv rather than as flags, so that the helper never has to parse the
// testing package's flags.
type roleConfig struct {
	Role          string        `json:"role"`
	ID            string        `json:"id"`
	BaseDir       string        `json:"baseDir"`
	LedgerDir     string        `json:"ledgerDir"`
	StatsDir      string        `json:"statsDir"`
	Writers       int           `json:"writers"`
	Readers       int           `json:"readers"`
	Duration      time.Duration `json:"duration"`
	WriteInterval time.Duration `json:"writeInterval"`
	ReadInterval  time.Duration `json:"readInterval"`
	OpTimeout     time.Duration `json:"opTimeout"`
	Nonce         string        `json:"nonce"`
	ActorName     string        `json:"actorName"`
	ActorEmail    string        `json:"actorEmail"`
}

func (c roleConfig) actor() store.Actor {
	return store.Actor{Name: c.ActorName, Email: c.ActorEmail}
}

func (c roleConfig) statsPath() string {
	return filepath.Join(c.StatsDir, c.ID+".json")
}

// runRole runs a helper process and returns its exit code.
func runRole(raw string) int {
	var cfg roleConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "soak: decode role configuration:", err)
		return 2
	}

	var err error
	switch cfg.Role {
	case roleOwner:
		err = runOwner(cfg)
	case roleClient:
		err = runClient(cfg)
	default:
		err = fmt.Errorf("unknown role %q", cfg.Role)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "soak: %s %s: %v\n", cfg.Role, cfg.ID, err)
		return 3
	}
	return 0
}

// fatal ends a helper process loudly. It is reached only when the rig
// cannot go on measuring — a ledger it cannot append to, for instance —
// because a rig that carries on after losing its own records would report
// numbers nobody should believe.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "soak: fatal:", err)
	os.Exit(5)
}

// runOwner is the process PRD §5.2.1 calls the single owner: it holds the
// store lock and the pidfile for its whole life, does its own work against
// the store in process, and serves everyone else over the loopback
// endpoint.
func runOwner(cfg roleConfig) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := localdolt.New(localdolt.Config{BaseDir: cfg.BaseDir, Actor: cfg.actor(), Logger: logger})
	if err != nil {
		return fmt.Errorf("new store: %w", err)
	}
	if err := st.Open(context.Background()); err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	if err := ensureSchema(context.Background(), st, cfg.actor()); err != nil {
		return err
	}

	routes, err := storeipc.NewHandler(storeipc.Config{Store: st, Logger: logger})
	if err != nil {
		return fmt.Errorf("new store routes: %w", err)
	}
	srv, err := ipc.Listen(ipc.Config{BaseDir: cfg.BaseDir, Handler: routes, Logger: logger})
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = srv.Close() }()

	stats := newRoleStats(roleOwner, cfg.ID, cfg.statsPath())
	if err := stats.write(); err != nil {
		return err
	}

	// The conductor waits for this line before it starts any client, so
	// that a client never has to poll for an owner that is not up yet.
	fmt.Println("READY", os.Getpid(), srv.Port())

	runCtx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()
	// A closed stdin is the conductor asking for a clean shutdown. An
	// unclean one arrives as a signal this process never sees.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	go stats.snapshotLoop(runCtx)

	runWorkload(runCtx, cfg, &directExecutor{store: st, actor: cfg.actor()}, stats)

	stats.markCleanEnd()
	if err := stats.write(); err != nil {
		return err
	}
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close endpoint: %w", err)
	}
	return st.Close()
}

// runClient is a process in the position PRD §5.2.1 puts a CLI invocation
// in: it does not open the store, it finds the owner and routes through it.
func runClient(cfg roleConfig) error {
	client, err := storeipc.Dial(cfg.BaseDir)
	if err != nil {
		return fmt.Errorf("dial the owner: %w", err)
	}

	stats := newRoleStats(roleClient, cfg.ID, cfg.statsPath())
	if err := stats.write(); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()
	go stats.snapshotLoop(runCtx)

	runWorkload(runCtx, cfg, &ipcExecutor{client: client, actor: cfg.actor()}, stats)

	stats.markCleanEnd()
	return stats.write()
}

func ensureSchema(ctx context.Context, st *localdolt.Store, actor store.Actor) error {
	// DDL is not concurrency-atomic (PRD §5.2), so the table is created
	// once, before anything concurrent starts.
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{SQL: createTableSQL}},
		NoText:     true,
		Message:    "soak: create the probe table",
		Author:     actor,
	}); err != nil {
		return fmt.Errorf("create probe table: %w", err)
	}
	return nil
}

// executor is the two things a soak participant does to the store, behind
// the two routes to it: in process, and over the owner's endpoint.
type executor interface {
	commit(ctx context.Context, rec Record) (hash string, outcome Outcome, err error)
	readRow(ctx context.Context, key string) (payload, digest string, found bool, err error)
	countRows(ctx context.Context) (int64, error)
}

type directExecutor struct {
	store *localdolt.Store
	actor store.Actor
}

func (e *directExecutor) commit(ctx context.Context, rec Record) (string, Outcome, error) {
	result, err := e.store.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  insertSQL,
			Args: []any{rec.Key, rec.Writer, rec.Seq, rec.Payload, rec.Digest},
		}},
		// A probe row is generated bytes in a table of its own, not
		// memory anyone wrote, so there is nothing for the deny-list to
		// scan (PRD §11.3).
		NoText:  true,
		Message: "soak write " + rec.Key,
		Author:  e.actor,
	})
	if err != nil {
		// In process there is no answer to lose, so a failure is a failure
		// — except a cancelled context, which can cut the call off with the
		// transaction's fate genuinely unknown.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", OutcomeIndeterminate, err
		}
		return "", OutcomeRefused, err
	}
	return result.Hash, OutcomeCommitted, nil
}

func (e *directExecutor) readRow(ctx context.Context, key string) (string, string, bool, error) {
	rows, err := e.store.Query(ctx, selectSQL, key)
	if err != nil {
		return "", "", false, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return "", "", false, rows.Err()
	}
	var payload, digest string
	if err := rows.Scan(&payload, &digest); err != nil {
		return "", "", false, err
	}
	return payload, digest, true, rows.Err()
}

func (e *directExecutor) countRows(ctx context.Context) (int64, error) {
	rows, err := e.store.Query(ctx, countSQL)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return 0, fmt.Errorf("count returned no row: %w", rows.Err())
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	return count, rows.Err()
}

type ipcExecutor struct {
	client *storeipc.Client
	actor  store.Actor
}

func (e *ipcExecutor) commit(ctx context.Context, rec Record) (string, Outcome, error) {
	resp, err := e.client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{{
			SQL:  insertSQL,
			Args: []any{rec.Key, rec.Writer, rec.Seq, rec.Payload, rec.Digest},
		}},
		NoText:  true,
		Message: "soak write " + rec.Key,
		Author:  storeipc.Actor{Name: e.actor.Name, Email: e.actor.Email},
	})
	if err != nil {
		if storeipc.IsOwnerRefusal(err) {
			return "", OutcomeRefused, err
		}
		// No status came back, so the request may have been executed and
		// only the answer lost. The rig refuses to guess either way.
		return "", OutcomeIndeterminate, err
	}
	return resp.Hash, OutcomeCommitted, nil
}

func (e *ipcExecutor) readRow(ctx context.Context, key string) (string, string, bool, error) {
	grid, err := e.client.Query(ctx, storeipc.QueryRequest{SQL: selectSQL, Args: []any{key}})
	if err != nil {
		return "", "", false, err
	}
	if len(grid.Rows) == 0 {
		return "", "", false, nil
	}
	row := grid.Rows[0]
	if len(row) != 2 || row[0] == nil || row[1] == nil {
		return "", "", false, fmt.Errorf("row for %s came back as %v, which is not a payload and a digest", key, row)
	}
	return *row[0], *row[1], true, nil
}

func (e *ipcExecutor) countRows(ctx context.Context) (int64, error) {
	grid, err := e.client.Query(ctx, storeipc.QueryRequest{SQL: countSQL})
	if err != nil {
		return 0, err
	}
	if len(grid.Rows) != 1 || len(grid.Rows[0]) != 1 || grid.Rows[0][0] == nil {
		return 0, fmt.Errorf("count came back as %v", grid.Rows)
	}
	return strconv.ParseInt(*grid.Rows[0][0], 10, 64)
}

// committedSet holds the keys this process has been told are committed.
// A reader only asks for keys in it, so the acknowledgement of a write
// always happens before the read that looks for it: a miss is then a real
// miss, not a race the rig invented.
type committedSet struct {
	mu       sync.Mutex
	keys     []string
	payloads map[string]string
}

func newCommittedSet() *committedSet {
	return &committedSet{payloads: make(map[string]string)}
}

func (s *committedSet) add(key, payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
	s.payloads[key] = payload
}

func (s *committedSet) random() (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return "", "", false
	}
	key := s.keys[rand.IntN(len(s.keys))]
	return key, s.payloads[key], true
}

// runWorkload drives this process's writers and readers until ctx ends.
func runWorkload(ctx context.Context, cfg roleConfig, ex executor, stats *RoleStats) {
	set := newCommittedSet()

	var wg sync.WaitGroup
	for i := 0; i < cfg.Writers; i++ {
		writerID := fmt.Sprintf("%s-w%d", cfg.ID, i)
		ledger, err := OpenLedger(cfg.LedgerDir, writerID)
		if err != nil {
			fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = ledger.Close() }()
			writerLoop(ctx, cfg, ex, ledger, stats, set, writerID)
		}()
	}
	for i := 0; i < cfg.Readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readerLoop(ctx, cfg, ex, stats, set)
		}()
	}
	wg.Wait()
}

// writerLoop records what it is about to do, does it, then records what it
// was told. The operation itself runs on a context the run deadline does
// not cancel, so that stopping the soak cannot manufacture indeterminate
// writes out of writes that were about to succeed.
func writerLoop(ctx context.Context, cfg roleConfig, ex executor, ledger *Ledger, stats *RoleStats, set *committedSet, writerID string) {
	for seq := 0; ctx.Err() == nil; seq++ {
		key := fmt.Sprintf("%s/%06d", writerID, seq)
		payload := fmt.Sprintf("%s|%s|%d", cfg.Nonce, writerID, seq)
		digest := digestOf(payload)

		intent := Record{Phase: PhaseIntent, Seq: seq, Key: key, Payload: payload, Digest: digest}
		if err := ledger.Append(intent); err != nil {
			fatal(err)
		}

		opCtx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
		hash, outcome, err := ex.commit(opCtx, Record{Writer: writerID, Seq: seq, Key: key, Payload: payload, Digest: digest})
		cancel()

		result := Record{Phase: PhaseResult, Seq: seq, Key: key, Payload: payload, Digest: digest, Outcome: outcome, Hash: hash}
		if err != nil {
			result.ErrClass = classify(err)
			result.ErrText = truncate(err.Error(), errorSampleBytes)
		}
		if appendErr := ledger.Append(result); appendErr != nil {
			fatal(appendErr)
		}

		stats.recordWrite(outcome, err)
		if outcome == OutcomeCommitted {
			set.add(key, payload)
		}

		if cfg.WriteInterval > 0 {
			sleepUnlessDone(ctx, cfg.WriteInterval)
		}
	}
}

// countEvery is how many row reads a reader does between the row-count
// reads that would expose the store going backwards.
const countEvery = 20

func readerLoop(ctx context.Context, cfg roleConfig, ex executor, stats *RoleStats, set *committedSet) {
	var highWater int64 = -1
	for i := 0; ctx.Err() == nil; i++ {
		key, payload, ok := set.random()
		if !ok {
			sleepUnlessDone(ctx, 10*time.Millisecond)
			continue
		}

		opCtx, cancel := context.WithTimeout(context.Background(), cfg.OpTimeout)
		gotPayload, gotDigest, found, err := ex.readRow(opCtx, key)
		cancel()

		stats.recordRead(err)
		switch {
		case err != nil:
		case !found:
			// The store acknowledged this write before the read started.
			stats.recordMissingRow(key)
		case gotPayload != payload || gotDigest != digestOf(payload):
			stats.recordMismatch(key)
		}

		if i%countEvery == 0 {
			countCtx, cancelCount := context.WithTimeout(context.Background(), cfg.OpTimeout)
			count, countErr := ex.countRows(countCtx)
			cancelCount()
			stats.recordRead(countErr)
			if countErr == nil {
				if highWater >= 0 && count < highWater {
					stats.recordCountRegression(highWater, count)
				}
				if count > highWater {
					highWater = count
				}
			}
		}

		if cfg.ReadInterval > 0 {
			sleepUnlessDone(ctx, cfg.ReadInterval)
		}
	}
}

func sleepUnlessDone(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
