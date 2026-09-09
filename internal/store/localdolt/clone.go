package localdolt

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/table"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/types"
	gmssql "github.com/dolthub/go-mysql-server/sql"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
)

// CloneResult is emitted only after the transfer, validation and all closes succeed.
type CloneResult struct {
	ProjectIdentity
	Store         string `json:"store"`
	MainCommit    string `json:"mainCommit"`
	SchemaVersion int    `json:"schemaVersion"`
}

var cloneUser = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// ponytail: Dolt's progress writer is process-global; serialize clone calls until
// the pinned engine supports a per-transfer writer. Other Store paths do not use it.
var cloneMu sync.Mutex

// Clone acquires main without calling Store.Open (which creates absent databases)
// or Migrate. It holds the ordinary ownership lock throughout. Failed artifacts
// are retained: DOLT_CLONE's SQL wrapper recursively deletes its destination on
// error, so use its same pinned CloneRemote engine with cleanup disabled instead.
func Clone(ctx context.Context, cfg Config, remoteURL, user string) (result CloneResult, err error) {
	return clone(ctx, cfg, remoteURL, user, (*singleowner.Lock).Release)
}

func clone(ctx context.Context, cfg Config, remoteURL, user string, release func(*singleowner.Lock) error) (result CloneResult, err error) {
	policy := RepoConfig{}
	if !cfg.Global {
		policy, err = ReadRepoConfig(cfg.BaseDir)
		if err != nil {
			return result, err
		}
	}
	if remoteURL == "" {
		remoteURL = policy.RemoteURL
	} else if policy.RemoteURL != "" && policy.RemoteURL != remoteURL {
		return result, errors.New("clone URL conflicts with [repo] remote_url; inspect configuration and choose one intended destination")
	}
	if err := validateCloneRemote(remoteURL, user); err != nil {
		return result, err
	}
	st, err := New(cfg)
	if err != nil {
		return result, err
	}
	if err := validateCloneDestination(st.DataDir(), cfg.Actor); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("clone canceled before transfer: %w", err)
	}
	// Resolve the selected repository, then refuse links in the managed paths.
	if err := os.MkdirAll(st.paths.Base(), 0o755); err != nil {
		return result, fmt.Errorf("create clone repository: %w", err)
	}
	base, err := filepath.EvalSymlinks(st.paths.Base())
	if err != nil {
		return result, fmt.Errorf("resolve clone repository: %w", err)
	}
	st.paths, err = layout.New(base)
	if err != nil {
		return result, err
	}
	expected, err := st.resolvedIdentity(ctx)
	if err != nil {
		return result, err
	}
	if err := validateCloneDestination(st.DataDir(), cfg.Actor); err != nil {
		return result, err
	}
	for _, path := range []string{st.paths.Dir(), st.paths.LockFile(), st.DataDir()} {
		if err := clonePath(base, path); err != nil {
			return result, err
		}
	}
	if err := os.MkdirAll(st.paths.Dir(), 0o700); err != nil {
		return result, fmt.Errorf("create clone state directory: %w", err)
	}
	lock, err := singleowner.Acquire(st.LockFile(), singleowner.Options{Logger: st.logger})
	if err != nil {
		return result, fmt.Errorf("clone requires exclusive ownership; stop the store owner and use an empty destination: %w", err)
	}
	defer func() {
		if closeErr := release(lock); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release clone ownership: %w", closeErr))
		}
		if err != nil {
			err = fmt.Errorf("clone did not complete; artifacts retained at %s; inspect them before retrying in a fresh --dir: %w", st.DataDir(), err)
			err = redactCloneError(err, user)
		}
	}()
	entries, err := os.ReadDir(st.DataDir())
	if err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("inspect clone destination: %w", err)
	}
	if len(entries) != 0 {
		return result, fmt.Errorf("clone destination %s is not empty; choose a fresh repository with --dir", st.DataDir())
	}
	if err := os.MkdirAll(st.DataDir(), 0o700); err != nil {
		return result, fmt.Errorf("create clone data directory: %w", err)
	}
	cloneMu.Lock()
	defer cloneMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("clone canceled before transfer: %w", err)
	}
	oldOut, oldErr := cli.CliOut, cli.CliErr
	cli.CliOut, cli.CliErr = io.Discard, io.Discard
	defer func() { cli.CliOut, cli.CliErr = oldOut, oldErr }()
	var required *ProjectIdentity
	if expected.ProjectID != "" || cfg.Global {
		required = &expected
	}
	result, err = cloneTransfer(ctx, st.DataDir(), remoteURL, user, required)
	if err == nil && (expected.ProjectID != "" || cfg.Global) {
		err = matchProjectIdentity(expected, result.ProjectIdentity)
	}
	return result, err
}

