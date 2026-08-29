package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// The three verdicts a check can reach.
//
// The distinction that matters is between fail and warn, and it is not
// severity: it is whether memdolt repairs the condition by itself. A stale
// lock record and an orphaned pidfile are both cleared by the next open
// (PRD §5.2.3), so reporting them is useful and exiting nonzero for them
// would train an operator to ignore the exit code. Everything memdolt
// cannot resolve on its own fails.
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// doctorCheck is one check's verdict, in the shape --json emits.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctorReport is what `memdolt doctor` prints. OK is false when any check
// failed, which is also when the command exits nonzero.
type doctorReport struct {
	Dir    string        `json:"dir"`
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func newDoctorCommand() *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report store ownership, IPC reachability, schema skew, and empty recalls",
		Long: "Run the store-health checks of PRD §5.2 and §6.4 plus the empty-recall\n" +
			"observability check of PRD §8.1 against this repository:\n\n" +
			"  store-lock      who, if anyone, owns the store — held, an orphaned\n" +
			"                  ownership record, or absent\n" +
			"  ipc             whether a live owner answers on its loopback endpoint\n" +
			"  schema-version  whether the store's schema is newer than this binary\n" +
			"  empty-recall-rate  local empty-above-floor recall count and rate\n\n" +
			"Against a directory with no store, doctor reports that rather than initializing\n" +
			"one or creating its directory. With no live owner, it opens an existing store\n" +
			"directly to read its schema; this briefly takes the ownership lock and may\n" +
			"create .memdolt/LOCK, but makes no durable database change. It exits\n" +
			"nonzero when a check fails, and zero for a condition memdolt clears by itself\n" +
			"(a stale lock record, an orphaned pidfile), which it reports as a warning.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, dir)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".",
		"repository root to check (the store lives in <dir>/.memdolt)")

	return cmd
}

func runDoctor(cmd *cobra.Command, dir string) error {
	paths, err := layout.New(dir)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	// The IPC check runs before the schema check because its answer
	// decides how the schema check may read the store: a live owner holds
	// the data directory, and PRD §5.2.1 routes every other process
	// through it rather than around it.
	owner, ownerLive := ownerCheck(ctx, paths)
	report := doctorReport{
		Dir: paths.Dir(),
		OK:  true,
		Checks: []doctorCheck{
			lockCheck(paths),
			owner,
			schemaCheck(ctx, paths, ownerLive),
			emptyRecallCheck(ctx, paths),
		},
	}

	failed := 0
	for _, check := range report.Checks {
		if check.Status == statusFail {
			failed++
		}
	}
	report.OK = failed == 0

	if err := writeDoctorReport(cmd, report); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("doctor: %d of %d checks failed", failed, len(report.Checks))
	}
	return nil
}

// emptyRecallCheck reports the machine-local observability counter required by
// PRD §8.1. Reading it never creates a missing embeddings.sqlite file.
func emptyRecallCheck(ctx context.Context, paths layout.Paths) doctorCheck {
	const name = "empty-recall-rate"
	status, err := embedding.ReadObservability(ctx, paths.EmbeddingsFile())
	if err != nil {
		return newCheck(name, statusFail, "%v", err)
	}
	if status.RecallCount == 0 {
		return newCheck(name, statusOK, "0 empty of 0 recall calls (rate 0.0%%; no calls recorded)")
	}
	return newCheck(name, statusOK, "%d empty of %d recall calls (rate %.1f%%)",
		status.EmptyCount, status.RecallCount, status.EmptyRate*100)
}

// lockCheck reports the ownership state of the store's own lock file
// (PRD §5.2.4's "stale LOCK"). It takes the lock for a moment to find out
// whether anyone else holds it, and gives it straight back.
func lockCheck(paths layout.Paths) doctorCheck {
	const name = "store-lock"

	owner, state, err := singleowner.Inspect(paths.LockFile())
	if err != nil {
		return newCheck(name, statusFail, "%v", err)
	}
	switch state {
	case singleowner.StateHeld:
		return newCheck(name, statusOK, "held: a live process owns the store (%s)", describeOwner(owner))
	case singleowner.StateStale:
		return newCheck(name, statusWarn,
			"an orphaned ownership record (%s) survives its owner; the next store open clears it (PRD §5.2.3)",
			describeOwner(owner))
	default:
		return newCheck(name, statusOK, "absent: no process owns the store")
	}
}

// ownerCheck reports whether a live MCP server owns the store and answers
// on its loopback endpoint (PRD §5.2.4's "orphaned pidfile, IPC
// reachability"). It also reports whether the store is held by a live
// owner, which is what the schema check needs to know.
func ownerCheck(ctx context.Context, paths layout.Paths) (doctorCheck, bool) {
	const name = "ipc"

	status, info, err := ipc.Probe(ctx, paths.Base())
	if err != nil {
		// Probe fails closed: a pidfile it cannot read, or a live holder
		// whose endpoint does not answer with a matching identity, is an
		// error rather than a verdict. Either way no CLI may open the
		// store, so it is a failure and not a warning.
		return newCheck(name, statusFail, "%v", err), false
	}
	switch status {
	case ipc.StatusOwnerLive:
		return newCheck(name, statusOK,
			"reachable: the owner (pid %d) answered on 127.0.0.1:%d", info.PID, info.Port), true
	case ipc.StatusOwnerDead:
		return newCheck(name, statusWarn,
			"an orphaned pidfile from pid %d survives its owner; the next server start clears it (PRD §5.2.3)",
			info.PID), false
	default:
		return newCheck(name, statusOK,
			"absent: no server owns this store, so a cli opens it directly (PRD §5.2.1)"), false
	}
}

