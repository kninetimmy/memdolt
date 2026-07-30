package singleowner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperLockPathEnv puts the test binary into helper mode: instead of
// running tests it acquires the named lock file and holds it until it is
// killed or its stdin closes. Holding a lock from a real second process is
// the only faithful way to test the live-owner and stale-lock paths, since
// both turn on what the kernel does with a lock when a process dies.
const helperLockPathEnv = "MEMDOLT_TEST_LOCK_HELPER_PATH"

func TestMain(m *testing.M) {
	if path := os.Getenv(helperLockPathEnv); path != "" {
		runLockHelper(path)
		return
	}
	os.Exit(m.Run())
}

func runLockHelper(path string) {
	lock, err := Acquire(path, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, "lock helper:", err)
		os.Exit(2)
	}
	fmt.Println("READY", os.Getpid())
	// Block until the parent closes stdin (clean exit) or kills us.
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lock.Release(); err != nil {
		fmt.Fprintln(os.Stderr, "lock helper release:", err)
		os.Exit(3)
	}
}

type helper struct {
	cmd   *exec.Cmd
	pid   int
	stdin io.WriteCloser
	waitc chan struct{}
}

// startLockHelper launches a second process that holds the lock at path.
func startLockHelper(t *testing.T, path string) *helper {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperLockPathEnv+"="+path)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}

	h := &helper{cmd: cmd, stdin: stdin, waitc: make(chan struct{})}
	t.Cleanup(func() { h.stop() })

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read helper readiness (got %q): %v", line, err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "READY" {
		t.Fatalf("unexpected helper greeting %q", line)
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse helper pid from %q: %v", line, err)
	}
	h.pid = pid
	return h
}

// kill terminates the helper without giving it a chance to clean up, which
// is the unclean shutdown that leaves a stale lock behind.
func (h *helper) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	// Wait reaps the process; by the time it returns the kernel has
	// released every lock the child held.
	_ = h.cmd.Wait()
	close(h.waitc)
}

func (h *helper) stop() {
	select {
	case <-h.waitc:
		return
	default:
	}
	_ = h.stdin.Close()
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
	close(h.waitc)
}

// syncBuffer collects log output from any goroutine.
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

func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "LOCK")
}

func TestAcquireRecordsOwnerAndReleaseClearsIt(t *testing.T) {
	path := lockPath(t)

	lock, err := Acquire(path, Options{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lock.Owner().PID != os.Getpid() {
		t.Fatalf("owner pid = %d, want %d", lock.Owner().PID, os.Getpid())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("lock file mode = %v, want 0600", perm)
		}
	}

	owner, state, err := Inspect(path)
	if err != nil {
		t.Fatalf("inspect while held: %v", err)
	}
	if state != StateHeld {
		t.Fatalf("state while held = %v, want %v", state, StateHeld)
	}
	if owner.PID != os.Getpid() {
		t.Fatalf("inspected pid = %d, want %d", owner.PID, os.Getpid())
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second release should be a no-op: %v", err)
	}

	_, state, err = Inspect(path)
	if err != nil {
		t.Fatalf("inspect after release: %v", err)
	}
	if state != StateNone {
		t.Fatalf("state after clean release = %v, want %v", state, StateNone)
	}
}

func TestAcquireRejectsSecondAcquireInThisProcess(t *testing.T) {
	path := lockPath(t)

	lock, err := Acquire(path, Options{})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = lock.Release() }()

	_, err = Acquire(path, Options{})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire error = %v, want one matching ErrLocked", err)
	}
}

