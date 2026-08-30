package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/layout"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// TestDoctorReportsAnAbsentStoreWithoutCreatingOne covers the case doctor
// has to survive rather than crash on: a directory memdolt has never
// touched. Every check reports absence, the command succeeds, and — the
// part that would be a real bug — nothing is created on the way.
func TestDoctorReportsAnAbsentStoreWithoutCreatingOne(t *testing.T) {
	base := scratchDir(t)

	report := runDoctorJSON(t, base)

	if !report.OK {
		t.Fatalf("doctor on an empty directory reported not-ok: %+v", report.Checks)
	}
	for _, name := range []string{"store-lock", "ipc", "schema-version", "empty-recall-rate"} {
		check := findCheck(t, report, name)
		if check.Status != statusOK {
			t.Fatalf("check %q = %s (%s), want ok", name, check.Status, check.Detail)
		}
		if name != "empty-recall-rate" && !strings.Contains(check.Detail, "absent") {
			t.Fatalf("check %q says %q, want it to report the absence", name, check.Detail)
		}
	}

	paths := pathsFor(t, base)
	if _, err := os.Stat(paths.Dir()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("doctor created %s (stat err %v); it must only report", paths.Dir(), err)
	}
}

// TestDoctorReportsALiveOwner covers PRD §5.2.4's "IPC reachability": with
// a process holding the store and serving its endpoint, doctor reports the
// lock as held, the endpoint as reachable, and reads the schema version
// through the owner rather than trying to open a store it may not have.
func TestDoctorReportsALiveOwner(t *testing.T) {
	base := scratchDir(t)
	runMemdolt(t, "init", "--dir", base)

	owner := serveStore(t, base)

	report := runDoctorJSON(t, base)

	if !report.OK {
		t.Fatalf("doctor against a live owner reported not-ok: %+v", report.Checks)
	}
	if got := findCheck(t, report, "store-lock"); !strings.Contains(got.Detail, "held") {
		t.Fatalf("store-lock check says %q, want it to report the store as held", got.Detail)
	}
	ipcCheck := findCheck(t, report, "ipc")
	if !strings.Contains(ipcCheck.Detail, "reachable") {
		t.Fatalf("ipc check says %q, want it to report the endpoint as reachable", ipcCheck.Detail)
	}
	if !strings.Contains(ipcCheck.Detail, strconv.Itoa(owner.Port())) {
		t.Fatalf("ipc check says %q, want it to name the port %d", ipcCheck.Detail, owner.Port())
	}

	// The schema version came over the wire: nothing else could have read
	// it while the owner holds the data directory.
	want := "schema v" + strconv.Itoa(store.LatestSchemaVersion())
	if got := findCheck(t, report, "schema-version"); !strings.Contains(got.Detail, want) {
		t.Fatalf("schema-version check says %q, want it to report %q", got.Detail, want)
	}
}

// TestDoctorWarnsAboutAnOrphanedPidfile covers PRD §5.2.4's "orphaned
// pidfile". memdolt clears one on the next server start (§5.2.3), so
// doctor reports it and still exits zero.
func TestDoctorWarnsAboutAnOrphanedPidfile(t *testing.T) {
	base := scratchDir(t)
	paths := pathsFor(t, base)
	if err := os.MkdirAll(paths.Dir(), 0o755); err != nil {
		t.Fatalf("create %s: %v", paths.Dir(), err)
	}

	if err := os.WriteFile(paths.PidFile(), []byte(ownershipRecord(t)+"\n"), 0o600); err != nil {
		t.Fatalf("write an orphaned pidfile: %v", err)
	}

	report := runDoctorJSON(t, base)

	if !report.OK {
		t.Fatalf("doctor reported not-ok for a condition it recovers from: %+v", report.Checks)
	}
	check := findCheck(t, report, "ipc")
	if check.Status != statusWarn {
		t.Fatalf("ipc check = %s (%s), want warn", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "orphaned pidfile") {
		t.Fatalf("ipc check says %q, want it to report the orphaned pidfile", check.Detail)
	}
}

// TestDoctorWarnsAboutAStaleStoreLock covers the third ownership state of
// PRD §5.2.4's "stale LOCK": an ownership record on the store's own lock
// file that outlived the process which wrote it. The next open clears it
// (§5.2.3), so doctor reports it and still exits zero.
func TestDoctorWarnsAboutAStaleStoreLock(t *testing.T) {
	base := scratchDir(t)
	paths := pathsFor(t, base)
	if err := os.MkdirAll(paths.Dir(), 0o755); err != nil {
		t.Fatalf("create %s: %v", paths.Dir(), err)
	}
	if err := os.WriteFile(paths.LockFile(), []byte(ownershipRecord(t)+"\n"), 0o600); err != nil {
		t.Fatalf("write a stale lock record: %v", err)
	}

	report := runDoctorJSON(t, base)

	if !report.OK {
		t.Fatalf("doctor reported not-ok for a condition it recovers from: %+v", report.Checks)
	}
	check := findCheck(t, report, "store-lock")
	if check.Status != statusWarn {
		t.Fatalf("store-lock check = %s (%s), want warn", check.Status, check.Detail)
	}
	for _, want := range []string{"orphaned ownership record", strconv.Itoa(deadPID)} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("store-lock check says %q, want it to mention %q", check.Detail, want)
		}
	}
}

