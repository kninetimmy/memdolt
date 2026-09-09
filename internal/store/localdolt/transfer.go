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

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
)

// TransferOptions selects an existing configured remote. Passwords never cross
// this application/IPC boundary; the executing process supplies its environment.
type TransferOptions struct {
	Remote     string          `json:"remote"`
	User       string          `json:"user,omitempty"`
	Author     store.Actor     `json:"author,omitempty"`
	Resolution *PullResolution `json:"resolution,omitempty"`
}

// TransferResult preserves exact observed hashes, including confirmed promotion
// when a later step fails. RemoteCommit is the fetched or published main hash;
// LocalCommit is the captured local main, MainCommit the confirmed resulting main.
type TransferResult struct {
	Operation    string             `json:"operation"`
	Remote       string             `json:"remote"`
	LocalCommit  string             `json:"localCommit"`
	RemoteCommit string             `json:"remoteCommit"`
	MainCommit   string             `json:"mainCommit"`
	Changed      bool               `json:"changed"`
	Status       string             `json:"status"`
	MergeBase    string             `json:"mergeBase,omitempty"`
	Conflicts    []PullConflict     `json:"conflicts,omitempty"`
	Cleared      []ClearedViolation `json:"cleared,omitempty"`
	Remedy       string             `json:"remedy,omitempty"`
	Error        string             `json:"error,omitempty"`
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
	beforeCommit func() error
	finalize     func(*sql.Tx) error
	afterMove    func() error
}

func (s *Store) transfer(ctx context.Context, operation string, opts TransferOptions, hooks transferHooks) (result TransferResult, err error) {
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	result = TransferResult{Operation: operation, Remote: opts.Remote, Status: "refused"}
	if err := opts.ValidateText(); err != nil {
		return result, err
	}
	if operation == "push" && opts.Resolution != nil {
		return result, errors.New("conflict choices apply only to pull")
	}
	if opts.Resolution != nil && (!transferHash.MatchString(opts.Resolution.LocalCommit) || !transferHash.MatchString(opts.Resolution.RemoteCommit)) {
		return result, errors.New("resolution requires the exact displayed localCommit and remoteCommit hashes")
	}
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
	identity, err := s.checkProjectIdentity(ctx, conn, result.LocalCommit, true)
	if err != nil {
		return result, err
	}
	remoteURL, selectedUser, err := s.configuredTransferRemote(ctx, conn, opts)
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
		call, transferErr := runEngineTransfer(ctx, conn, operation, opts.Remote, remoteURL, user, result.LocalCommit, identity)
		if !call.completed {
			if !call.attempted {
				return result, transferErr
			}
			result.Status = "unknown"
			return result, fmt.Errorf("push did not confirm completion for captured main %s; remote outcome may be unknown; inspect remote main before retrying (non-fast-forward history must be reconciled manually): %w", result.LocalCommit, transferErr)
		}
		result.RemoteCommit = result.LocalCommit
		result.Changed = call.changed
		err = transferErr
	} else {
		if _, err := runEngineTransfer(ctx, conn, operation, opts.Remote, remoteURL, user, result.LocalCommit, identity); err != nil {
			return result, fmt.Errorf("fetch remote main; check remote main, connectivity and owner credentials: %w", err)
		}
		if err := conn.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", "remotes/"+opts.Remote+"/main").Scan(&result.RemoteCommit); err != nil {
			return result, fmt.Errorf("read fetched main: %w", err)
		}
		if !transferHash.MatchString(result.RemoteCommit) {
			return result, errors.New("fetched main is not an immutable Dolt commit hash")
		}
		incoming, err := readProjectIdentity(ctx, conn, result.RemoteCommit)
		if err != nil {
			return result, err
		}
		if err := matchProjectIdentity(identity, incoming); err != nil {
			return result, err
		}
		if opts.Resolution != nil && (opts.Resolution.LocalCommit != result.LocalCommit || opts.Resolution.RemoteCommit != result.RemoteCommit) {
			return result, errors.New("local or remote main changed since conflict review; run `memdolt pull --json` for a fresh review; no choices were applied")
		}
		if err := conn.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?)", result.LocalCommit, result.RemoteCommit).Scan(&result.MergeBase); err != nil {
			return result, fmt.Errorf("cannot establish fast-forward ancestry; inspect and reconcile histories manually: %w", err)
		}
		if result.MergeBase != result.RemoteCommit {
			if err := validateTransferSchema(ctx, conn, result.RemoteCommit); err != nil {
				return result, fmt.Errorf("incompatible incoming store; update memdolt or repair/migrate the remote with a compatible client before retrying: %w", err)
			}
			if err := s.scanTransfer(ctx, conn, result.LocalCommit, result.RemoteCommit); err != nil {
				return result, fmt.Errorf("refuse promotion: %w", err)
			}
			if result.MergeBase != result.LocalCommit {
				err := s.pullMerge(ctx, conn, opts, &result, hooks)
				return result, err
			}
			if opts.Resolution != nil {
				return result, errors.New("there is no divergent merge to resolve; run `memdolt pull` again")
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
		} else if opts.Resolution != nil {
			return result, errors.New("the remote is already contained; the reviewed merge is no longer pending")
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

func (s *Store) configuredTransferRemote(ctx context.Context, conn *sql.Conn, opts TransferOptions) (remoteURL, user string, err error) {
	cfg, err := s.repoConfig()
	if err != nil {
		return "", "", err
	}
	// Refresh native state using its protected reader before selecting a target.
	configured := engineRemotes{base: s.paths.Base()}
	if _, err := conn.ExecContext(context.WithValue(ctx, remoteContextKey{}, &configured), "CALL memdolt_remotes()"); err != nil {
		return "", "", err
	}
	var rawParams sql.NullString
	err = conn.QueryRowContext(ctx, "SELECT url, params FROM dolt_remotes WHERE name = ?", opts.Remote).Scan(&remoteURL, &rawParams)
	if errors.Is(err, sql.ErrNoRows) && opts.Remote == "origin" && cfg.RemoteURL != "" {
		remoteURL, err = cfg.RemoteURL, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("no remote: configure one with `memdolt repo remote add <name> <absolute-url> --dir <repository>`, or use `memdolt clone` in a fresh --dir")
	}
	if err != nil {
		return "", "", errors.New("cannot read selected remote configuration; inspect it with Dolt while the owner is stopped")
	}
	var params map[string]string
	if rawParams.Valid && json.Unmarshal([]byte(rawParams.String), &params) != nil {
		return "", "", errors.New("invalid remote parameters; configure only an optional SQL username")
	}
	remote, err := sanitizedRemote(opts.Remote, remoteURL, params)
	if err != nil {
		return "", "", err
	}
	remoteURL, user = remote.URL, remote.User
	if opts.Remote == "origin" && cfg.RemoteURL != "" && cfg.RemoteURL != remoteURL {
		return "", "", errors.New("native origin conflicts with [repo] remote_url; inspect both configurations and choose one intended destination; neither was contacted")
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
	if err := validateRemoteURL(remoteURL, user); err != nil {
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
