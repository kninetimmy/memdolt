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

	"github.com/dolthub/dolt/go/cmd/dolt/doltversion"
	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/hub"
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
// Explicit remote diagnostics also warn about unobserved native release,
// absent identity or unresolved divergence; none claims full compatibility.
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
	Dir    string                      `json:"dir,omitempty"`
	OK     bool                        `json:"ok"`
	Checks []doctorCheck               `json:"checks"`
	Hub    *hub.Report                 `json:"hub,omitempty"`
	Remote *localdolt.RepoStatusReport `json:"remote,omitempty"`
}

func newDoctorCommand() *cobra.Command {
	var flags storeFlags
	var selectedHub bool
	var config string
	var remote localdolt.RepoStatusOptions

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report local health and explicitly selected hub or remote compatibility",
		Long: "Run the store-health checks of PRD §5.2 and §6.4 plus the empty-recall\n" +
			"observability check of PRD §8.1 against this repository:\n\n" +
			"  embedded-dolt-version  pinned Dolt release versus the measured baseline\n" +
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
			"OpenCode registration are warnings and exit zero. Ordinary doctor is offline\n" +
			"apart from local owner IPC; it never contacts a configured remote.\n\n" +
			"--hub --config <absolute-hub.json> runs existing hub status inspection on\n" +
			"the selected Linux deployment, without opening repository memory. It needs\n" +
			"permission to inspect nftables and deployment files. This observes the\n" +
			"configured native executable, private boundary, addresses and listeners;\n" +
			"it does not install/start anything or prove listener identity or SQL grants.\n\n" +
			"--remote <name> adds existing repo status inspection through the direct or\n" +
			"authenticated owner route. It fetches committed remote main and checks\n" +
			"schema/identity compatibility, preserving main, proposals, tags, working\n" +
			"memory, indexes and config. Fetched objects/tracking refs may remain even\n" +
			"on refusal; a lost reply is never replayed. --user overrides the stored\n" +
			"SQL username. Only the executing owner's DOLT_REMOTE_PASSWORD supplies a\n" +
			"password; restart that owner to change it. Passwords never cross IPC.\n\n" +
			"Remote executable release is unobserved: storage metadata and successful\n" +
			"fetches do not establish it. Run doctor --hub --config on the hub for that\n" +
			"evidence. The measured baseline is exactly Dolt 1.88.1, not a compatible\n" +
			"range. Physical two-client acceptance and backup/disk checks remain separate.\n" +
			"--hub cannot combine with --dir, --remote or --user; --config needs --hub.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			switch {
			case selectedHub && (cmd.Flags().Changed("dir") || cmd.Flags().Changed("remote") || cmd.Flags().Changed("user")):
				err = errors.New("--hub cannot be combined with --dir, --remote or --user")
			case selectedHub && config == "":
				err = errors.New("--hub requires --config <absolute-hub.json>")
			case !selectedHub && cmd.Flags().Changed("config"):
				err = errors.New("--config requires --hub")
			case cmd.Flags().Changed("remote") && remote.Remote == "":
				err = errors.New("--remote must name one configured remote")
			case cmd.Flags().Changed("user") && (remote.Remote == "" || remote.User == ""):
				err = errors.New("--user must not be empty and requires --remote <name>")
			default:
				err = remote.Validate()
			}
			if err != nil {
				return finishDoctorReport(cmd, doctorReport{Checks: []doctorCheck{newCheck("selection", statusFail, "%v", err)}})
			}
			if selectedHub {
				return runDoctorHub(cmd, config)
			}
			return runDoctor(cmd, flags, remote)
		},
	}

	cmd.Flags().BoolVar(&selectedHub, "hub", false, "inspect an explicitly selected managed Linux hub instead of repository memory")
	cmd.Flags().StringVar(&config, "config", "", "absolute path to the selected generated hub.json; requires --hub")
	cmd.Flags().StringVar(&remote.Remote, "remote", "", "inspect this configured repository remote; permits a fetch")
	cmd.Flags().StringVar(&remote.User, "user", "", "SQL username override for --remote; password comes only from the owner's environment")
	return flags.bind(cmd)
}