func validateCloneDestination(dataDir string, actor store.Actor) error {
	// Both the supplied path and its resolved target must remain addressable
	// by ordinary embedded opens and by Dolt's file-URL loader.
	if _, err := buildDSN(dataDir, actor, DatabaseName); err != nil {
		return err
	}
	if strings.Contains(dataDir, "%") {
		return errors.New("clone destination contains '%', which the pinned Dolt file loader can decode twice; choose another --dir")
	}
	return nil
}

func validateCloneRemote(raw, user string) error {
	if user != "" {
		if !cloneUser.MatchString(user) {
			return errors.New("invalid --user; use 1-32 ASCII letters, digits, dots, underscores, or hyphens")
		}
		if _, ok := os.LookupEnv("DOLT_REMOTE_PASSWORD"); !ok {
			return errors.New("set DOLT_REMOTE_PASSWORD in the process environment to use --user")
		}
	}
	return validateRemoteURL(raw, user)
}

// validateRemoteURL checks the shared clone/transfer URL contract without
// consulting credentials or contacting the destination. Configuration needs
// this same contract before a password or even a file target exists.
func validateRemoteURL(raw, user string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Opaque != "" || u.User != nil ||
		strings.ContainsAny(raw, "?#\\") || strings.IndexFunc(raw, unicode.IsSpace) >= 0 ||
		strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 ||
		strings.ContainsAny(u.Path, "?#\\") {
		return errors.New("invalid clone URL; use an absolute http(s) remotesapi or file URL without userinfo, query, or fragment")
	}
	switch u.Scheme {
	case "http", "https":
		if u.Hostname() == "" || u.Path == "" || u.Path == "/" || !strings.HasPrefix(u.Path, "/") {
			return errors.New("clone requires an absolute http(s) remotesapi URL including its database path")
		}
		if port := u.Port(); port != "" {
			if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
				return errors.New("invalid clone remote port; use a port from 1 to 65535")
			}
		} else if strings.HasSuffix(u.Host, ":") {
			return errors.New("invalid clone remote port; omit the colon or supply a port")
		}
	case "file":
		path := cloneFilePath(u)
		if !strings.HasPrefix(raw, "file:///") || u.Host != "" || !filepath.IsAbs(path) || user != "" {
			return errors.New("file clone requires an absolute file:/// path on this OS and no --user")
		}
		// Keep origin compatible with Dolt: its FileFactory unescapes again on
		// POSIX even though our source reader now uses the decoded path directly.
		if strings.Contains(u.Path, "%") {
			return errors.New("file clone URL contains an ambiguous encoded percent sign")
		}
	default:
		return errors.New("unsupported clone URL scheme; use explicit http://, https://, or file:///")
	}
	return nil
}

func cloneFilePath(u *url.URL) string {
	path := filepath.FromSlash(u.Path)
	if filepath.Separator == '\\' && len(path) > 2 && path[0] == '\\' && path[2] == ':' {
		path = path[1:] // file:///C:/... on Windows
	}
	return path
}