// TestDoctorFailsAndExitsNonzeroOnASchemaNewerThanTheBinary covers the
// skew check of PRD §6.4 and the acceptance criterion attached to it: a
// failing check makes the command exit nonzero.
func TestDoctorFailsAndExitsNonzeroOnASchemaNewerThanTheBinary(t *testing.T) {
	base := scratchDir(t)
	runMemdolt(t, "init", "--dir", base)
	recordANewerSchemaVersion(t, base)

	out, err := runMemdoltResult(t, "doctor", "--dir", base, "--json")
	if err == nil {
		t.Fatalf("doctor succeeded against a store newer than the binary; output %q", out)
	}

	report := decodeDoctorReport(t, out)
	if report.OK {
		t.Fatalf("doctor reported ok while returning %v", err)
	}
	check := findCheck(t, report, "schema-version")
	if check.Status != statusFail {
		t.Fatalf("schema-version check = %s (%s), want fail", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "memdolt upgrade") {
		t.Fatalf("schema-version check says %q, want it to tell the operator to upgrade", check.Detail)
	}
}

// TestDoctorHumanOutputNamesEveryCheck covers the default, non-JSON
// rendering: one line per check, each carrying its verdict.
func TestDoctorHumanOutputNamesEveryCheck(t *testing.T) {
	out := runMemdolt(t, "doctor", "--dir", scratchDir(t))

	for _, want := range []string{
		"memdolt doctor:", "store-lock", "ipc", "schema-version", "empty-recall-rate",
		"mcp-registration-opencode", statusOK,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output %q does not mention %q", out, want)
		}
	}
}

