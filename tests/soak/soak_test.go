//go:build soak

// Package soak is PRD §16's rig 1: the embedded-driver concurrency soak
// whose exit gate — "zero data-loss events" — decides whether memdolt is
// built at all.
//
// It is behind the `soak` build tag because it starts operating-system
// processes, kills one of them, and runs for as long as it is told to. The
// default `go test ./...` must stay a fast, hermetic suite on three
// platforms, so it never sees this package.
//
//	go test -tags soak,gms_pure_go ./tests/soak/...
//	go test -tags soak,gms_pure_go ./tests/soak/... -timeout 30m \
//	    -soak.duration=10m -soak.owner-writers=4 -soak.client-processes=4
//
// The second tag is not decoration: the embedded Dolt driver only builds
// with gms_pure_go, and a -tags on the command line replaces the one in
// GOFLAGS rather than adding to it, so the mandatory tag has to be repeated
// here. See docs/spikes/m0-rig1.md.
package soak

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// The load is configured by the caller, not by this file: a rig with a
// hardcoded shape can only ever answer one question.
var (
	flagDuration = flag.Duration("soak.duration", 15*time.Second,
		"how long writers and readers run")
	flagKillAfter = flag.Duration("soak.kill-after", 0,
		"how long into the unclean-kill scenario the owner is killed; 0 means two fifths of the duration")
	flagOwnerWriters = flag.Int("soak.owner-writers", 2,
		"concurrent writers inside the owning process")
	flagOwnerReaders = flag.Int("soak.owner-readers", 2,
		"concurrent readers inside the owning process")
	flagClientProcesses = flag.Int("soak.client-processes", 2,
		"client processes routed over the owner's IPC endpoint")
	flagClientWriters = flag.Int("soak.client-writers", 2,
		"concurrent writers in each client process")
	flagClientReaders = flag.Int("soak.client-readers", 2,
		"concurrent readers in each client process")
	flagWriteInterval = flag.Duration("soak.write-interval", 0,
		"pause between one writer's writes; 0 means write as fast as the store allows")
	flagReadInterval = flag.Duration("soak.read-interval", 2*time.Millisecond,
		"pause between one reader's reads")
	flagOpTimeout = flag.Duration("soak.op-timeout", 2*time.Minute,
		"how long a single store operation may take before it is abandoned")
	flagSummaryDir = flag.String("soak.summary-dir", "",
		"directory to write each scenario's machine-readable summary into")
	flagKeepScratch = flag.Bool("soak.keep-scratch", false,
		"keep the scratch repository, ledgers and stats after the run")
)

// ownerShutdownGrace is how long the owning process is allowed to outlive
// the clients' deadline while it waits to be told to stop.
const ownerShutdownGrace = 2 * time.Minute

// soakActors attribute the two halves of the load to different actors, so
// that dolt_log's provenance (PRD §3.1) can be read back under concurrent
// load rather than assumed.
var (
	ownerActor  = store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}
	clientActor = store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	rigActor    = store.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"}
)

func TestMain(m *testing.M) {
	// A helper process is this same binary, told which role to play. It
	// must not run any test.
	if raw := os.Getenv(roleEnv); raw != "" {
		os.Exit(runRole(raw))
	}
	os.Exit(m.Run())
}

// TestConcurrencySoak drives PRD §5.2.1's topology under load with a clean
// shutdown: one owning process holding the store, client processes routed
// through it, writers and readers concurrent on both sides.
func TestConcurrencySoak(t *testing.T) {
	summary := runScenario(t, "concurrency", scenarioFromFlags(0))
	assertVerdict(t, summary)
}

// TestUncleanKillAndStaleLockRecovery does the same and then kills the
// owning process without giving it a chance to clean up, which is what PRD
// §16 means by "incl. unclean-kill/stale-LOCK recovery". Everything the
// dead owner acknowledged must still be readable after the next open
// recovers the lock it left behind.
func TestUncleanKillAndStaleLockRecovery(t *testing.T) {
	killAfter := *flagKillAfter
	if killAfter <= 0 {
		killAfter = *flagDuration * 2 / 5
	}
	if killAfter >= *flagDuration {
		t.Fatalf("-soak.kill-after (%v) must be shorter than -soak.duration (%v)", killAfter, *flagDuration)
	}
	summary := runScenario(t, "unclean-kill", scenarioFromFlags(killAfter))
	assertVerdict(t, summary)
}