// openCloneRemote only reads a file source. FileFactory.CreateDbNoCache creates
// missing oldgen directories even when opening an empty, invalid remote; that
// factory remains suitable for writable stores, but not this clone source.
// NewLocalStore opens existing non-journaled NBS files without initializing them.
// Only its read operations are used by CloneRemote; it is not a read-only Store.
func openCloneRemote(ctx context.Context, remote env.Remote) (db *doltdb.DoltDB, err error) {
	u, err := url.Parse(remote.Url)
	if err != nil {
		return nil, errors.New("invalid clone remote URL")
	}
	if u.Scheme != "file" {
		return remote.GetRemoteDBWithoutCaching(ctx, types.Format_Default, env.NewGRPCDialProvider())
	}
	path, err := filepath.EvalSymlinks(cloneFilePath(u))
	if err != nil {
		return nil, fmt.Errorf("resolve existing file clone source: %w", err)
	}
	quota := nbs.NewUnlimitedMemQuotaProvider()
	newGen, err := nbs.NewLocalStore(ctx, types.Format_Default.VersionString(), path, 0, quota, false)
	if err != nil {
		return nil, err
	}
	var oldGen *nbs.NomsBlockStore
	defer func() {
		if err != nil {
			err = errors.Join(err, newGen.Close())
			if oldGen != nil {
				err = errors.Join(err, oldGen.Close())
			}
		}
	}()
	var source chunks.ChunkStore = newGen
	oldPath := filepath.Join(path, "oldgen")
	_, statErr := os.Stat(oldPath)
	if statErr == nil {
		oldGen, err = nbs.NewLocalStore(ctx, newGen.Version(), oldPath, 0, quota, false)
		if err != nil {
			return nil, err
		}
		if oldGen.Version() != "" && oldGen.Version() != newGen.Version() {
			return nil, errors.New("file clone source has incompatible NBS generation formats; use a consistent remote")
		}
		ghosts, err := nbs.NewGhostBlockStore(path)
		if err != nil {
			return nil, err
		}
		source = nbs.NewGenerationalCS(oldGen, newGen, ghosts)
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect file clone source oldgen: %w", statErr)
	}
	return doltdb.DoltDBFromCS(source, DatabaseName)
}