func TestDoctorRecognizesOnlyParsedOpenCodeRegistrationPaths(t *testing.T) {
	t.Run("native repo JSONC", func(t *testing.T) {
		repo, home := t.TempDir(), t.TempDir()
		writeTestFile(t, filepath.Join(repo, "opencode.jsonc"), `{
  // native V2 registration
  "literal": "keep // and /* markers */",
  "mcp": {"servers": {/* comment */ "memdolt": {"command": ["memdolt", "serve"],},},},
}`)
		check := openCodeRegistrationCheckWithHome(repo, home)
		if check.Status != statusOK || !strings.Contains(check.Detail, "opencode.jsonc") {
			t.Fatalf("native JSONC check = %+v, want recognized repo registration", check)
		}
	})

	t.Run("supported V1 user JSONC", func(t *testing.T) {
		repo, home := t.TempDir(), t.TempDir()
		path := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
		writeTestFile(t, path, `{"mcp":{"memdolt":{"command":["memdolt","serve"],},},}`)
		check := openCodeRegistrationCheckWithHome(repo, home)
		if check.Status != statusOK || !strings.Contains(check.Detail, path) {
			t.Fatalf("V1 user check = %+v, want recognized user registration", check)
		}
	})

	t.Run("malformed and similarly named paths", func(t *testing.T) {
		repo, home := t.TempDir(), t.TempDir()
		writeTestFile(t, filepath.Join(repo, "opencode.json"),
			`{"mcp":{"memdolt":{}}} /* unterminated`)
		writeTestFile(t, filepath.Join(repo, "opencode.jsonc"),
			`{"mcpServers":{"memdolt":{}},"mcp":{"servers":{"memdolt-other":{}}}}`)
		check := openCodeRegistrationCheckWithHome(repo, home)
		if check.Status != statusWarn {
			t.Fatalf("unsupported configs check = %+v, want warn", check)
		}
	})

	t.Run("invalid trailing comma is not repaired", func(t *testing.T) {
		repo, home := t.TempDir(), t.TempDir()
		writeTestFile(t, filepath.Join(repo, "opencode.json"),
			`{"mcp":{"memdolt":{}},"malformed":[,]}`)
		if check := openCodeRegistrationCheckWithHome(repo, home); check.Status != statusWarn {
			t.Fatalf("malformed array check = %+v, want warn", check)
		}
	})
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// deadPID is above Linux's maximum pid_max and odd, which no Windows pid
// is, so no platform can have a process using it.
const deadPID = 2147483647

// ownershipRecord is exactly what a process killed mid-run leaves in a
// lock file: an ownership record whose advisory lock nobody holds any
// more. Its endpoint detail is what the pidfile carries; on the store's
// LOCK file it is ignored.
func ownershipRecord(t *testing.T) string {
	t.Helper()
	return `{"pid":` + strconv.Itoa(deadPID) + `,"host":"` + hostname(t) + `","id":"0123456789abcdef",` +
		`"acquiredAt":"` + time.Now().UTC().Format(time.RFC3339) + `","detail":{"port":1,"token":"t"}}`
}

// serveStore holds the store the way an MCP server does: the single-owner
// lock plus a loopback endpoint carrying store operations (PRD §5.2).
func serveStore(t *testing.T, base string) *ipc.Server {
	t.Helper()

	st, err := localdolt.New(localdolt.Config{
		BaseDir: base,
		Actor:   cliActor,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	configPath := pathsFor(t, base).ConfigFile()
	handler, err := storeipc.NewHandler(storeipc.Config{
		Store: st,
		ReviewAccept: func(ctx context.Context, id string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
			return reviewgate.Accept(ctx, st, configPath, id, reviewer, force)
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new storeipc handler: %v", err)
	}
	server, err := ipc.Listen(ipc.Config{
		BaseDir: base,
		Handler: handler,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// recordANewerSchemaVersion leaves behind exactly what a newer memdolt
// would: a schema version past everything this binary knows how to read.
func recordANewerSchemaVersion(t *testing.T, base string) {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: cliActor})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.Commit(context.Background(), store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "UPDATE " + store.MetaTable + " SET v = ? WHERE k = ?",
			Args: []any{strconv.Itoa(store.LatestSchemaVersion() + 1), store.SchemaVersionKey},
		}},
		NoText:  true,
		Message: "record a schema version a newer memdolt migrated to",
		Author:  cliActor,
	}); err != nil {
		t.Fatalf("record a newer schema version: %v", err)
	}
}

func runDoctorJSON(t *testing.T, base string) doctorReport {
	t.Helper()
	return decodeDoctorReport(t, runMemdolt(t, "doctor", "--dir", base, "--json"))
}

func decodeDoctorReport(t *testing.T, out string) doctorReport {
	t.Helper()
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("expected stdout to contain a single line, got %q", out)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		t.Fatalf("expected a single valid JSON object, got %q: %v", out, err)
	}
	return report
}

func findCheck(t *testing.T, report doctorReport, name string) doctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor reported no %q check, only %+v", name, report.Checks)
	return doctorCheck{}
}

func pathsFor(t *testing.T, base string) layout.Paths {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	return paths
}

func hostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	return host
}

// runMemdoltResult runs a command and hands back both its output and its
// error, which is what a command expected to exit nonzero needs: doctor
// prints its report either way.
func runMemdoltResult(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}
