//go:build soak

package soak

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	// Registers the "dolt" database/sql driver, so that this test can open
	// the data directory the way something that has never heard of memdolt
	// would.
	_ "github.com/dolthub/driver"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

const (
	// foreignProbeDuration is how long the owner runs; the probe happens
	// well inside it.
	foreignProbeDuration = 25 * time.Second

	// foreignProbeSettle lets the owner commit some writes before anything
	// else touches the data directory, so the probe runs against a store in
	// use rather than an idle one.
	foreignProbeSettle = 4 * time.Second

	foreignStepTimeout = 60 * time.Second
)

// TestForeignOpenerAgainstALiveOwner measures the failure mode PRD §5.2 and
// risk R1 describe wrongly.
//
// Both expect a second process on the same data directory to hit "database
// is locked". Two things are actually true, and the difference is the whole
// reason memdolt keeps a lock of its own: a second *memdolt* process is
// refused before the driver is involved, loudly and immediately; a process
// that does not take memdolt's lock is not refused at all, and finds out
// much later and much more quietly. The exact shape of "much later" is what
// this measures, because a client that believes it is writing and is not is
// precisely the data-loss event the §16 gate is about.
func TestForeignOpenerAgainstALiveOwner(t *testing.T) {
	ctx := context.Background()

	sc := ScenarioConfig{
		Duration:      foreignProbeDuration,
		OwnerWriters:  1,
		OwnerReaders:  1,
		WriteInterval: *flagWriteInterval,
		ReadInterval:  *flagReadInterval,
		OpTimeout:     *flagOpTimeout,
	}
	summary := newSummary("foreign-opener", sc)
	observation := &ForeignOpener{StepOutcomes: map[string]string{}}
	summary.Foreign = observation

	root := scratchDir(t)
	base := filepath.Join(root, "repo")
	ledgerDir := filepath.Join(root, "ledger")
	statsDir := filepath.Join(root, "stats")
	for _, dir := range []string{base, ledgerDir, statsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	owner := startRole(t, roleConfig{
		Role:          roleOwner,
		ID:            "owner",
		BaseDir:       base,
		LedgerDir:     ledgerDir,
		StatsDir:      statsDir,
		Writers:       sc.OwnerWriters,
		Readers:       sc.OwnerReaders,
		Duration:      foreignProbeDuration,
		WriteInterval: sc.WriteInterval,
		ReadInterval:  sc.ReadInterval,
		OpTimeout:     sc.OpTimeout,
		Nonce:         newNonce(t),
		ActorName:     ownerActor.Name,
		ActorEmail:    ownerActor.Email,
	}, true)
	t.Logf("owner is pid %d on port %d", owner.pid, owner.port)

	time.Sleep(foreignProbeSettle)

	probeSecondMemdolt(t, ctx, base, observation)
	probeForeignDriver(t, ctx, base, observation)
	// A step the probe never reached says something too: the write-up needs
	// to name which step is the first to refuse a foreign writer, and an
	// absent key would leave that ambiguous.
	for _, step := range []string{"open", "ping", "read", "write", "commit"} {
		if _, ok := observation.StepOutcomes[step]; !ok {
			observation.StepOutcomes[step] = "not attempted: an earlier step failed"
		}
	}
	probeOwnerStillWorks(t, ctx, base, observation, summary)

	owner.requestShutdown()
	if err := owner.wait(); err != nil {
		t.Errorf("owner exited badly after the foreign-opener probe: %v\n%s", err, owner.stderrTail())
	}

	recordPostRunState(t, ctx, base, summary)
	logs := &syncBuffer{}
	reopened := reopenStore(t, base, logs)
	stored := readStoredRows(t, ctx, reopened)
	observeStore(t, ctx, reopened, base, summary, int64(len(stored)))
	if err := reopened.Close(); err != nil {
		t.Errorf("close the re-opened store: %v", err)
	}

	ledgers, err := ReadLedgers(ledgerDir)
	if err != nil {
		t.Fatalf("read ledgers: %v", err)
	}
	reconcile(ledgers, stored, summary, time.Time{})

	roles, err := readRoleStats(statsDir)
	if err != nil {
		t.Fatalf("read role stats: %v", err)
	}
	absorbRoleStats(roles, summary)

	if observation.StepOutcomes["write"] == "ok" && observation.StepOutcomes["commit"] == "ok" {
		summary.addFinding(
			"a process that does not take memdolt's lock committed to the data directory while the owner held it")
	}
	if !observation.SecondMemdoltMatchedError {
		summary.addFinding(
			"a second memdolt process was not refused with store.ErrLocked: %s", observation.SecondMemdoltOpenError)
	}

	summary.decide()
	if err := summary.Emit(os.Stdout); err != nil {
		t.Errorf("emit summary: %v", err)
	}
	if *flagSummaryDir != "" {
		if path, err := summary.WriteFile(*flagSummaryDir); err != nil {
			t.Errorf("write summary: %v", err)
		} else {
			t.Logf("summary written to %s", path)
		}
	}
	assertVerdict(t, summary)

	// The single-owner rule is what PRD §5.2.1 promises, so a second
	// memdolt process failing to be refused is a failure of this test, not
	// merely a finding.
	if !observation.SecondMemdoltMatchedError {
		t.Errorf("a second memdolt process opened a store a live owner holds; §5.2.1's single-owner rule does not hold")
	}
}

func probeSecondMemdolt(t *testing.T, ctx context.Context, base string, observation *ForeignOpener) {
	t.Helper()
	second, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: rigActor, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("new second store: %v", err)
	}
	start := time.Now()
	err = second.Open(ctx)
	observation.SecondMemdoltOpenMillis = time.Since(start).Milliseconds()
	if err == nil {
		observation.SecondMemdoltOpenError = "<none: the open succeeded>"
		_ = second.Close()
		return
	}
	observation.SecondMemdoltOpenError = err.Error()
	observation.SecondMemdoltMatchedError = errors.Is(err, store.ErrLocked)
}

