//go:build soak

package soak

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// RigName identifies this rig in the summaries it emits.
const RigName = "m0-rig1"

// Platform records what the numbers were measured on. A soak result with
// no provenance is an anecdote.
type Platform struct {
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	NumCPU        int               `json:"numCpu"`
	GoVersion     string            `json:"goVersion"`
	Compiler      string            `json:"compiler"`
	Modules       map[string]string `json:"modules"`
	ModulesSource string            `json:"modulesSource"`
	BuildSettings map[string]string `json:"buildSettings"`
}

// doltModulePrefix selects the dependencies whose versions change what this
// rig measures. Every one of them is a Dolt module, and matching on the
// prefix rather than on a list means a rename cannot silently empty the
// provenance.
const doltModulePrefix = "github.com/dolthub/"

// interestingBuildSettings are the build settings that decide whether the
// binary under measurement is the one the project ships.
var interestingBuildSettings = []string{
	"-tags", "CGO_ENABLED", "GOOS", "GOARCH", "GOAMD64", "GOARM64",
	"-compiler", "vcs.revision", "vcs.time", "vcs.modified",
}

func describePlatform() Platform {
	p := Platform{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		NumCPU:        runtime.NumCPU(),
		GoVersion:     runtime.Version(),
		Compiler:      runtime.Compiler,
		Modules:       map[string]string{},
		BuildSettings: map[string]string{},
	}

	p.Modules, p.ModulesSource = moduleVersions()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return p
	}
	for _, setting := range info.Settings {
		for _, wanted := range interestingBuildSettings {
			if setting.Key == wanted {
				p.BuildSettings[setting.Key] = setting.Value
			}
		}
	}
	return p
}

// moduleVersions reads the dependency versions this rig was built against.
//
// It does not use debug.ReadBuildInfo, which reports the build settings
// above but no dependencies at all inside a `go test` binary — measured as
// len(Deps) == 0 on go1.26.5. Since `go test` runs a test binary with its
// working directory set to the package's source directory, the module file
// is a bounded walk up from there.
//
// A version this cannot find is reported as missing rather than left as an
// empty map, so that a summary never quietly claims to have no provenance
// when it merely failed to read it.
func moduleVersions() (map[string]string, string) {
	dir, err := os.Getwd()
	if err != nil {
		return map[string]string{}, "unavailable: " + err.Error()
	}
	for depth := 0; depth < 8; depth++ {
		path := filepath.Join(dir, "go.mod")
		raw, err := os.ReadFile(path)
		if err == nil {
			return parseDoltModules(string(raw)), path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return map[string]string{}, "unavailable: no go.mod above the test's working directory"
}

func parseDoltModules(gomod string) map[string]string {
	versions := map[string]string{}
	for _, line := range strings.Split(gomod, "\n") {
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], doltModulePrefix) || !strings.HasPrefix(fields[1], "v") {
			continue
		}
		versions[fields[0]] = fields[1]
	}
	return versions
}

// ScenarioConfig is the shape of the load, echoed into the summary so that
// a number can never be read without the configuration that produced it.
type ScenarioConfig struct {
	Duration        time.Duration `json:"duration"`
	KillAfter       time.Duration `json:"killAfter"`
	OwnerWriters    int           `json:"ownerWriters"`
	OwnerReaders    int           `json:"ownerReaders"`
	ClientProcesses int           `json:"clientProcesses"`
	ClientWriters   int           `json:"clientWritersEach"`
	ClientReaders   int           `json:"clientReadersEach"`
	WriteInterval   time.Duration `json:"writeInterval"`
	ReadInterval    time.Duration `json:"readInterval"`
	OpTimeout       time.Duration `json:"opTimeout"`
}

