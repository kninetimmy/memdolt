package localdolt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"

	"github.com/kninetimmy/memdolt/internal/layout"
)

// TransferOptions selects an existing configured remote. Passwords never cross
// this application/IPC boundary; the executing process supplies its environment.
type TransferOptions struct {
	Remote string `json:"remote"`
	User   string `json:"user,omitempty"`
}

// TransferResult preserves exact observed hashes, including confirmed promotion
// when a later step fails. RemoteCommit is the fetched or published main hash;
// LocalCommit is the captured local main, MainCommit the confirmed resulting main.
type TransferResult struct {
	Operation    string `json:"operation"`
	Remote       string `json:"remote"`
	LocalCommit  string `json:"localCommit"`
	RemoteCommit string `json:"remoteCommit"`
	MainCommit   string `json:"mainCommit"`
	Changed      bool   `json:"changed"`
	Status       string `json:"status"`
}

var transferRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var transferHash = regexp.MustCompile(`^[0-9a-v]{32}$`)

// RequireExistingTransferStore checks before Open, whose established behavior
// creates missing databases. It never initializes or migrates anything.
func RequireExistingTransferStore(baseDir string) error {
	paths, err := layout.New(baseDir)
	if err != nil {
		return err
	}
	base, err := filepath.EvalSymlinks(paths.Base())
	if err != nil {
		return fmt.Errorf("no memdolt store; run `memdolt init --dir <repository>` first: %w", err)
	}
	paths, err = layout.New(base)
	if err != nil {
		return err
	}
	manifest := filepath.Join(paths.DoltDataDir(), DatabaseName, ".dolt", "noms", "manifest")
	if err := clonePath(base, manifest); err != nil {
		return err
	}
	if err := clonePath(base, paths.LockFile()); err != nil {
		return err
	}
	info, err := os.Stat(manifest)
	if os.IsNotExist(err) {
		return errors.New("no initialized memdolt store; run `memdolt init --dir <repository>` first")
	}
	if err != nil {
		return fmt.Errorf("inspect existing memdolt store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("invalid memdolt manifest; inspect the store before retrying")
	}
	return nil
}

func (s *Store) Push(ctx context.Context, opts TransferOptions) (TransferResult, error) {
	return s.transfer(ctx, "push", opts, transferHooks{})
}

func (s *Store) Pull(ctx context.Context, opts TransferOptions) (TransferResult, error) {
	return s.transfer(ctx, "pull", opts, transferHooks{})
}

type transferHooks struct {
	afterCapture func()
	beforeMove   func()
	afterMove    func() error
}

func (s *Store) transfer(ctx context.Context, operation string, opts TransferOptions, hooks transferHooks) (result TransferResult, err error) {
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	result = TransferResult{Operation: operation, Remote: opts.Remote, Status: "refused"}
	if !transferRemoteName.MatchString(opts.Remote) {
		return result, errors.New("invalid remote name; select one configured name using 1-64 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	// ponytail: serialize memdolt mutations through the network operation. Split
	// capture/fetch/promotion locks only if transfer latency becomes a problem.
	// This is one Store's boundary, not a lock on foreign Dolt processes.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	db, err := s.handle()
	if err != nil {
		return result, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, err
	}
	user := opts.User
	defer func() {
		err = errors.Join(err, conn.Close())
		if err != nil {
			if operation == "pull" {
				err = fmt.Errorf("pull: fetched objects and tracking refs may remain; inspect `memdolt repo status` and remote main before retrying: %w", err)
			}
			if result.Status == "changed" {
				err = fmt.Errorf("%s confirmed main %s (remote %s) before a later failure: %w", operation, result.MainCommit, result.RemoteCommit, err)
			}
			err = redactCloneError(err, user)
		}
	}()
	if err := requireTransferClean(ctx, conn); err != nil {
		return result, err
	}
	result.LocalCommit, err = branchHead(ctx, conn, MainBranch)
	if err != nil {
		return result, err
	}
	result.MainCommit = result.LocalCommit
	if err := validateTransferSchema(ctx, conn, result.LocalCommit); err != nil {
		return result, fmt.Errorf("unsupported local store; inspect or explicitly initialize/upgrade it before transfer: %w", err)
	}
	remoteURL, selectedUser, err := configuredTransferRemote(ctx, conn, opts)
	user = selectedUser
	if err != nil {
		return result, err
	}
	if err := validateTransferFile(remoteURL); err != nil {
		return result, err
	}
	if hooks.afterCapture != nil {
		hooks.afterCapture()
	}
	if operation == "push" {
		if err := s.scanTransfer(ctx, conn, "", result.LocalCommit); err != nil {
			return result, fmt.Errorf("refuse upload: %w", err)
		}
		call, transferErr := runEngineTransfer(ctx, conn, operation, opts.Remote, remoteURL, user, result.LocalCommit)
		if !call.completed {
			result.Status = "unknown"
			return result, fmt.Errorf("push did not confirm completion for captured main %s; remote outcome may be unknown; inspect remote main before retrying (non-fast-forward history must be reconciled manually): %w", result.LocalCommit, transferErr)
		}
		result.RemoteCommit = result.LocalCommit
		result.Changed = call.changed
		err = transferErr
	} else {
		if _, err := runEngineTransfer(ctx, conn, operation, opts.Remote, remoteURL, user, result.LocalCommit); err != nil {
			return result, fmt.Errorf("fetch remote main; check remote main, connectivity and owner credentials: %w", err)
		}
		if err := conn.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", "remotes/"+opts.Remote+"/main").Scan(&result.RemoteCommit); err != nil {
			return result, fmt.Errorf("read fetched main: %w", err)
		}
		if !transferHash.MatchString(result.RemoteCommit) {
			return result, errors.New("fetched main is not an immutable Dolt commit hash")
		}
		var base string
		if err := conn.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?)", result.LocalCommit, result.RemoteCommit).Scan(&base); err != nil {
			return result, fmt.Errorf("cannot establish fast-forward ancestry; inspect and reconcile histories manually: %w", err)
		}
		if base != result.RemoteCommit {
			if base != result.LocalCommit {
				return result, errors.New("divergent main histories; reconcile manually; automatic merge and conflict resolution are not supported")
			}
			if err := validateTransferSchema(ctx, conn, result.RemoteCommit); err != nil {
				return result, fmt.Errorf("incompatible incoming store; update memdolt or repair/migrate the remote with a compatible client before retrying: %w", err)
			}
			if err := s.scanTransfer(ctx, conn, result.LocalCommit, result.RemoteCommit); err != nil {
				return result, fmt.Errorf("refuse promotion: %w", err)
			}
			if hooks.beforeMove != nil {
				hooks.beforeMove()
			}
			if err := requireTransferClean(ctx, conn); err != nil {
				return result, err
			}
			current, err := branchHead(ctx, conn, MainBranch)
			if err != nil || current != result.LocalCommit {
				return result, errors.Join(errors.New("local main changed during pull; inspect it before retrying"), err)
			}
			var hash, message sql.NullString
			var ff, conflicts int
			if err := conn.QueryRowContext(ctx, "CALL DOLT_MERGE('--ff-only', ?)", result.RemoteCommit).Scan(&hash, &ff, &conflicts, &message); err != nil {
				result.Status = "unknown"
				return result, fmt.Errorf("pull promotion was not confirmed; inspect local main for candidate %s before retrying: %w", result.RemoteCommit, err)
			}
			result.MainCommit = result.RemoteCommit
			result.Changed = true
		}
	}
	result.Status = "current"
	if result.Changed {
		result.Status = "changed"
	}
	if hooks.afterMove != nil {
		err = errors.Join(err, hooks.afterMove())
	}
	return result, err
}