func scenarioFromFlags(killAfter time.Duration) ScenarioConfig {
	return ScenarioConfig{
		Duration:        *flagDuration,
		KillAfter:       killAfter,
		OwnerWriters:    *flagOwnerWriters,
		OwnerReaders:    *flagOwnerReaders,
		ClientProcesses: *flagClientProcesses,
		ClientWriters:   *flagClientWriters,
		ClientReaders:   *flagClientReaders,
		WriteInterval:   *flagWriteInterval,
		ReadInterval:    *flagReadInterval,
		OpTimeout:       *flagOpTimeout,
	}
}

func assertVerdict(t *testing.T, summary *Summary) {
	t.Helper()
	if summary.Verdict == VerdictPass {
		return
	}
	t.Errorf("rig 1 verdict for scenario %q is %s:\n  - %s",
		summary.Scenario, summary.Verdict, strings.Join(summary.VerdictReasons, "\n  - "))
}

// runScenario is the conductor. It never writes to the store while the
// soak is running: everything it concludes comes from the ledgers on disk
// and from re-opening the store after every participant is gone.
func runScenario(t *testing.T, name string, sc ScenarioConfig) *Summary {
	t.Helper()
	ctx := context.Background()
	summary := newSummary(name, sc)

	root := scratchDir(t)
	base := filepath.Join(root, "repo")
	ledgerDir := filepath.Join(root, "ledger")
	statsDir := filepath.Join(root, "stats")
	for _, dir := range []string{base, ledgerDir, statsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	nonce := newNonce(t)

	shared := roleConfig{
		BaseDir:       base,
		LedgerDir:     ledgerDir,
		StatsDir:      statsDir,
		Duration:      sc.Duration,
		WriteInterval: sc.WriteInterval,
		ReadInterval:  sc.ReadInterval,
		OpTimeout:     sc.OpTimeout,
		Nonce:         nonce,
	}

	ownerCfg := shared
	ownerCfg.Role = roleOwner
	ownerCfg.ID = "owner"
	ownerCfg.Writers = sc.OwnerWriters
	ownerCfg.Readers = sc.OwnerReaders
	ownerCfg.ActorName = ownerActor.Name
	ownerCfg.ActorEmail = ownerActor.Email
	// The owner outlives the clients' own deadline, and is stopped by the
	// conductor once they have all exited. Otherwise every scenario would
	// end with the endpoint closing under clients that are still talking to
	// it, and the clean run would report the transport failures that belong
	// to the unclean one.
	ownerCfg.Duration = sc.Duration + ownerShutdownGrace

	owner := startRole(t, ownerCfg, true)
	t.Logf("owner is pid %d on port %d", owner.pid, owner.port)

	clients := make([]*helperProcess, 0, sc.ClientProcesses)
	for i := 0; i < sc.ClientProcesses; i++ {
		clientCfg := shared
		clientCfg.Role = roleClient
		clientCfg.ID = fmt.Sprintf("client%d", i)
		clientCfg.Writers = sc.ClientWriters
		clientCfg.Readers = sc.ClientReaders
		clientCfg.ActorName = clientActor.Name
		clientCfg.ActorEmail = clientActor.Email
		clients = append(clients, startRole(t, clientCfg, false))
	}

	var killedAt time.Time
	if sc.KillAfter > 0 {
		time.Sleep(sc.KillAfter)
		killedAt = killOwner(t, ctx, base, owner, summary)
	}

	for _, client := range clients {
		if err := client.wait(); err != nil {
			t.Errorf("client %s exited badly: %v\n%s", client.id, err, client.stderrTail())
		}
	}
	if sc.KillAfter == 0 {
		owner.requestShutdown()
		if err := owner.wait(); err != nil {
			t.Errorf("owner exited badly: %v\n%s", err, owner.stderrTail())
		}
	}

	// Everything that could have been writing is gone; from here the
	// conductor is the only process that touches the store.
	recordPostRunState(t, ctx, base, summary)

	logs := &syncBuffer{}
	reopened := reopenStore(t, base, logs)
	if sc.KillAfter > 0 {
		measureStaleLockRecovery(t, ctx, base, owner, logs, summary)
	}

	stored := readStoredRows(t, ctx, reopened)
	observeStore(t, ctx, reopened, base, summary, int64(len(stored)))

	if err := reopened.Close(); err != nil {
		t.Errorf("close the re-opened store: %v", err)
	}

	ledgers, err := ReadLedgers(ledgerDir)
	if err != nil {
		t.Fatalf("read ledgers: %v", err)
	}
	reconcile(ledgers, stored, summary, killedAt)

	roles, err := readRoleStats(statsDir)
	if err != nil {
		t.Fatalf("read role stats: %v", err)
	}
	absorbRoleStats(roles, summary)

	summary.decide()
	if err := summary.Emit(os.Stdout); err != nil {
		t.Errorf("emit summary: %v", err)
	}
	if *flagSummaryDir != "" {
		path, err := summary.WriteFile(*flagSummaryDir)
		if err != nil {
			t.Errorf("write summary: %v", err)
		} else {
			t.Logf("summary written to %s", path)
		}
	}
	if owner.stderrTail() != "" {
		t.Logf("owner stderr (tail):\n%s", owner.stderrTail())
	}
	return summary
}

// killOwner ends the owning process the way a crash or a `kill -9` would:
// no signal handler, no deferred close, no chance to release anything.
func killOwner(t *testing.T, ctx context.Context, base string, owner *helperProcess, summary *Summary) time.Time {
	t.Helper()
	summary.Recovery.Performed = true
	summary.Recovery.Trigger = "os.Process.Kill (SIGKILL on Unix, TerminateProcess on Windows)"
	summary.Recovery.OwnerPID = owner.pid

	if status, _, err := ipc.Probe(ctx, base); err != nil {
		summary.Recovery.ProbeBeforeKill = "error: " + err.Error()
	} else {
		summary.Recovery.ProbeBeforeKill = status.String()
	}

	killedAt := time.Now()
	if err := owner.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the owner: %v", err)
	}

	// Sampled before the process is reaped: a pid check can still resolve a
	// handle the parent holds, which is exactly why staleness is decided by
	// the lock and not by the pid (PRD §5.2.3).
	liveness, livenessErr := singleowner.ProcessLiveness(owner.pid)
	summary.Recovery.OwnerPIDLivenessBeforeReap = liveness.String()
	if livenessErr != nil {
		summary.Recovery.OwnerPIDLivenessError = livenessErr.Error()
	}

	_ = owner.wait()

	liveness, livenessErr = singleowner.ProcessLiveness(owner.pid)
	summary.Recovery.OwnerPIDLivenessAfterReap = liveness.String()
	if livenessErr != nil && summary.Recovery.OwnerPIDLivenessError == "" {
		summary.Recovery.OwnerPIDLivenessError = livenessErr.Error()
	}

	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	lockOwner, lockState, err := singleowner.Inspect(paths.LockFile())
	if err != nil {
		t.Fatalf("inspect the store lock after the kill: %v", err)
	}
	summary.Recovery.LockStateAfterKill = lockState.String()
	summary.Recovery.StaleRecordPID = lockOwner.PID

	_, pidState, err := singleowner.Inspect(paths.PidFile())
	if err != nil {
		t.Fatalf("inspect the pidfile after the kill: %v", err)
	}
	summary.Recovery.PidfileStateAfterKill = pidState.String()

	if status, _, err := ipc.Probe(ctx, base); err != nil {
		summary.Recovery.ProbeAfterKill = "error: " + err.Error()
	} else {
		summary.Recovery.ProbeAfterKill = status.String()
	}
	return killedAt
}