// WriteAccounting reconciles the ledger against the re-opened store.
type WriteAccounting struct {
	Attempted     int `json:"attempted"`
	Committed     int `json:"committed"`
	Refused       int `json:"refused"`
	Indeterminate int `json:"indeterminate"`
	NoResult      int `json:"noResultRecorded"`

	Lost      int `json:"lost"`
	Corrupted int `json:"corrupted"`

	Phantom              int `json:"phantomPresentAfterRefusal"`
	IndeterminatePresent int `json:"indeterminatePresent"`
	IndeterminateAbsent  int `json:"indeterminateAbsent"`
	NoResultPresent      int `json:"noResultPresent"`
	NoResultAbsent       int `json:"noResultAbsent"`
	Unledgered           int `json:"unledgeredRowsInStore"`

	LostKeys       []string `json:"lostKeys,omitempty"`
	CorruptedKeys  []string `json:"corruptedKeys,omitempty"`
	PhantomKeys    []string `json:"phantomKeys,omitempty"`
	UnledgeredKeys []string `json:"unledgeredKeys,omitempty"`
	TornLedgers    []string `json:"tornLedgerTails,omitempty"`
}

// ReadAccounting is what the concurrent readers saw.
type ReadAccounting struct {
	Attempted           int      `json:"attempted"`
	Failed              int      `json:"failed"`
	MissingCommittedRow int      `json:"missingCommittedRow"`
	MismatchedPayload   int      `json:"mismatchedPayload"`
	RowCountRegressions int      `json:"rowCountRegressions"`
	MissingKeySamples   []string `json:"missingKeySamples,omitempty"`
}

// StoreObservations are facts read out of the store after the run.
type StoreObservations struct {
	RowsInProbeTable   int64          `json:"rowsInProbeTable"`
	DoltCommits        int64          `json:"doltCommits"`
	CommitsByCommitter map[string]int `json:"doltCommitsByCommitter"`
	DataDirBytes       int64          `json:"dataDirBytes"`
	LockStateAfterRun  string         `json:"lockStateAfterRun"`
	PidStateAfterRun   string         `json:"pidfileStateAfterRun"`
	ProbeAfterRun      string         `json:"ipcProbeAfterRun"`
}

// RecoveryOutcome is the unclean-kill half of the rig: what the store's
// lock looked like after an owner was killed without cleanup, and whether a
// later open recovered it and could still read everything the dead owner
// had acknowledged.
type RecoveryOutcome struct {
	Performed bool   `json:"performed"`
	Trigger   string `json:"trigger,omitempty"`
	OwnerPID  int    `json:"ownerPid,omitempty"`

	ProbeBeforeKill string `json:"ipcProbeBeforeKill,omitempty"`

	LockStateAfterKill    string `json:"lockStateAfterKill,omitempty"`
	PidfileStateAfterKill string `json:"pidfileStateAfterKill,omitempty"`
	ProbeAfterKill        string `json:"ipcProbeAfterKill,omitempty"`
	StaleRecordPID        int    `json:"stalePidInLockRecord,omitempty"`

	// The pid is sampled twice: once while the parent still holds a handle
	// to the killed process, and once after it has been reaped. The pair is
	// the evidence behind §5.2.3's [verify] marker — what a pid check would
	// have concluded at each moment, next to what the lock concluded.
	OwnerPIDLivenessBeforeReap string `json:"ownerPidLivenessBeforeReap,omitempty"`
	OwnerPIDLivenessAfterReap  string `json:"ownerPidLivenessAfterReap,omitempty"`
	OwnerPIDLivenessError      string `json:"ownerPidLivenessError,omitempty"`

	ReopenSucceeded     bool   `json:"reopenSucceeded"`
	LoudWarningLogged   bool   `json:"loudWarningLogged"`
	WarningNamedStale   bool   `json:"warningNamedStalePid"`
	WarningLine         string `json:"warningLine,omitempty"`
	StaleRecordCleared  bool   `json:"staleRecordClearedAfterReopen"`
	PidfileRecovered    bool   `json:"pidfileRecoveredOnNextListen"`
	PidfileWarningLine  string `json:"pidfileWarningLine,omitempty"`
	RecycledPIDProbe    string `json:"recycledPidProbe,omitempty"`
	RecycledPIDRecovery bool   `json:"recycledPidRecovered"`

	CommittedBeforeKill  int      `json:"committedBeforeKill"`
	PresentAfterRecovery int      `json:"presentAfterRecovery"`
	MissingAfterRecovery int      `json:"missingAfterRecovery"`
	MissingKeys          []string `json:"missingKeys,omitempty"`

	Outcome string `json:"outcome"`
}