func requireTransferClean(ctx context.Context, conn *sql.Conn) error {
	active, err := activeBranch(ctx, conn)
	if err != nil || active != MainBranch {
		return errors.Join(errors.New("transfers require a session on main"), err)
	}
	var dirty, conflicts, violations int
	var merging bool
	for _, check := range []struct {
		query string
		dest  any
	}{
		{"SELECT COUNT(*) FROM dolt_status", &dirty},
		{"SELECT is_merging FROM dolt_merge_status", &merging},
		{"SELECT COUNT(*) FROM dolt_conflicts", &conflicts},
		{"SELECT COUNT(*) FROM dolt_constraint_violations", &violations},
	} {
		if err := conn.QueryRowContext(ctx, check.query).Scan(check.dest); err != nil {
			return fmt.Errorf("inspect main before transfer: %w", err)
		}
	}
	if dirty != 0 || merging || conflicts != 0 || violations != 0 {
		return errors.New("refusing transfer over a dirty main working set or active merge/conflict state; inspect `memdolt repo status` and finish or resolve local work first")
	}
	return nil
}

func configuredTransferRemote(ctx context.Context, conn *sql.Conn, opts TransferOptions) (remoteURL, user string, err error) {
	var rawParams sql.NullString
	err = conn.QueryRowContext(ctx, "SELECT url, params FROM dolt_remotes WHERE name = ?", opts.Remote).Scan(&remoteURL, &rawParams)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("no remote: stop the owner and configure the selected remote using Dolt in <repository>/.memdolt/dolt/memory (`dolt remote add <name> <absolute-url>`), or use `memdolt clone` in a fresh --dir")
	}
	if err != nil {
		return "", "", errors.New("cannot read selected remote configuration; inspect it with Dolt while the owner is stopped")
	}
	var params map[string]string
	if rawParams.Valid && json.Unmarshal([]byte(rawParams.String), &params) != nil {
		return "", "", errors.New("invalid remote parameters; configure only an optional SQL username")
	}
	for key, value := range params {
		if key != dbfactory.GRPCUsernameAuthParam || !cloneUser.MatchString(value) || strings.HasPrefix(value, "-") {
			return "", "", errors.New("unsupported remote parameters; configure only a valid SQL username, never a password or driver option")
		}
		user = value
	}
	if opts.User != "" {
		user = opts.User
	}
	if user != "" && (!cloneUser.MatchString(user) || strings.HasPrefix(user, "-")) {
		return "", "", errors.New("invalid --user; use 1-32 ASCII letters, digits, dots, underscores or hyphens without a leading hyphen")
	}
	if user != "" {
		if _, present := os.LookupEnv("DOLT_REMOTE_PASSWORD"); !present {
			return "", user, errors.New("set DOLT_REMOTE_PASSWORD in the transfer process; if an owner is running, restart the owner with DOLT_REMOTE_PASSWORD in its environment, then retry")
		}
	}
	// Dolt remote-add serializes Windows file:///C:/ paths as file://C:/.
	// Restore only that unambiguous native spelling before the clone validator.
	if filepath.Separator == '\\' && strings.HasPrefix(remoteURL, "file://") && len(remoteURL) > 9 && remoteURL[8:10] == ":/" &&
		((remoteURL[7] >= 'A' && remoteURL[7] <= 'Z') || (remoteURL[7] >= 'a' && remoteURL[7] <= 'z')) {
		remoteURL = "file:///" + remoteURL[7:]
	}
	// Native remote-add also stores decoded file-path spaces. Re-escape those
	// without accepting URL credentials, query/fragment, controls or ambiguity:
	// the unchanged clone validator still checks the canonical URL below.
	if parsed, parseErr := url.Parse(remoteURL); parseErr == nil && parsed.Scheme == "file" {
		remoteURL = parsed.String()
	}
	if err := validateCloneRemote(remoteURL, user); err != nil {
		return "", user, err
	}
	return remoteURL, user, nil
}

// Resolve the explicitly selected file remote and refuse managed symlinks.
// Pull reuses clone's source-preserving opener, including absent oldgen.
func validateTransferFile(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid transfer URL")
	}
	if u.Scheme != "file" {
		return nil
	}
	path, err := filepath.EvalSymlinks(cloneFilePath(u))
	if err != nil {
		return fmt.Errorf("resolve existing file remote; select an existing remote directory: %w", err)
	}
	// Also refuse linked table/lock files: a writable remote must not reach
	// outside the explicitly selected tree through an existing file link.
	return filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("file transfer refuses symbolic links inside the selected remote; use a self-contained remote")
		}
		return nil
	})
}