// measureStaleLockRecovery records what the re-open actually did with the
// lock the killed owner left behind, and answers PRD §5.2.3's [verify]
// marker with a measurement rather than an assertion.
func measureStaleLockRecovery(t *testing.T, ctx context.Context, base string, owner *helperProcess, logs *syncBuffer, summary *Summary) {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}

	logged := logs.String()
	summary.Recovery.ReopenSucceeded = true
	summary.Recovery.LoudWarningLogged = strings.Contains(logged, "level=WARN") &&
		strings.Contains(logged, "removed a stale lock left by a dead owner")
	summary.Recovery.WarningNamedStale = strings.Contains(logged, "stale_pid="+strconv.Itoa(owner.pid))
	summary.Recovery.WarningLine = firstLineContaining(logged, "removed a stale lock")

	remaining, err := os.ReadFile(paths.LockFile())
	if err != nil {
		t.Fatalf("read the lock file after recovery: %v", err)
	}
	summary.Recovery.StaleRecordCleared = !strings.Contains(string(remaining), strconv.Itoa(owner.pid))

	// The pidfile is a second orphaned record; the next endpoint to start
	// has to recover it too (PRD §5.2.4's doctor check).
	pidLogs := &syncBuffer{}
	srv, err := ipc.Listen(ipc.Config{
		BaseDir: base,
		Logger:  slog.New(slog.NewTextHandler(pidLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		summary.Recovery.PidfileWarningLine = "listen after recovery failed: " + err.Error()
	} else {
		summary.Recovery.PidfileRecovered = strings.Contains(pidLogs.String(), "removed a stale lock left by a dead owner")
		summary.Recovery.PidfileWarningLine = firstLineContaining(pidLogs.String(), "removed a stale lock")
		if err := srv.Close(); err != nil {
			t.Errorf("close the endpoint opened to test pidfile recovery: %v", err)
		}
	}

	measureRecycledPID(t, ctx, summary)
}

// measureRecycledPID is the case a pid check cannot survive and a lock can:
// a stale record naming a pid that is very much alive, and that never held
// the lock. Nothing here simulates the failure — the pid recorded is this
// test process's own.
func measureRecycledPID(t *testing.T, ctx context.Context, summary *Summary) {
	t.Helper()
	base := filepath.Join(scratchDir(t), "recycled")
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
	record, err := json.Marshal(singleowner.Owner{
		PID:        os.Getpid(),
		Host:       host,
		ID:         "recycled-pid-probe",
		AcquiredAt: time.Now().Add(-time.Hour).UTC(),
	})
	if err != nil {
		t.Fatalf("marshal the recycled-pid record: %v", err)
	}
	if err := os.WriteFile(paths.LockFile(), record, 0o600); err != nil {
		t.Fatalf("write the recycled-pid record: %v", err)
	}

	logs := &syncBuffer{}
	st, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   rigActor,
		Logger:  slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("new store for the recycled-pid probe: %v", err)
	}
	if err := st.Open(ctx); err != nil {
		summary.Recovery.RecycledPIDProbe = "open refused: " + err.Error()
		return
	}
	summary.Recovery.RecycledPIDRecovery = true
	summary.Recovery.RecycledPIDProbe = firstLineContaining(logs.String(), "removed a stale lock")
	if err := st.Close(); err != nil {
		t.Errorf("close the recycled-pid probe store: %v", err)
	}
}

func recordPostRunState(t *testing.T, ctx context.Context, base string, summary *Summary) {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	_, lockState, err := singleowner.Inspect(paths.LockFile())
	if err != nil {
		t.Fatalf("inspect the store lock after the run: %v", err)
	}
	summary.Store.LockStateAfterRun = lockState.String()

	_, pidState, err := singleowner.Inspect(paths.PidFile())
	if err != nil {
		t.Fatalf("inspect the pidfile after the run: %v", err)
	}
	summary.Store.PidStateAfterRun = pidState.String()

	if status, _, err := ipc.Probe(ctx, base); err != nil {
		summary.Store.ProbeAfterRun = "error: " + err.Error()
	} else {
		summary.Store.ProbeAfterRun = status.String()
	}
}

func reopenStore(t *testing.T, base string, logs *syncBuffer) *localdolt.Store {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   rigActor,
		Logger:  slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("new store for the re-open: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("re-open the store after the run: %v", err)
	}
	return st
}

type storedRow struct {
	payload string
	digest  string
}

func readStoredRows(t *testing.T, ctx context.Context, st *localdolt.Store) map[string]storedRow {
	t.Helper()
	rows, err := st.Query(ctx, selectAllSQL)
	if err != nil {
		t.Fatalf("read back the probe table: %v", err)
	}
	defer func() { _ = rows.Close() }()

	stored := make(map[string]storedRow)
	for rows.Next() {
		var id, payload, digest string
		if err := rows.Scan(&id, &payload, &digest); err != nil {
			t.Fatalf("scan the probe table: %v", err)
		}
		stored[id] = storedRow{payload: payload, digest: digest}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the probe table: %v", err)
	}
	return stored
}

func observeStore(t *testing.T, ctx context.Context, st *localdolt.Store, base string, summary *Summary, rowCount int64) {
	t.Helper()
	summary.Store.RowsInProbeTable = rowCount

	if commits, err := scanCount(ctx, st, "SELECT COUNT(*) FROM dolt_log"); err != nil {
		summary.addFinding("could not count dolt_log commits: %v", err)
	} else {
		summary.Store.DoltCommits = commits
	}

	rows, err := st.Query(ctx, "SELECT committer, COUNT(*) FROM dolt_log GROUP BY committer")
	if err != nil {
		summary.addFinding("could not read commit attribution from dolt_log: %v", err)
	} else {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var committer string
			var count int
			if err := rows.Scan(&committer, &count); err != nil {
				summary.addFinding("could not scan commit attribution: %v", err)
				break
			}
			summary.Store.CommitsByCommitter[committer] = count
		}
		if err := rows.Err(); err != nil {
			summary.addFinding("could not read commit attribution: %v", err)
		}
	}

	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	size, err := directorySize(paths.DoltDataDir())
	if err != nil {
		summary.addFinding("could not measure the data directory: %v", err)
	} else {
		summary.Store.DataDirBytes = size
	}
}

