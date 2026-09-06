package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// Before the host-registration check, warn meant only a condition memdolt
// repairs itself. After it, warn also carries an actionable integration
// advisory that does not make the repository unsafe to use. Fail remains a
// condition that prevents safe operation. Stale ownership records retain
// their existing warning and zero-exit behavior (PRD §5.2.3).
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
		Short: "Report store, retrieval, and host-registration health",
		Long: "Run the store-health checks of PRD §5.2 and §6.4 plus the empty-recall\n" +
			"observability check of PRD §8.1 against this repository:\n\n" +
			"  store-lock      who, if anyone, owns the store — held, an orphaned\n" +
			"                  ownership record, or absent\n" +
			"  ipc             whether a live owner answers on its loopback endpoint\n" +
			"  schema-version  whether the store's schema is newer than this binary\n" +
			"  empty-recall-rate  local empty-above-floor recall count and rate\n" +
			"  mcp-registration-opencode  parsed repo or user OpenCode registration\n\n" +
			"Against a directory with no store, doctor reports that rather than initializing\n" +
			"one or creating its directory. With no live owner, it opens an existing store\n" +
			"directly to read its schema; this briefly takes the ownership lock and may\n" +
			"create .memdolt/LOCK, but makes no durable database change. It exits\n" +
			"nonzero when a check fails. Stale ownership records and a missing optional\n" +
			"OpenCode registration are warnings and exit zero.",
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
			openCodeRegistrationCheck(paths.Base()),
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

func openCodeRegistrationCheck(repoRoot string) doctorCheck {
	home, _ := os.UserHomeDir()
	return openCodeRegistrationCheckWithHome(repoRoot, home)
}

// openCodeRegistrationCheckWithHome recognizes only PRD §11.4's parsed
// native V2 mcp.servers.memdolt and supported V1 mcp.memdolt paths. The home
// argument is injected so tests never depend on a developer's real config.
func openCodeRegistrationCheckWithHome(repoRoot, home string) doctorCheck {
	const name = "mcp-registration-opencode"
	candidates := []string{
		filepath.Join(repoRoot, "opencode.json"),
		filepath.Join(repoRoot, "opencode.jsonc"),
	}
	if home != "" {
		userDir := filepath.Join(home, ".config", "opencode")
		candidates = append(candidates,
			filepath.Join(userDir, "opencode.json"),
			filepath.Join(userDir, "opencode.jsonc"),
		)
	}
	for _, path := range candidates {
		if openCodeConfigRegistersMemdolt(path) {
			return newCheck(name, statusOK, "memdolt MCP server registered in %s", path)
		}
	}
	return newCheck(name, statusWarn,
		"no parsed OpenCode registration at mcp.servers.memdolt or mcp.memdolt (checked repo and user config)")
}

func openCodeConfigRegistersMemdolt(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var config map[string]json.RawMessage
	clean := stripJSONCTrailingCommas(stripJSONCComments(raw))
	if err := json.Unmarshal(clean, &config); err != nil {
		return false
	}
	// Maps keep every path segment case-sensitive; struct decoding would also
	// accept an unsupported root such as "MCP" or "Mcp".
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(config["mcp"], &mcp); err != nil || mcp == nil {
		return false
	}

	if rawServers, ok := mcp["servers"]; ok {
		var servers map[string]json.RawMessage
		if json.Unmarshal(rawServers, &servers) == nil && jsonObject(servers["memdolt"]) {
			return true
		}
	}
	return jsonObject(mcp["memdolt"])
}

func jsonObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// stripJSONCComments removes line and block comments while preserving comment
// markers and escapes inside JSON strings. An unterminated block comment is
// retained so the subsequent real JSON parse fails closed.
func stripJSONCComments(raw []byte) []byte {
	clean := make([]byte, 0, len(raw))
	start := 0
	inString, escaped := false, false
	for i := 0; i < len(raw); {
		if inString {
			switch {
			case escaped:
				escaped = false
			case raw[i] == '\\':
				escaped = true
			case raw[i] == '"':
				inString = false
			}
			i++
			continue
		}

		switch {
		case raw[i] == '"':
			inString = true
			i++
		case i+1 < len(raw) && raw[i] == '/' && raw[i+1] == '/':
			clean = append(clean, raw[start:i]...)
			i += 2
			for i < len(raw) && raw[i] != '\r' && raw[i] != '\n' {
				i++
			}
			start = i
		case i+1 < len(raw) && raw[i] == '/' && raw[i+1] == '*':
			commentStart := i
			clean = append(clean, raw[start:i]...)
			i += 2
			for i+1 < len(raw) && (raw[i] != '*' || raw[i+1] != '/') {
				i++
			}
			if i+1 >= len(raw) {
				clean = append(clean, raw[commentStart:]...)
				return clean
			}
			clean = append(clean, ' ')
			i += 2
			start = i
		default:
			i++
		}
	}
	return append(clean, raw[start:]...)
}

// stripJSONCTrailingCommas removes a comma before ] or } only when the comma
// follows a token that can end a JSON value. It therefore accepts JSONC's
// trailing commas without repairing malformed shapes such as "[, ]".
func stripJSONCTrailingCommas(raw []byte) []byte {
	clean := make([]byte, 0, len(raw))
	start := 0
	inString, escaped := false, false
	for i, b := range raw {
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b != ',' || !previousJSONValue(raw[:i]) || !nextJSONCloser(raw[i+1:]) {
			continue
		}
		clean = append(clean, raw[start:i]...)
		start = i + 1
	}
	return append(clean, raw[start:]...)
}

func previousJSONValue(raw []byte) bool {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n' {
			continue
		}
		b := raw[i]
		return b == '}' || b == ']' || b == '"' || (b >= '0' && b <= '9') || b == 'e' || b == 'l'
	}
	return false
}

func nextJSONCloser(raw []byte) bool {
	for _, b := range raw {
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		return b == '}' || b == ']'
	}
	return false
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
		client, err := storeipc.DialOwnerStore(paths.Base())
		if err != nil {
			return 0, fmt.Errorf("reach the store's owner: %w", err)
		}
		return client.SchemaVersion(ctx)
	}

	// Before owner routing was complete, doctor was the exception: it built
	// two raw IPC queries by hand while every other schema reader opened
	// LocalStore. After, all schema readers share OwnerStore.SchemaVersion;
	// the direct branch below remains the no-live-owner behavior.
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