// schemaCheck reports whether the store's schema is newer than the
// migrations this binary ships (PRD §6.4). A store that does not exist yet
// is not a failure — it is the answer, and doctor reports it without
// creating one.
func schemaCheck(ctx context.Context, paths layout.Paths, ownerLive bool) doctorCheck {
	const name = "schema-version"
	latest := store.LatestSchemaVersion()

	if _, err := os.Stat(paths.DoltDataDir()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return newCheck(name, statusOK,
				"absent: no store at %s yet; `memdolt init` creates one at schema v%d",
				paths.DoltDataDir(), latest)
		}
		return newCheck(name, statusFail, "look for the store at %s: %v", paths.DoltDataDir(), err)
	}

	version, err := readSchemaVersion(ctx, paths, ownerLive)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			// Another memdolt process holds the store without serving an
			// endpoint — a second CLI, mid-run. It will let go; doctor
			// does not queue behind it.
			return newCheck(name, statusWarn,
				"another process holds the store lock, so its schema version was not read: %v", err)
		}
		return newCheck(name, statusFail, "%v", err)
	}
	if err := store.CheckSchemaVersion(version); err != nil {
		return newCheck(name, statusFail, "%v", err)
	}
	return newCheck(name, statusOK, "store is at schema v%d; this binary knows v%d", version, latest)
}

// readSchemaVersion reads meta.schema_version the way PRD §5.2.1 says a
// CLI reads anything: through the owner when one is live, directly when
// none is.
//
// Opening directly is the case that also answers the question by failing —
// Open refuses a store newer than this binary (PRD §6.4), and that refusal
// is what the caller reports.
func readSchemaVersion(ctx context.Context, paths layout.Paths, ownerLive bool) (int, error) {
	if ownerLive {
		return schemaVersionOverIPC(ctx, paths.Base())
	}

	st, err := localdolt.New(localdolt.Config{BaseDir: paths.Base(), Actor: cliActor})
	if err != nil {
		return 0, err
	}
	if err := st.Open(ctx); err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()
	return st.SchemaVersion(ctx)
}

// schemaVersionOverIPC reads meta.schema_version through the process that
// owns the store.
//
// It asks the two questions localdolt asks of its own connection, in the
// same order and for the same reason: a store with no meta table has not
// been initialized and is at version 0, and its absence is established
// through information_schema rather than by matching the text of Dolt's
// "table not found" error.
func schemaVersionOverIPC(ctx context.Context, base string) (int, error) {
	client, err := storeipc.Dial(base)
	if err != nil {
		return 0, fmt.Errorf("reach the store's owner: %w", err)
	}

	tables, err := queryCell(ctx, client,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?",
		localdolt.DatabaseName, store.MetaTable)
	if err != nil {
		return 0, err
	}
	count, err := parseCell(tables, "the number of "+store.MetaTable+" tables")
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	raw, err := queryCell(ctx, client,
		"SELECT v FROM "+store.MetaTable+" WHERE k = ?", store.SchemaVersionKey)
	if err != nil {
		return 0, err
	}
	return parseCell(raw, store.MetaTable+"."+store.SchemaVersionKey)
}

// queryCell runs a statement that returns at most one row of one column,
// and reports nil for no row.
func queryCell(ctx context.Context, client *storeipc.Client, sql string, args ...any) (*string, error) {
	grid, err := client.Query(ctx, storeipc.QueryRequest{SQL: sql, Args: args})
	if err != nil {
		return nil, err
	}
	if len(grid.Rows) == 0 {
		return nil, nil
	}
	if len(grid.Rows[0]) != 1 {
		return nil, fmt.Errorf("the owner answered with %d columns, want 1", len(grid.Rows[0]))
	}
	return grid.Rows[0][0], nil
}

// parseCell reads a whole number out of a cell the owner sent as text. An
// absent cell — no row, or SQL NULL — is zero, which is what an
// uninitialized store reports.
func parseCell(cell *string, what string) (int, error) {
	if cell == nil {
		return 0, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(*cell))
	if err != nil {
		return 0, fmt.Errorf("the owner reports %s as %q, which is not a number", what, *cell)
	}
	return value, nil
}

// describeOwner renders a lock record for an operator. A record with no
// pid was either unreadable or still being written when it was read;
// singleowner reads it without the lock, on purpose.
func describeOwner(owner singleowner.Owner) string {
	if owner.PID == 0 {
		return "no readable ownership record"
	}
	return fmt.Sprintf("pid %d on %s since %s",
		owner.PID, owner.Host, owner.AcquiredAt.Format(time.RFC3339))
}

func newCheck(name, status, format string, args ...any) doctorCheck {
	return doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)}
}

func writeDoctorReport(cmd *cobra.Command, report doctorReport) error {
	out := cmd.OutOrStdout()

	if jsonOutput {
		encoded, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encode doctor report as json: %w", err)
		}
		if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
			return fmt.Errorf("write doctor json: %w", err)
		}
		return nil
	}

	if _, err := fmt.Fprintf(out, "memdolt doctor: %s\n", report.Dir); err != nil {
		return fmt.Errorf("write doctor line: %w", err)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "  %-4s  %-14s  %s\n", check.Status, check.Name, check.Detail); err != nil {
			return fmt.Errorf("write doctor line: %w", err)
		}
	}
	return nil
}
