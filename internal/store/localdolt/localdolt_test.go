package localdolt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// deadPID is above Linux's maximum pid_max and odd, which no Windows pid
// is, so no platform can have a process using it.
const deadPID = 2147483647

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// baseDir returns a scratch repository root. It does not use t.TempDir
// because that fails the test if the directory cannot be removed, and the
// embedded engine memory-maps files inside the Dolt data directory that
// Windows may still hold briefly after the store is closed.
func baseDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-store")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func openStore(t *testing.T, base string, logger *slog.Logger) *localdolt.Store {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"},
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCommitProducesADoltCommitVisibleInDoltLog covers the acceptance
// criterion that the store is rooted at .memdolt/dolt beneath a
// caller-supplied base directory and that a committed write is observable
// in dolt_log.
func TestCommitProducesADoltCommitVisibleInDoltLog(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())

	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if st.DataDir() != paths.DoltDataDir() {
		t.Fatalf("data dir = %q, want %q", st.DataDir(), paths.DoltDataDir())
	}
	if _, err := os.Stat(filepath.Join(st.DataDir(), localdolt.DatabaseName, ".dolt")); err != nil {
		t.Fatalf("database %q was not created beneath %s: %v", localdolt.DatabaseName, st.DataDir(), err)
	}

	author := store.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"}
	const message = "propose fact msrv=1.24"
	result, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY, v TEXT)"},
			{SQL: "INSERT INTO probe (k, v) VALUES (?, ?)", Args: []any{"msrv", "1.24"}},
		},
		NoText:  true,
		Message: message,
		Author:  author,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if result.Hash == "" {
		t.Fatal("commit returned an empty hash")
	}
	if result.RowsAffected != 1 {
		t.Fatalf("rows affected = %d, want 1", result.RowsAffected)
	}

	rows, err := st.Query(ctx,
		"SELECT commit_hash, committer, email, message FROM dolt_log ORDER BY date DESC LIMIT 1")
	if err != nil {
		t.Fatalf("query dolt_log: %v", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		t.Fatalf("dolt_log has no rows (err %v)", rows.Err())
	}
	var hash, committer, email, loggedMessage string
	if err := rows.Scan(&hash, &committer, &email, &loggedMessage); err != nil {
		t.Fatalf("scan dolt_log: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dolt_log rows: %v", err)
	}

	if hash != result.Hash {
		t.Fatalf("dolt_log head = %q, want the committed hash %q", hash, result.Hash)
	}
	if loggedMessage != message {
		t.Fatalf("dolt_log message = %q, want %q", loggedMessage, message)
	}
	// PRD §3.1: the commit is attributed to the normalized actor, so
	// dolt_log answers provenance questions with no application code.
	if committer != author.Name || email != author.Email {
		t.Fatalf("dolt_log author = %q <%q>, want %q <%q>", committer, email, author.Name, author.Email)
	}

	var value string
	valueRows, err := st.Query(ctx, "SELECT v FROM probe WHERE k = ?", "msrv")
	if err != nil {
		t.Fatalf("query probe: %v", err)
	}
	defer func() { _ = valueRows.Close() }()
	if !valueRows.Next() {
		t.Fatalf("probe row missing (err %v)", valueRows.Err())
	}
	if err := valueRows.Scan(&value); err != nil {
		t.Fatalf("scan probe: %v", err)
	}
	if value != "1.24" {
		t.Fatalf("probe value = %q, want %q", value, "1.24")
	}
}

func TestCommitRejectsAWriteWithNothingToRecord(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, baseDir(t), discardLogger())

	author := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{SQL: "SELECT 1"}},
		NoText:     true,
		Message:    "nothing to see",
		Author:     author,
	}); err == nil {
		t.Fatal("a commit that changes nothing should fail loudly, not create an empty commit")
	}
}

func TestOperationsOnAClosedStoreFail(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := openStore(t, base, discardLogger())
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second close should be a no-op: %v", err)
	}

	if _, err := st.Query(ctx, "SELECT 1"); !errors.Is(err, store.ErrNotOpen) {
		t.Fatalf("query after close = %v, want one matching ErrNotOpen", err)
	}
	if _, err := st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{SQL: "SELECT 1"}},
		NoText:     true,
		Message:    "m",
		Author:     store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); !errors.Is(err, store.ErrNotOpen) {
		t.Fatalf("commit after close = %v, want one matching ErrNotOpen", err)
	}

	// Closing releases the lock, so the store can be opened again.
	openStore(t, base, discardLogger())
}

// TestOpenRejectsAStoreOwnedByALiveProcess covers the acceptance criterion
// that opening a store whose LOCK file is held by a live process fails
// with a distinct, programmatically identifiable error.
func TestOpenRejectsAStoreOwnedByALiveProcess(t *testing.T) {
	base := baseDir(t)
	openStore(t, base, discardLogger())

	second, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   store.Actor{Name: "user", Email: "user@memdolt.invalid"},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("new second store: %v", err)
	}

	start := time.Now()
	err = second.Open(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second store opened a data directory that is already owned")
	}
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("open error = %v, want one matching store.ErrLocked", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("open took %v; it must fail fast rather than block", elapsed)
	}
}

// TestOpenRecoversALockLeftByADeadProcess covers the acceptance criterion
// that opening a store whose LOCK file names a dead process removes the
// stale lock, logs the removal loudly with the stale pid, and then opens.
func TestOpenRecoversALockLeftByADeadProcess(t *testing.T) {
	base := baseDir(t)
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if err := os.MkdirAll(paths.Dir(), 0o755); err != nil {
		t.Fatalf("create %s: %v", paths.Dir(), err)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	// Exactly what an owner that was killed rather than shut down leaves
	// behind: its record, with no process holding the lock.
	record, err := json.Marshal(singleowner.Owner{
		PID:        deadPID,
		Host:       host,
		ID:         "5ea1ed5ea1ed5ea1",
		AcquiredAt: time.Now().Add(-time.Hour).UTC(),
	})
	if err != nil {
		t.Fatalf("marshal stale record: %v", err)
	}
	if err := os.WriteFile(paths.LockFile(), record, 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st := openStore(t, base, logger)

	logged := logs.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("stale lock removal was not logged loudly: %q", logged)
	}
	if !strings.Contains(logged, "stale_pid="+strconv.Itoa(deadPID)) {
		t.Fatalf("stale lock removal log %q does not name the stale pid %d", logged, deadPID)
	}

	remaining, err := os.ReadFile(paths.LockFile())
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if bytes.Contains(remaining, []byte(strconv.Itoa(deadPID))) {
		t.Fatalf("the stale record survived: %s", remaining)
	}

	// And the store really is usable afterwards.
	if _, err := st.Commit(context.Background(), store.CommitRequest{
		Statements: []store.Statement{{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY)"}},
		NoText:     true,
		Message:    "schema probe",
		Author:     store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("commit after stale recovery: %v", err)
	}
}

func TestNewRejectsAnInvalidConfiguration(t *testing.T) {
	if _, err := localdolt.New(localdolt.Config{
		Actor: store.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err == nil {
		t.Fatal("a store with no base directory was accepted")
	}
	if _, err := localdolt.New(localdolt.Config{BaseDir: t.TempDir()}); err == nil {
		t.Fatal("a store with no commit identity was accepted")
	}
}