func scanCount(ctx context.Context, st *localdolt.Store, query string) (int64, error) {
	rows, err := st.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, fmt.Errorf("%q returned no row: %w", query, rows.Err())
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	return count, rows.Err()
}

// reconcile compares what the writers recorded they were told against what
// the re-opened store actually holds.
func reconcile(ledgers map[string]*WriterLedger, stored map[string]storedRow, summary *Summary, killedAt time.Time) {
	type attempt struct {
		intent *Record
		result *Record
	}

	seen := make(map[string]bool, len(stored))
	writers := make([]string, 0, len(ledgers))
	for name := range ledgers {
		writers = append(writers, name)
	}
	sort.Strings(writers)

	committedAfterKill := 0

	for _, name := range writers {
		ledger := ledgers[name]
		if ledger.TornTail {
			summary.Writes.TornLedgers = append(summary.Writes.TornLedgers, name)
		}

		order := make([]string, 0, len(ledger.Records))
		attempts := make(map[string]*attempt, len(ledger.Records))
		maxSeq := -1
		for i := range ledger.Records {
			rec := &ledger.Records[i]
			entry, ok := attempts[rec.Key]
			if !ok {
				entry = &attempt{}
				attempts[rec.Key] = entry
				order = append(order, rec.Key)
			}
			switch rec.Phase {
			case PhaseIntent:
				entry.intent = rec
			case PhaseResult:
				entry.result = rec
			}
			if rec.Seq > maxSeq {
				maxSeq = rec.Seq
			}
		}

		for _, key := range order {
			entry := attempts[key]
			if entry.intent == nil {
				// A result with no intent cannot happen unless the ledger is
				// not what it claims to be; count it so it cannot hide.
				summary.Writes.Unledgered++
				summary.Writes.UnledgeredKeys = appendCapped(summary.Writes.UnledgeredKeys, key+" (result without intent)")
				continue
			}
			seen[key] = true
			summary.Writes.Attempted++

			row, present := stored[key]
			intact := present && row.payload == entry.intent.Payload && row.digest == entry.intent.Digest

			if entry.result == nil {
				summary.Writes.NoResult++
				if present {
					summary.Writes.NoResultPresent++
				} else {
					summary.Writes.NoResultAbsent++
				}
				continue
			}

			switch entry.result.Outcome {
			case OutcomeCommitted:
				summary.Writes.Committed++
				if !killedAt.IsZero() && time.Unix(0, entry.result.AtUnixNS).After(killedAt) {
					committedAfterKill++
				}
				switch {
				case !present:
					summary.Writes.Lost++
					summary.Writes.LostKeys = appendCapped(summary.Writes.LostKeys, key)
				case !intact:
					summary.Writes.Corrupted++
					summary.Writes.CorruptedKeys = appendCapped(summary.Writes.CorruptedKeys, key)
				}
			case OutcomeRefused:
				summary.Writes.Refused++
				if present {
					summary.Writes.Phantom++
					summary.Writes.PhantomKeys = appendCapped(summary.Writes.PhantomKeys, key)
				}
			case OutcomeIndeterminate:
				summary.Writes.Indeterminate++
				if present {
					summary.Writes.IndeterminatePresent++
				} else {
					summary.Writes.IndeterminateAbsent++
				}
			}
		}

		// A tail torn by the kill describes exactly one write, the next one
		// this writer would have made. If that row is in the store, it is
		// accounted for here rather than counted as a row nobody asked for.
		if ledger.TornTail {
			tornKey := fmt.Sprintf("%s/%06d", name, maxSeq+1)
			if _, present := stored[tornKey]; present && !seen[tornKey] {
				seen[tornKey] = true
				summary.Writes.Attempted++
				summary.Writes.NoResult++
				summary.Writes.NoResultPresent++
			}
		}
	}

	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			summary.Writes.Unledgered++
			summary.Writes.UnledgeredKeys = appendCapped(summary.Writes.UnledgeredKeys, key)
		}
	}

	if summary.Recovery.Performed {
		summary.Recovery.CommittedBeforeKill = summary.Writes.Committed - committedAfterKill
		summary.Recovery.MissingAfterRecovery = summary.Writes.Lost
		summary.Recovery.PresentAfterRecovery = summary.Recovery.CommittedBeforeKill - summary.Writes.Lost
		summary.Recovery.MissingKeys = summary.Writes.LostKeys
		switch {
		case !summary.Recovery.ReopenSucceeded:
			summary.Recovery.Outcome = "the store could not be re-opened"
		case !summary.Recovery.LoudWarningLogged:
			summary.Recovery.Outcome = "the store re-opened but the stale lock removal was not logged loudly"
		case !summary.Recovery.StaleRecordCleared:
			summary.Recovery.Outcome = "the store re-opened but the dead owner's record survived"
		case summary.Recovery.MissingAfterRecovery > 0:
			summary.Recovery.Outcome = "recovered, but writes acknowledged before the kill are missing"
		default:
			summary.Recovery.Outcome = "recovered"
		}
		if committedAfterKill > 0 {
			summary.addFinding(
				"%d write(s) are recorded as committed after the owner was killed, which should be impossible",
				committedAfterKill)
		}
	} else {
		summary.Recovery.Outcome = "not attempted in this scenario"
	}

	if summary.Writes.Phantom > 0 {
		summary.addFinding(
			"%d write(s) reported to the writer as failed are present in the store; a caller told 'no' will retry them",
			summary.Writes.Phantom)
	}
	if summary.Writes.IndeterminatePresent > 0 {
		summary.addFinding(
			"%d write(s) whose answer was lost are present in the store, and %d are absent; neither is scored",
			summary.Writes.IndeterminatePresent, summary.Writes.IndeterminateAbsent)
	}
	if len(summary.Writes.TornLedgers) > 0 {
		summary.addFinding("ledger tails torn by the kill: %s", strings.Join(summary.Writes.TornLedgers, ", "))
	}
}