func cloneTransfer(ctx context.Context, dataDir, remoteURL, user string, expected *ProjectIdentity) (result CloneResult, err error) {
	// Reserve memory exclusively before remote contact. No recursive cleanup runs
	// if foreign content appears later, even while initialization is failing.
	if err := os.Mkdir(filepath.Join(dataDir, DatabaseName), 0o700); err != nil {
		return result, fmt.Errorf("reserve clone database: %w", err)
	}
	fs, err := newCloneFS(dataDir)
	if err != nil {
		return result, err
	}
	// Dolt requires a global config even for an anonymous clone. Keep its empty
	// bootstrap config inside the destination rather than touching the user's home.
	home := filepath.Join(dataDir, ".clone-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return result, fmt.Errorf("reserve clone bootstrap configuration: %w", err)
	}
	if err := fs.MkDirs(filepath.Join(home, ".dolt")); err != nil {
		return result, err
	}
	params := map[string]string{}
	if user != "" {
		params[dbfactory.GRPCUsernameAuthParam] = user
	}
	r := env.NewRemote("origin", remoteURL, params)
	src, err := openCloneRemote(ctx, r)
	if err != nil {
		return result, fmt.Errorf("access clone remote; check URL, connectivity and --user/DOLT_REMOTE_PASSWORD: %w", err)
	}
	defer func() {
		if closeErr := src.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close clone remote: %w", closeErr))
		}
	}()
	dbfs, err := fs.WithWorkingDir(DatabaseName)
	if err != nil {
		return result, err
	}
	dest := env.LoadWithoutDB(ctx, func() (string, error) { return home, nil }, dbfs, doltdb.LocalDirDoltDB, "memdolt")
	defer func() {
		if closeErr := dest.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close clone storage: %w", closeErr))
		}
	}()
	if dest.CfgLoadErr != nil {
		return result, fmt.Errorf("load clone bootstrap configuration: %w", dest.CfgLoadErr)
	}
	dest.DBLoadParams = map[string]any{dbfactory.DisableSingletonCacheParam: struct{}{}}
	if err := dest.InitRepoWithNoData(ctx, src.ValueReadWriter().Format()); err != nil {
		return result, fmt.Errorf("prepare clone storage: %w", err)
	}
	sourceMain, err := src.ResolveCommitRef(ctx, ref.NewBranchRef(MainBranch))
	if err != nil {
		return result, fmt.Errorf("remote has no Dolt data on a main branch; select an initialized memdolt remote: %w", err)
	}
	sourceRoot, err := sourceMain.GetRootValue(ctx)
	if err != nil {
		return result, err
	}
	metadata, err := cloneMetadata(ctx, sourceRoot)
	if err != nil {
		return result, err
	}
	sourceIdentity, err := identityFromMetadata(metadata)
	if err != nil {
		return result, err
	}
	if expected != nil {
		if err := matchProjectIdentity(*expected, sourceIdentity); err != nil {
			return result, err
		}
	}
	dest.RepoState, err = env.CloneRepoState(dest.FS, r)
	if err != nil {
		return result, fmt.Errorf("register clone origin: %w", err)
	}
	dest.RSLoadErr = nil
	if err := actions.CloneRemote(ctx, src, "origin", MainBranch, false, -1, dest, nil); err != nil {
		return result, fmt.Errorf("transfer main; check that the remote contains a committed main branch: %w", err)
	}
	if err := dest.RepoStateWriter().UpdateBranch(MainBranch, env.BranchConfig{Merge: dest.RepoState.Head, Remote: "origin"}); err != nil {
		return result, fmt.Errorf("record clone tracking branch: %w", err)
	}
	return inspectClone(ctx, dataDir, dest.DoltDB(ctx))
}

// inspectClone reads the already-open committed root. Opening another SQL engine
// would reintroduce Dolt's asynchronous old-temp-file sweep with a native FS,
// outside cloneFS's no-cleanup policy. Ordinary driver reopening is tested after
// Clone returns; it is not part of the clone lifecycle.
func inspectClone(ctx context.Context, dataDir string, db *doltdb.DoltDB) (result CloneResult, err error) {
	main, err := db.ResolveCommitRef(ctx, ref.NewBranchRef(MainBranch))
	if err != nil {
		return result, fmt.Errorf("read cloned main; remote must contain a main branch: %w", err)
	}
	hash, err := main.HashOf()
	if err != nil {
		return result, fmt.Errorf("read cloned main hash: %w", err)
	}
	result.Store = dataDir
	result.MainCommit = hash.String()
	root, err := main.GetRootValue(ctx)
	if err != nil {
		return result, fmt.Errorf("read cloned committed root: %w", err)
	}
	metadata, err := cloneMetadata(ctx, root)
	if err != nil {
		return result, fmt.Errorf("invalid cloned memdolt metadata: %w", err)
	}
	rawVersion := metadata[store.SchemaVersionKey]
	if rawVersion == "" {
		rawVersion = "0"
	}
	result.SchemaVersion, err = strconv.Atoi(strings.TrimSpace(rawVersion))
	if err != nil || result.SchemaVersion < 0 {
		return result, errors.New("meta.schema_version is not a version number")
	}
	result.ProjectIdentity, err = identityFromMetadata(metadata)
	if err != nil {
		return result, err
	}
	if result.SchemaVersion == 0 {
		return result, errors.New("remote is not an initialized memdolt store: meta.schema_version is missing or zero; choose a memdolt remote")
	}
	if err := store.CheckSchemaVersion(result.SchemaVersion); err != nil {
		return result, err
	}
	if result.SchemaVersion < store.LatestSchemaVersion() {
		return result, fmt.Errorf("cloned schema v%d needs v%d: run `memdolt init` with the same --dir to apply missing migrations", result.SchemaVersion, store.LatestSchemaVersion())
	}
	// A version marker alone is not proof that the bootstrap read shapes exist.
	for _, check := range []struct{ name, columns string }{
		{"facts", ""}, {"decisions", ""}, {"commands", ""},
		{"project_state", ""}, {"project_arch", ""}, {"documents", ""}, {"doc_chunks", ""},
		{"tasks", "id title status notes created_at updated_at"},
		{"session_notes", "id actor actor_raw text created_at session_id agent_id provider_id model_id variant"},
		{"proposals", "id kind rationale actor created_at target"},
	} {
		tbl, ok, err := root.GetTable(ctx, doltdb.TableName{Name: check.name})
		if err != nil {
			return result, fmt.Errorf("invalid cloned memdolt schema: read %s: %w", check.name, err)
		}
		if !ok {
			return result, fmt.Errorf("invalid cloned memdolt schema: missing %s; choose an initialized memdolt remote", check.name)
		}
		sch, err := tbl.GetSchema(ctx)
		if err != nil {
			return result, fmt.Errorf("invalid cloned memdolt schema for %s: %w", check.name, err)
		}
		for _, column := range strings.Fields(check.columns) {
			if !sch.GetAllCols().Contains(column) {
				return result, fmt.Errorf("invalid cloned memdolt schema: %s lacks %s; choose an initialized memdolt remote", check.name, column)
			}
		}
	}
	return result, ctx.Err()
}