// TestAcquireRejectsLockHeldByLiveProcess covers the acceptance criterion
// that opening a store whose LOCK file is held by a live process fails
// with an identifiable error instead of blocking, panicking or proceeding.
func TestAcquireRejectsLockHeldByLiveProcess(t *testing.T) {
	path := lockPath(t)
	h := startLockHelper(t, path)

	start := time.Now()
	lock, err := Acquire(path, Options{})
	elapsed := time.Since(start)
	if err == nil {
		_ = lock.Release()
		t.Fatal("acquire succeeded while another process held the lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("acquire error = %v, want one matching ErrLocked", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("acquire took %v; it must fail fast rather than block", elapsed)
	}

	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("acquire error %v is not a *LockedError", err)
	}
	if locked.Owner.PID != h.pid {
		t.Fatalf("reported owner pid = %d, want the helper's %d", locked.Owner.PID, h.pid)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(h.pid)) {
		t.Fatalf("error %q does not name the holding pid %d", err.Error(), h.pid)
	}

	owner, state, err := Inspect(path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if state != StateHeld || owner.PID != h.pid {
		t.Fatalf("inspect = (%v, pid %d), want (%v, pid %d)", state, owner.PID, StateHeld, h.pid)
	}
}

// TestAcquireRecoversLockLeftByDeadProcess covers PRD §5.2.3: a LOCK file
// naming a dead process is removed, the removal is logged loudly with the
// stale pid, and the acquisition then succeeds.
func TestAcquireRecoversLockLeftByDeadProcess(t *testing.T) {
	path := lockPath(t)
	h := startLockHelper(t, path)
	h.kill(t)

	if liveness, err := ProcessLiveness(h.pid); err == nil && liveness == LivenessLive {
		t.Skipf("helper pid %d was recycled before the check; nothing to assert", h.pid)
	}

	owner, state, err := Inspect(path)
	if err != nil {
		t.Fatalf("inspect after kill: %v", err)
	}
	if state != StateStale {
		t.Fatalf("state after unclean kill = %v, want %v", state, StateStale)
	}
	if owner.PID != h.pid {
		t.Fatalf("stale record pid = %d, want %d", owner.PID, h.pid)
	}

	logger, logs := captureLogger()
	lock, err := Acquire(path, Options{Logger: logger})
	if err != nil {
		t.Fatalf("acquire after unclean kill: %v", err)
	}
	defer func() { _ = lock.Release() }()

	logged := logs.String()
	if !strings.Contains(logged, "stale") {
		t.Fatalf("stale recovery was not logged loudly; log was %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("stale recovery must be logged at warn level or above; log was %q", logged)
	}
	if !strings.Contains(logged, "stale_pid="+strconv.Itoa(h.pid)) {
		t.Fatalf("stale recovery log %q does not include the stale pid %d", logged, h.pid)
	}

	if lock.Owner().PID != os.Getpid() {
		t.Fatalf("after recovery the owner pid = %d, want %d", lock.Owner().PID, os.Getpid())
	}
	// The stale record must be gone, not merely ignored.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if bytes.Contains(raw, []byte(`"pid":`+strconv.Itoa(h.pid))) {
		t.Fatalf("lock file still carries the stale record: %s", raw)
	}
}

func TestAcquireRefusesRecordFromAnotherHost(t *testing.T) {
	path := lockPath(t)

	encoded, err := json.Marshal(Owner{
		PID:        os.Getpid(),
		Host:       "some-other-machine",
		ID:         "0123456789abcdef",
		AcquiredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal owner: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write foreign record: %v", err)
	}

	lock, err := Acquire(path, Options{})
	if err == nil {
		_ = lock.Release()
		t.Fatal("acquire took a lock recorded by another host")
	}
	if !errors.Is(err, ErrForeignHost) {
		t.Fatalf("acquire error = %v, want one matching ErrForeignHost", err)
	}
}

func TestInspectReportsNoOwnerForMissingFile(t *testing.T) {
	_, state, err := Inspect(filepath.Join(t.TempDir(), "absent", "LOCK"))
	if err != nil {
		t.Fatalf("inspect missing file: %v", err)
	}
	if state != StateNone {
		t.Fatalf("state = %v, want %v", state, StateNone)
	}
}

func TestProcessLiveness(t *testing.T) {
	// definitelyUnusedPID is above Linux's maximum pid_max (2^22) and is
	// odd, which no Windows pid is, so no platform can have a process with
	// it.
	const definitelyUnusedPID = 2147483647

	if liveness, err := ProcessLiveness(os.Getpid()); err != nil || liveness != LivenessLive {
		t.Fatalf("liveness of this process = (%v, %v), want (%v, nil)", liveness, err, LivenessLive)
	}

	if liveness, err := ProcessLiveness(definitelyUnusedPID); err != nil || liveness != LivenessDead {
		t.Fatalf("liveness of pid %d = (%v, %v), want (%v, nil)", definitelyUnusedPID, liveness, err, LivenessDead)
	}

	if _, err := ProcessLiveness(0); err == nil {
		t.Fatal("liveness of pid 0 should be an error, not a verdict")
	}

	h := startLockHelper(t, lockPath(t))
	if liveness, err := ProcessLiveness(h.pid); err != nil || liveness != LivenessLive {
		t.Fatalf("liveness of the running helper = (%v, %v), want (%v, nil)", liveness, err, LivenessLive)
	}
	h.kill(t)
	liveness, err := ProcessLiveness(h.pid)
	if err != nil {
		t.Fatalf("liveness of the killed helper: %v", err)
	}
	if liveness == LivenessLive {
		// A pid can be recycled between the kill and the check; that is a
		// property of pids, not a defect in the check, and is exactly why
		// staleness is decided by the lock instead.
		t.Skipf("pid %d was recycled after the helper exited", h.pid)
	}
	if liveness != LivenessDead {
		t.Fatalf("liveness of the killed helper = %v, want %v", liveness, LivenessDead)
	}
}