// probeForeignDriver opens the Dolt data directory with the embedded driver
// directly, taking no memdolt lock, and records what each step does.
func probeForeignDriver(t *testing.T, ctx context.Context, base string, observation *ForeignOpener) {
	t.Helper()

	dsn, dataDir := foreignDSN(t, base)
	step := func(name string, err error) {
		if err == nil {
			observation.StepOutcomes[name] = "ok"
			return
		}
		observation.StepOutcomes[name] = classify(err)
	}

	db, err := sql.Open("dolt", dsn)
	step("open", err)
	if err != nil {
		observation.DriverOpenError = err.Error()
		return
	}
	defer func() { _ = db.Close() }()
	t.Logf("a foreign process opened %s with no memdolt lock", dataDir)

	pingCtx, cancel := context.WithTimeout(ctx, foreignStepTimeout)
	defer cancel()
	err = db.PingContext(pingCtx)
	step("ping", err)
	if err != nil {
		observation.DriverPingError = err.Error()
		return
	}

	readCtx, cancelRead := context.WithTimeout(ctx, foreignStepTimeout)
	defer cancelRead()
	row := db.QueryRowContext(readCtx, countSQL)
	var count int64
	err = row.Scan(&count)
	step("read", err)
	if err != nil {
		observation.DriverReadError = err.Error()
	} else {
		observation.DriverReadRows = count
	}

	// A table of its own, so that a foreign write that succeeds cannot be
	// mistaken for one of the soak's own rows going missing or appearing.
	writeCtx, cancelWrite := context.WithTimeout(ctx, foreignStepTimeout)
	defer cancelWrite()
	_, err = db.ExecContext(writeCtx, "CREATE TABLE IF NOT EXISTS foreign_probe (k VARCHAR(64) PRIMARY KEY)")
	if err == nil {
		_, err = db.ExecContext(writeCtx, "INSERT INTO foreign_probe (k) VALUES (?)", "written-behind-the-owner")
	}
	step("write", err)
	if err != nil {
		observation.DriverWriteError = err.Error()
		return
	}

	commitCtx, cancelCommit := context.WithTimeout(ctx, foreignStepTimeout)
	defer cancelCommit()
	var hash string
	err = db.QueryRowContext(commitCtx,
		"CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
		"foreign write", "foreign <foreign@memdolt.invalid>").Scan(&hash)
	step("commit", err)
	if err != nil {
		observation.DriverCommitError = err.Error()
	}
}

// probeOwnerStillWorks checks that the owner can still commit after
// something else has had the data directory open.
func probeOwnerStillWorks(t *testing.T, ctx context.Context, base string, observation *ForeignOpener, summary *Summary) {
	t.Helper()
	client, err := storeipc.Dial(base)
	if err != nil {
		observation.OwnerCommitError = "dial: " + err.Error()
		return
	}
	// Its own table, for the same reason the foreign write has one.
	_, err = client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE IF NOT EXISTS owner_marker (k VARCHAR(64) PRIMARY KEY)"},
			{SQL: "INSERT INTO owner_marker (k) VALUES (?)", Args: []any{"after-the-foreign-open"}},
		},
		NoText:  true,
		Message: "soak: owner writes after a foreign process opened the data directory",
		Author:  storeipc.Actor{Name: ownerActor.Name, Email: ownerActor.Email},
	})
	if err != nil {
		observation.OwnerCommitError = err.Error()
		summary.addFinding("the owner could not commit after a foreign process opened the data directory: %v", err)
		return
	}
	observation.OwnerStillCommits = true
}

// foreignDSN renders the driver's data source name without going through
// localdolt's own builder, which is the point: this is what a process that
// does not use memdolt's code would hand the driver. The duplication is
// deliberate and confined to this rig. The path still comes from
// internal/layout, because it is memdolt's layout either way.
func foreignDSN(t *testing.T, base string) (string, string) {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	abs, err := filepath.Abs(paths.DoltDataDir())
	if err != nil {
		t.Fatalf("resolve %s: %v", paths.DoltDataDir(), err)
	}
	params := url.Values{}
	params.Set("commitname", "foreign")
	params.Set("commitemail", "foreign@memdolt.invalid")
	params.Set("database", localdolt.DatabaseName)
	return fmt.Sprintf("file://%s?%s", filepath.ToSlash(abs), params.Encode()), abs
}