// ForeignOpener records what a process that does not take memdolt's lock
// finds when it opens a data directory a live owner already holds.
//
// It is measured rather than assumed because PRD §5.2 and risk R1 both
// expect a loud "database is locked", and the driver does something else
// entirely. A second memdolt process is turned away by memdolt's own lock
// before the driver is involved; a foreign one — a `dolt` CLI, a
// `dolt sql-server`, anything that does not know about the lock — is not,
// and what happens to it is the residual risk the single-owner rule cannot
// remove.
type ForeignOpener struct {
	SecondMemdoltOpenError    string `json:"secondMemdoltOpenError"`
	SecondMemdoltMatchedError bool   `json:"secondMemdoltOpenMatchedErrLocked"`
	SecondMemdoltOpenMillis   int64  `json:"secondMemdoltOpenMillis"`

	DriverOpenError   string `json:"driverOpenError"`
	DriverPingError   string `json:"driverPingError"`
	DriverReadError   string `json:"driverReadError"`
	DriverReadRows    int64  `json:"driverReadRows"`
	DriverWriteError  string `json:"driverWriteError"`
	DriverCommitError string `json:"driverCommitError"`

	// StepOutcomes names each step's error class, or "ok" where the step
	// succeeded. It is the shape of the finding: which step is the one that
	// finally tells a foreign writer it is not writing.
	StepOutcomes map[string]string `json:"stepOutcomes"`

	OwnerStillCommits bool   `json:"ownerStillCommitsAfterwards"`
	OwnerCommitError  string `json:"ownerCommitError,omitempty"`
}

