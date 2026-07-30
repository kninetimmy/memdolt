// Package singleowner implements the lock files behind memdolt's
// single-owner rule (PRD §5.2).
//
// # Why memdolt keeps its own lock
//
// The embedded Dolt driver takes filesystem locks of its own, but they do
// not give a caller what §5.2 needs. Measured against
// github.com/dolthub/driver v1.88.1, a second OS process that opens an
// already-open Dolt data directory does not fail: it opens successfully
// and silently degrades to read-only, so the first write fails much later
// with "cannot update manifest: database is read only". Dolt's internal
// LOCK file records no pid, so "if a LOCK file exists and its owning pid
// is dead, remove it" (§5.2.3) cannot be implemented against it either.
//
// memdolt therefore takes its own lock first, before the driver ever
// touches the data directory, so that a second memdolt process fails fast
// with an error that names the owner.
//
// # How ownership is decided
//
// The authoritative signal is an advisory exclusive lock the owner holds
// on the lock file for its whole lifetime (flock on Unix, LockFileEx on
// Windows). The kernel releases it when the owning process exits, however
// it exits — including SIGKILL, TerminateProcess and a power loss — so
// "the lock is free" proves the previous owner is gone. That is stronger
// than a pid check, which cannot distinguish a live owner from an
// unrelated process that inherited a recycled pid.
//
// The recorded pid is corroborating detail: it names the owner in the
// error a second process gets, and in the loud log emitted when a stale
// record is cleared. It is deliberately not what decides staleness. See
// ProcessLiveness for what a pid check can and cannot establish.
//
// # Residual limitations
//
//   - The lock only binds processes that take it, i.e. memdolt. A foreign
//     `dolt` CLI or `dolt sql-server` against the same data directory is
//     not excluded by it; Dolt's own locking is the only backstop there.
//   - Advisory locks depend on the filesystem. On a network filesystem
//     that does not honour them, two processes could both acquire. memdolt
//     refuses to evaluate a record written by another host (see Acquire)
//     rather than guess, but it cannot detect a filesystem that accepts a
//     lock request and enforces nothing.
//   - If an operator deletes a lock file out from under a live owner, a
//     second process will create a fresh one and single ownership is lost.
//     No memdolt message ever tells an operator to do that.
package singleowner

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrLocked reports that a lock file is held by a live process. Callers
// identify the condition with errors.Is; *LockedError carries the detail.
var ErrLocked = errors.New("lock file is held by a live process")

// ErrForeignHost reports a lock file whose ownership record was written by
// a different host. memdolt cannot evaluate another machine's pid, and
// will not assume the record is stale, so it refuses to take the lock.
var ErrForeignHost = errors.New("lock file was written by another host")

// ErrUnsupportedPlatform reports that this build has no advisory file
// locking implementation. memdolt fails closed rather than running without
// the single-owner guarantee.
var ErrUnsupportedPlatform = errors.New("advisory file locking is not implemented on this platform")

// acquireAttempts and acquireRetryDelay bound how long Acquire waits for a
// lock before declaring it held. A probe (Inspect) has to take the lock for
// a moment to find out whether anyone else holds it, so an unlucky Acquire
// can collide with a concurrent probe; retrying briefly turns that into a
// short pause instead of a spurious ErrLocked. The bound is deliberately
// tiny: a genuinely held lock must still fail fast, not block.
const (
	acquireAttempts   = 3
	acquireRetryDelay = 50 * time.Millisecond
)

// State is what Inspect found at a lock file path.
type State int

const (
	// StateNone means nothing owns the lock: either the file is absent, or
	// it is present but carries no ownership record because its last owner
	// released it cleanly.
	StateNone State = iota

	// StateHeld means a live process holds the lock.
	StateHeld

	// StateStale means an ownership record survived its owner: the record
	// is present but the advisory lock is free, which proves the process
	// that wrote it is gone.
	StateStale
)