// appendCappedLimit bounds how many example keys a summary carries.
const appendCappedLimit = 50

func appendCapped(list []string, value string) []string {
	if len(list) >= appendCappedLimit {
		return list
	}
	return append(list, value)
}

func absorbRoleStats(roles []*RoleStats, summary *Summary) {
	summary.Roles = roles
	for _, role := range roles {
		summary.Reads.Attempted += role.ReadsAttempted
		summary.Reads.Failed += role.ReadsFailed
		summary.Reads.MissingCommittedRow += role.ReadsMissingCommittedRow
		summary.Reads.MismatchedPayload += role.ReadsMismatchedPayload
		summary.Reads.RowCountRegressions += role.RowCountRegressions
		for _, key := range role.MissingKeySamples {
			summary.Reads.MissingKeySamples = appendCapped(summary.Reads.MissingKeySamples, role.ID+": "+key)
		}
		summary.Errors.merge(role.Errors)
	}
	if summary.Reads.RowCountRegressions > 0 {
		summary.addFinding("%d reader observation(s) saw the row count go backwards", summary.Reads.RowCountRegressions)
	}
	if summary.Errors.total() > 0 {
		summary.addFinding("error classes observed: %s", strings.Join(summary.Errors.classes(), ", "))
	}
}

// helperProcess is one participant, running as its own operating-system
// process.
type helperProcess struct {
	id     string
	cmd    *exec.Cmd
	pid    int
	port   int
	stdin  io.WriteCloser
	stderr *syncBuffer

	once    sync.Once
	waitErr error
}