// Summary is the rig's machine-readable result.
type Summary struct {
	Rig        string    `json:"rig"`
	Scenario   string    `json:"scenario"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	Platform Platform          `json:"platform"`
	Config   ScenarioConfig    `json:"config"`
	Writes   WriteAccounting   `json:"writes"`
	Reads    ReadAccounting    `json:"reads"`
	Store    StoreObservations `json:"store"`
	Errors   *ErrorTally       `json:"errorsByClass"`
	Recovery RecoveryOutcome   `json:"recovery"`
	Foreign  *ForeignOpener    `json:"foreignOpener,omitempty"`
	Roles    []*RoleStats      `json:"roles"`

	DataLossEvents int      `json:"dataLossEvents"`
	Verdict        string   `json:"verdict"`
	VerdictReasons []string `json:"verdictReasons"`
	Findings       []string `json:"findings"`
}

// Verdicts.
const (
	VerdictPass = "PASS"
	VerdictFail = "FAIL"
)

func newSummary(scenario string, cfg ScenarioConfig) *Summary {
	return &Summary{
		Rig:       RigName,
		Scenario:  scenario,
		StartedAt: time.Now().UTC(),
		Platform:  describePlatform(),
		Config:    cfg,
		Errors:    newErrorTally(),
		Store:     StoreObservations{CommitsByCommitter: map[string]int{}},
	}
}

// decide fills in the verdict.
//
// PRD §16's rig-1 gate is "zero data-loss events in the concurrency soak".
// This is what this rig counts as one. The list was fixed before any
// measurement was taken and is not adjusted afterwards; that is the whole
// point of writing it down here rather than in prose after the fact.
//
//  1. A write the store acknowledged that is absent when the store is
//     re-opened.
//  2. A write the store acknowledged whose stored value is not the value
//     that was written.
//  3. A write acknowledged before an unclean kill that is absent after
//     stale-lock recovery.
//  4. A committed write that a concurrent reader could not see, or saw with
//     the wrong value. The writer had already been told it was committed,
//     so this is memory the product would have failed to recall.
//  5. A row in the store that no ledger intent accounts for. That loses
//     nothing by itself, but it means the four counts above cannot be
//     trusted, and a rig that cannot vouch for its own accounting must not
//     report a pass.
//
// Two things are deliberately *not* counted as data loss, and are reported
// as findings instead:
//
//   - A write reported as failed that is present anyway. The store gained
//     data rather than losing it. It is still a defect — a caller told "no"
//     will retry — so it is named in the findings and in the write-up.
//   - A write whose answer never arrived. Nothing promised the writer
//     anything, so scoring its presence either way would be inventing a
//     promise. Presence and absence are both reported.
func (s *Summary) decide() {
	s.FinishedAt = time.Now().UTC()

	events := 0
	var reasons []string

	if s.Writes.Lost > 0 {
		events += s.Writes.Lost
		reasons = append(reasons, fmt.Sprintf(
			"%d acknowledged write(s) are absent from the re-opened store", s.Writes.Lost))
	}
	if s.Writes.Corrupted > 0 {
		events += s.Writes.Corrupted
		reasons = append(reasons, fmt.Sprintf(
			"%d acknowledged write(s) came back with a different value", s.Writes.Corrupted))
	}
	if s.Reads.MissingCommittedRow > 0 {
		events += s.Reads.MissingCommittedRow
		reasons = append(reasons, fmt.Sprintf(
			"%d read(s) could not see a write the store had already acknowledged", s.Reads.MissingCommittedRow))
	}
	if s.Reads.MismatchedPayload > 0 {
		events += s.Reads.MismatchedPayload
		reasons = append(reasons, fmt.Sprintf(
			"%d read(s) returned a different value than was written", s.Reads.MismatchedPayload))
	}
	if s.Writes.Unledgered > 0 {
		events += s.Writes.Unledgered
		reasons = append(reasons, fmt.Sprintf(
			"%d row(s) in the store are accounted for by no ledger intent, so the accounting cannot be trusted",
			s.Writes.Unledgered))
	}
	if s.Recovery.Performed {
		if s.Recovery.MissingAfterRecovery > 0 {
			events += s.Recovery.MissingAfterRecovery
			reasons = append(reasons, fmt.Sprintf(
				"%d write(s) acknowledged before the unclean kill were unreadable after recovery",
				s.Recovery.MissingAfterRecovery))
		}
		if s.Recovery.Outcome != "recovered" {
			reasons = append(reasons, "stale-lock recovery after the unclean kill did not complete: "+s.Recovery.Outcome)
		}
	}

	s.DataLossEvents = events
	s.VerdictReasons = reasons
	if len(reasons) == 0 {
		s.Verdict = VerdictPass
		s.VerdictReasons = []string{"zero data-loss events under the definition recorded in tests/soak/summary.go"}
		return
	}
	s.Verdict = VerdictFail
}

func (s *Summary) addFinding(format string, args ...any) {
	s.Findings = append(s.Findings, fmt.Sprintf(format, args...))
}

// Emit writes the summary where a human running the rig will see it.
func (s *Summary) Emit(w io.Writer) error {
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode summary: %w", err)
	}
	_, err = fmt.Fprintf(w, "\n=== SOAK SUMMARY BEGIN %s/%s ===\n%s\n=== SOAK SUMMARY END %s/%s ===\n",
		s.Rig, s.Scenario, encoded, s.Rig, s.Scenario)
	return err
}

// WriteFile saves the summary next to the others from the same run.
func (s *Summary) WriteFile(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create summary directory %s: %w", dir, err)
	}
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode summary: %w", err)
	}
	path := filepath.Join(dir, s.Rig+"-"+s.Scenario+".json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write summary %s: %w", path, err)
	}
	return path, nil
}