func (s State) String() string {
	switch s {
	case StateNone:
		return "none"
	case StateHeld:
		return "held"
	case StateStale:
		return "stale"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Owner is the ownership record stored inside a lock file.
//
// Detail carries caller-defined data — the IPC endpoint puts its loopback
// port and per-run token there. Nothing in this package logs or formats
// Detail, so a secret placed in it cannot leak through this package's
// errors or its stale-lock log.
type Owner struct {
	PID        int             `json:"pid"`
	Host       string          `json:"host"`
	ID         string          `json:"id"`
	Executable string          `json:"executable,omitempty"`
	AcquiredAt time.Time       `json:"acquiredAt"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// LockedError reports that Acquire found the lock held by a live process.
//
// Owner is best-effort: the record is read without the lock, so it may be
// zero if the holder had not finished writing it. Its Detail is always
// cleared — a caller-defined payload may hold a secret (the IPC endpoint
// puts its token there), and an error value travels further than the
// record it came from.
type LockedError struct {
	Path  string
	Owner Owner
}

func (e *LockedError) Error() string {
	if e.Owner.PID == 0 {
		return fmt.Sprintf("%s: %v", e.Path, ErrLocked)
	}
	return fmt.Sprintf("%s: %v (pid %d on %s since %s)",
		e.Path, ErrLocked, e.Owner.PID, e.Owner.Host, e.Owner.AcquiredAt.Format(time.RFC3339))
}

// Unwrap makes errors.Is(err, ErrLocked) report true.
func (e *LockedError) Unwrap() error { return ErrLocked }

// Options configures Acquire.
type Options struct {
	// Detail is marshalled to JSON and stored in the ownership record. It
	// may be nil.
	Detail any

	// Logger receives the loud warning emitted when a stale record is
	// cleared (PRD §5.2.3). Defaults to slog.Default().
	Logger *slog.Logger
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// held tracks the lock files this process owns.
//
// Advisory locks are an agreement between processes, and the two platform
// implementations do not agree on what a second request from the *same*
// process means. Recording our own locks makes that case deterministic:
// re-acquiring a path this process already holds is always ErrLocked,
// everywhere.
var held = struct {
	sync.Mutex
	byPath map[string]*Lock
}{byPath: make(map[string]*Lock)}

// Lock is an acquired lock file. It stays acquired until Release.
type Lock struct {
	path  string
	owner Owner

	mu       sync.Mutex
	file     *os.File
	released bool
}

// Path returns the absolute path of the lock file.
func (l *Lock) Path() string { return l.path }

// Owner returns the ownership record this process wrote.
func (l *Lock) Owner() Owner { return l.owner }

// Acquire takes exclusive ownership of the lock file at path, creating it
// with mode 0600 if it does not exist. The parent directory must already
// exist.
//
// If a live process holds the lock, Acquire returns a *LockedError (which
// matches errors.Is(err, ErrLocked)) instead of blocking. If the file
// carries a record left behind by a process that is gone, Acquire clears
// the record, logs the removal at warn level with the stale pid, and takes
// the lock — PRD §5.2.3's startup recovery.
//
// Acquire fails closed. Anything it cannot establish — a lock request the
// filesystem rejects, a record written by another host, an unreadable
// file — is an error, never an assumption that the lock is free.
func Acquire(path string, opts Options) (*Lock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve lock file path %q: %w", path, err)
	}

	detail, err := marshalDetail(opts.Detail)
	if err != nil {
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read hostname for lock file %s: %w", abs, err)
	}

	id, err := newOwnerID()
	if err != nil {
		return nil, fmt.Errorf("generate owner id for lock file %s: %w", abs, err)
	}

	held.Lock()
	if existing, ok := held.byPath[abs]; ok {
		held.Unlock()
		if existing == nil {
			// Another Acquire in this process has reserved the path and is
			// mid-flight. Report it locked; it is about to be. The
			// reservation lasts as long as the filesystem half of Acquire,
			// which is the full retry budget when a third process holds the
			// lock, so this is a window callers really do arrive in.
			return nil, &LockedError{Path: abs, Owner: Owner{PID: os.Getpid()}}
		}
		return nil, &LockedError{Path: abs, Owner: withoutDetail(existing.owner)}
	}
	// Reserve the path before touching the filesystem so two goroutines in
	// this process cannot both get past the check.
	held.byPath[abs] = nil
	held.Unlock()

	lock, err := acquireFile(abs, host, id, detail, opts)
	if err != nil {
		// Only this goroutine can hold the reservation, so dropping it is
		// unconditional.
		held.Lock()
		delete(held.byPath, abs)
		held.Unlock()
		return nil, err
	}

	held.Lock()
	held.byPath[abs] = lock
	held.Unlock()
	return lock, nil
}

// acquireFile does the filesystem half of Acquire. The caller has already
// reserved abs in the in-process registry.
func acquireFile(abs, host, id string, detail json.RawMessage, opts Options) (*Lock, error) {
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", abs, err)
	}

	locked := false
	for attempt := 0; attempt < acquireAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(acquireRetryDelay)
		}
		locked, err = tryLockFile(f)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("lock %s: %w", abs, err), f.Close())
		}
		if locked {
			break
		}
	}
	if !locked {
		prev, _ := readOwner(f) // best effort: the holder may still be writing it
		return nil, errors.Join(&LockedError{Path: abs, Owner: withoutDetail(prev)}, f.Close())
	}

	// From here on the advisory lock is ours, which proves no other process
	// owns this file. Any record still in it belongs to an owner that is
	// gone.
	if err := recoverStale(f, abs, host, opts); err != nil {
		return nil, errors.Join(err, unlockFile(f), f.Close())
	}

	owner := Owner{
		PID:        os.Getpid(),
		Host:       host,
		ID:         id,
		Executable: executablePath(),
		AcquiredAt: time.Now().UTC(),
		Detail:     detail,
	}
	if err := writeOwner(f, owner); err != nil {
		return nil, errors.Join(fmt.Errorf("write ownership record to %s: %w", abs, err), unlockFile(f), f.Close())
	}

	return &Lock{path: abs, owner: owner, file: f}, nil
}

// recoverStale clears an ownership record left behind by a dead owner,
// logging the removal loudly (PRD §5.2.3). The advisory lock must already
// be held.
func recoverStale(f *os.File, abs, host string, opts Options) error {
	raw, err := readRawOwner(f)
	if err != nil {
		return fmt.Errorf("read lock file %s: %w", abs, err)
	}
	if len(raw) == 0 {
		return nil // fresh file, or one released cleanly
	}

	var prev Owner
	if err := json.Unmarshal(raw, &prev); err != nil {
		// The record is unreadable, but the free advisory lock already
		// established that its writer is gone, so it is safe to replace.
		opts.logger().Warn("memdolt: replacing an unreadable lock record; its owner is gone",
			"path", abs, "bytes", len(raw), "error", err.Error())
		return nil
	}

	if prev.Host != "" && prev.Host != host {
		// Fail closed: pids are only meaningful on the host that issued
		// them, and on a shared filesystem the recorded owner may be alive
		// elsewhere.
		return fmt.Errorf("%s: %w (recorded host %q, pid %d; this host is %q)",
			abs, ErrForeignHost, prev.Host, prev.PID, host)
	}

	liveness, livenessErr := ProcessLiveness(prev.PID)
	attrs := []any{
		"path", abs,
		"stale_pid", prev.PID,
		"stale_owner_id", prev.ID,
		"stale_host", prev.Host,
		"stale_acquired_at", prev.AcquiredAt.Format(time.RFC3339),
		"stale_pid_liveness", liveness.String(),
	}
	if prev.Executable != "" {
		attrs = append(attrs, "stale_executable", prev.Executable)
	}
	if livenessErr != nil {
		attrs = append(attrs, "stale_pid_liveness_error", livenessErr.Error())
	}
	if liveness == LivenessLive {
		// The advisory lock is free, so the process that wrote this record
		// has exited; a live pid means the number was recycled. Say so
		// rather than letting the log imply memdolt evicted a live owner.
		attrs = append(attrs, "note", "pid is running but does not hold the lock; it was almost certainly recycled")
	}
	opts.logger().Warn("memdolt: removed a stale lock left by a dead owner", attrs...)

	if err := truncate(f); err != nil {
		return fmt.Errorf("clear stale record in %s: %w", abs, err)
	}
	return nil
}

// Release gives up the lock. It clears the ownership record first, so the
// next Acquire sees a clean file rather than reporting a stale recovery.
// Release is idempotent.
//
// It deliberately does not delete the lock file. Unlinking a file while
// holding a lock on it reintroduces exactly the race the lock prevents:
// between the unlink and the next create, two processes can end up locking
// two different files under one name and both believe they own the store.
// An empty lock file left on disk costs nothing.
func (l *Lock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	held.Lock()
	delete(held.byPath, l.path)
	held.Unlock()

	var errs []error
	if err := truncate(l.file); err != nil {
		errs = append(errs, fmt.Errorf("clear ownership record in %s: %w", l.path, err))
	}
	if err := unlockFile(l.file); err != nil {
		errs = append(errs, fmt.Errorf("unlock %s: %w", l.path, err))
	}
	if err := l.file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close %s: %w", l.path, err))
	}
	return errors.Join(errs...)
}

// Inspect reports who, if anyone, owns the lock file at path, without
// taking ownership of it.
//
// Inspect has to request the lock to find out whether it is free, so it
// holds it for a moment. A concurrent Acquire tolerates that (see
// acquireAttempts); two concurrent Inspects can briefly see each other and
// report StateHeld. Callers that must not be fooled by that treat StateHeld
// as "ask the owner", which is what ipc.Probe does.
func Inspect(path string) (Owner, State, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Owner{}, StateNone, fmt.Errorf("resolve lock file path %q: %w", path, err)
	}

	held.Lock()
	mine, ok := held.byPath[abs]
	held.Unlock()
	if ok {
		if mine == nil {
			// An Acquire in this process has reserved the path and is
			// mid-flight. Report it held; it is about to be.
			return Owner{PID: os.Getpid()}, StateHeld, nil
		}
		return mine.owner, StateHeld, nil
	}

	f, err := os.OpenFile(abs, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Owner{}, StateNone, nil
		}
		return Owner{}, StateNone, fmt.Errorf("open lock file %s: %w", abs, err)
	}
	defer func() { _ = f.Close() }()

	locked, err := tryLockFile(f)
	if err != nil {
		return Owner{}, StateNone, fmt.Errorf("probe lock on %s: %w", abs, err)
	}
	if !locked {
		owner, _ := readOwner(f) // best effort; the holder may still be writing
		return owner, StateHeld, nil
	}

	// We hold the lock, so nobody else does; read the record and let go.
	owner, readErr := readOwner(f)
	if err := unlockFile(f); err != nil {
		return Owner{}, StateNone, fmt.Errorf("release probe lock on %s: %w", abs, err)
	}
	if readErr != nil || owner.PID == 0 {
		// An absent or unreadable record with a free lock means no owner is
		// running. An unreadable one is reported as stale so that callers
		// clean it up rather than trusting it.
		if readErr != nil {
			return Owner{}, StateStale, nil
		}
		return Owner{}, StateNone, nil
	}
	return owner, StateStale, nil
}

// withoutDetail strips the caller-defined payload from an ownership
// record so that it can be embedded in an error safely.
func withoutDetail(owner Owner) Owner {
	owner.Detail = nil
	return owner
}

func marshalDetail(detail any) (json.RawMessage, error) {
	if detail == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		// Deliberately does not include the value: Detail may hold a secret.
		return nil, fmt.Errorf("encode lock file detail: %w", err)
	}
	return encoded, nil
}

func newOwnerID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// executablePath records the running binary for operator diagnostics. It is
// never used to decide ownership: memdolt has no portable way to read
// another process's image path, so it cannot be compared against a
// recorded one.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

func readRawOwner(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(raw), nil
}

func readOwner(f *os.File) (Owner, error) {
	raw, err := readRawOwner(f)
	if err != nil {
		return Owner{}, err
	}
	if len(raw) == 0 {
		return Owner{}, nil
	}
	var owner Owner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return Owner{}, fmt.Errorf("decode ownership record: %w", err)
	}
	return owner, nil
}

func writeOwner(f *os.File, owner Owner) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(encoded); err != nil {
		return err
	}
	return f.Sync()
}

func truncate(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return f.Sync()
}