func runDoctor(cmd *cobra.Command, flags storeFlags, remote localdolt.RepoStatusOptions) error {
	paths, err := layout.New(flags.dir)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	report := doctorReport{Dir: paths.Dir(), Checks: []doctorCheck{embeddedDoltCheck(doltversion.Version)}}
	if remote.Remote != "" {
		// Guard before schemaCheck too: Open can create a missing database in
		// a partial store. Explicit remote inspection must never initialize it.
		if err := localdolt.RequireExistingTransferStore(flags.dir); err != nil {
			report.Checks = append(report.Checks, newCheck("remote-compatibility", statusFail, "%v", err), remoteDoltCheck())
			return finishDoctorReport(cmd, report)
		}
	}

	// The IPC check runs before the schema check because its answer
	// decides how the schema check may read the store: a live owner holds
	// the data directory, and PRD §5.2.1 routes every other process
	// through it rather than around it.
	owner, ownerLive := ownerCheck(ctx, paths)
	report.Checks = append(report.Checks, lockCheck(paths), owner,
		schemaCheck(ctx, paths, ownerLive), emptyRecallCheck(ctx, paths), openCodeRegistrationCheck(paths.Base()))
	if remote.Remote != "" {
		st, err := flags.open(ctx, cliActor)
		check := newCheck("remote-compatibility", statusFail, "%v", err)
		if err == nil {
			check, report.Remote = doctorRemoteCheck(ctx, st, remote)
		}
		report.Checks = append(report.Checks, check, remoteDoltCheck())
	}
	return finishDoctorReport(cmd, report)
}

func embeddedDoltCheck(version string) doctorCheck {
	const name = "embedded-dolt-version"
	if version == "" {
		return newCheck(name, statusFail, "embedded Dolt release unobservable; rebuild from the pinned dependency; measured baseline is exactly %s", hub.Version)
	}
	if version != hub.Version {
		return newCheck(name, statusFail, "unsupported embedded Dolt release %s; measured baseline is exactly %s; use the measured pinned build", version, hub.Version)
	}
	return newCheck(name, statusOK, "embedded Dolt release %s from pinned dolt/go doltversion.Version; measured baseline is exactly %s", version, hub.Version)
}

func runDoctorHub(cmd *cobra.Command, config string) error {
	observed, err := hub.Inspect(cmd.Context(), config, "status", false)
	report := doctorReport{Hub: &observed, Checks: []doctorCheck{embeddedDoltCheck(doltversion.Version)}}
	for _, check := range observed.Checks {
		report.Checks = append(report.Checks, newCheck("hub-"+check.Name, check.Status, "%s", check.Detail))
	}
	return errors.Join(err, finishDoctorReport(cmd, report))
}

// doctorRemoteCheck owns the opened store and retains observations on a late
// failure. RepoStatus alone determines which committed evidence was validated;
// an authenticated owner refusal may provide no partial report at all.
func doctorRemoteCheck(ctx context.Context, st commandStore, opts localdolt.RepoStatusOptions) (doctorCheck, *localdolt.RepoStatusReport) {
	const name = "remote-compatibility"
	report, err := st.RepoStatus(ctx, opts)
	if err = errors.Join(err, st.Close()); err != nil {
		if report.Store == "" {
			return newCheck(name, statusFail, "%v", err), nil
		}
		return newCheck(name, statusFail, "%v; captured main: local %q, remote %q (empty means unobserved)",
			err, report.MainCommit, report.RemoteCommit), &report
	}
	status := statusOK
	identity := "matching committed project identity " + report.ProjectID
	if report.ProjectID == "" {
		status = statusWarn
		identity = "committed project identity absent on both sides; review Git origin and explicit init --adopt-identity"
	}
	if report.Status == "conflicted" || report.Status == "diverged-unassessed" {
		status = statusWarn
	}
	return newCheck(name, status,
		"remote %s reachable; committed main %s; schema v%d compatible with local main %s; %s; status %s. %s",
		opts.Remote, report.RemoteCommit, report.SchemaVersion, report.MainCommit, identity, report.Status, report.Remedy), &report
}

func remoteDoltCheck() doctorCheck {
	return newCheck("remote-dolt-version", statusWarn,
		"remote executable release unobserved: remotes metadata describes storage formats, not the native release. On the hub run `memdolt doctor --hub --config <absolute-hub.json>` with permission to inspect deployment files and nftables; measured baseline is exactly %s", hub.Version)
}

func finishDoctorReport(cmd *cobra.Command, report doctorReport) error {
	failed := 0
	for _, check := range report.Checks {
		if check.Status == statusFail {
			failed++
		}
	}
	report.OK = failed == 0

	var err error
	if failed > 0 {
		err = fmt.Errorf("doctor: %d of %d checks failed", failed, len(report.Checks))
	}
	return errors.Join(err, writeDoctorReport(cmd, report))
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

	target := report.Dir
	if report.Hub != nil {
		target = "hub " + report.Hub.Config
	}
	if _, err := fmt.Fprintf(out, "memdolt doctor: %s\n", target); err != nil {
		return fmt.Errorf("write doctor line: %w", err)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "  %-4s  %-14s  %s\n", check.Status, check.Name, check.Detail); err != nil {
			return fmt.Errorf("write doctor line: %w", err)
		}
	}
	return nil
}