func cloneMetadata(ctx context.Context, root doltdb.RootValue) (values map[string]string, err error) {
	values = map[string]string{}
	meta, ok, err := root.GetTable(ctx, doltdb.TableName{Name: store.MetaTable})
	if err != nil || !ok {
		return values, err
	}
	sch, err := meta.GetSchema(ctx)
	if err != nil {
		return values, err
	}
	cols := sch.GetAllCols()
	if cols.Size() != 2 || cols.GetByIndex(0).Name != "k" || cols.GetByIndex(1).Name != "v" {
		return values, errors.New("expected meta columns k and v")
	}
	index, err := meta.GetRowData(ctx)
	if err != nil {
		return values, err
	}
	rows, err := table.NewTableIterator(ctx, sch, index)
	if err != nil {
		return values, err
	}
	defer func() { err = errors.Join(err, rows.Close(ctx)) }()
	for {
		row, err := rows.Next(ctx)
		if errors.Is(err, io.EOF) {
			return values, nil
		}
		if err != nil {
			return values, err
		}
		key, ok, err := gmssql.Unwrap[string](ctx, row[0])
		if err != nil || !ok {
			return values, errors.New("meta key must be text")
		}
		if (strings.EqualFold(key, "project_id") || strings.EqualFold(key, "project_origin")) && key != strings.ToLower(key) {
			return values, errors.New("project metadata keys must use their exact lowercase spelling")
		}
		if key != store.SchemaVersionKey && key != "project_id" && key != "project_origin" {
			continue
		}
		raw, ok, err := gmssql.Unwrap[string](ctx, row[1])
		if err != nil {
			return values, err
		}
		if !ok {
			return values, errors.New("schema and project metadata must be text")
		}
		values[key] = raw
	}
}

func redactCloneError(err error, user string) error {
	password := os.Getenv("DOLT_REMOTE_PASSWORD")
	if password == "" {
		return err
	}
	message := err.Error()
	for _, secret := range []string{base64.StdEncoding.EncodeToString([]byte(user + ":" + password)), url.QueryEscape(password), url.PathEscape(password), password} {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return &cloneDiagnostic{message: message, cause: err}
}

type cloneDiagnostic struct {
	message string
	cause   error
}

func (e *cloneDiagnostic) Error() string { return e.message }
func (e *cloneDiagnostic) Unwrap() error { return e.cause }