func startRole(t *testing.T, cfg roleConfig, waitReady bool) *helperProcess {
	t.Helper()

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode role configuration: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), roleEnv+"="+string(encoded))

	stderr := &syncBuffer{}
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s stdin pipe: %v", cfg.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("%s stdout pipe: %v", cfg.ID, err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cfg.ID, err)
	}

	h := &helperProcess{id: cfg.ID, cmd: cmd, stdin: stdin, stderr: stderr}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		h.once.Do(func() { h.waitErr = cmd.Wait() })
	})
	if waitReady {
		ready := make(chan string, 1)
		go func() { ready <- readLine(stdout) }()

		select {
		case line := <-ready:
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != "READY" {
				t.Fatalf("%s did not announce itself (got %q)\n%s", cfg.ID, line, stderr.String())
			}
			if h.pid, err = strconv.Atoi(fields[1]); err != nil {
				t.Fatalf("parse %s pid from %q: %v", cfg.ID, line, err)
			}
			if h.port, err = strconv.Atoi(fields[2]); err != nil {
				t.Fatalf("parse %s port from %q: %v", cfg.ID, line, err)
			}
		case <-time.After(readyTimeout):
			t.Fatalf("%s never became ready\n%s", cfg.ID, stderr.String())
		}
	}

	// Only now: whatever a role prints after its greeting is diagnostic,
	// and draining it keeps the child from blocking on a full pipe. Started
	// any earlier it would race the reader above for the greeting itself.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return h
}

// readyTimeout bounds how long a role may take to announce itself. Opening
// the embedded engine is the slow part and takes well under a second.
const readyTimeout = 60 * time.Second

func readLine(r io.Reader) string {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// requestShutdown asks a role to stop cleanly by closing its stdin.
func (h *helperProcess) requestShutdown() {
	_ = h.stdin.Close()
}

func (h *helperProcess) wait() error {
	h.once.Do(func() { h.waitErr = h.cmd.Wait() })
	return h.waitErr
}

func (h *helperProcess) stderrTail() string {
	const tail = 4000
	text := h.stderr.String()
	if len(text) <= tail {
		return text
	}
	return "…" + text[len(text)-tail:]
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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

// scratchDir returns a run directory. It does not use t.TempDir because
// that fails the test if the directory cannot be removed, and the embedded
// engine memory-maps files inside the Dolt data directory that Windows may
// still hold briefly after the store is closed.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-soak")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() {
		if *flagKeepScratch {
			t.Logf("scratch kept at %s", dir)
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("scratch directory %s could not be removed: %v", dir, err)
		}
	})
	return dir
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newNonce(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate run nonce: %v", err)
	}
	return hex.EncodeToString(buf)
}

func firstLineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimRight(line, "\r")
		}
	}
	return ""
}

func directorySize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	return total, nil
}
