package localdolt

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dprocedures"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
)

// Remote is the complete configuration surface: no password, arbitrary driver
// parameter, or caller-supplied refspec can cross the application/IPC boundary.
type Remote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	User string `json:"user,omitempty"`
}

// ValidateRemote performs no IO and never quotes rejected values in errors.
func ValidateRemote(remote Remote) error {
	if !transferRemoteName.MatchString(remote.Name) {
		return errors.New("invalid remote name; use 1-64 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	if remote.User != "" && (!cloneUser.MatchString(remote.User) || strings.HasPrefix(remote.User, "-")) {
		return errors.New("invalid --user; use 1-32 ASCII letters, digits, dots, underscores or hyphens without a leading hyphen")
	}
	return validateRemoteURL(remote.URL, remote.User)
}

func sanitizedRemote(name, rawURL string, params map[string]string) (Remote, error) {
	remote := Remote{Name: name, URL: rawURL}
	for key, value := range params {
		if key != dbfactory.GRPCUsernameAuthParam || !cloneUser.MatchString(value) || strings.HasPrefix(value, "-") {
			return Remote{}, errors.New("unsupported remote parameters; configure only a valid SQL username, never a password or driver option")
		}
		remote.User = value
	}
	// Native Dolt stores Windows file:///C:/ as file://C:/ and decodes spaces.
	// Restore only those spellings; the shared validator still refuses unsafe
	// URL components and encoded-percent ambiguity. Never rewrite stored data.
	if filepath.Separator == '\\' && strings.HasPrefix(remote.URL, "file://") && len(remote.URL) > 9 && remote.URL[8:10] == ":/" &&
		((remote.URL[7] >= 'A' && remote.URL[7] <= 'Z') || (remote.URL[7] >= 'a' && remote.URL[7] <= 'z')) {
		remote.URL = "file:///" + remote.URL[7:]
	}
	if parsed, err := url.Parse(remote.URL); err == nil && parsed.Scheme == "file" {
		remote.URL = parsed.String()
	}
	if err := ValidateRemote(remote); err != nil {
		return Remote{}, err
	}
	return remote, nil
}

func (s *Store) ListRemotes(ctx context.Context) ([]Remote, error) {
	call, err := s.remotes(ctx, nil, nil)
	return call.remotes, err
}

func (s *Store) AddRemote(ctx context.Context, remote Remote) (Remote, error) {
	if err := ValidateRemote(remote); err != nil {
		return Remote{}, err
	}
	call, err := s.remotes(ctx, &remote, nil)
	return call.added, err
}

type remoteContextKey struct{}

type engineRemotes struct {
	base    string
	add     *Remote
	added   Remote
	remotes []Remote
	save    func(*env.RepoState, filesys.ReadWriteFS) error
}

func init() {
	// Like memdolt_transfer, only the owning Store supplies this private context
	// capability. Ordinary SQL and IPC Commit cannot invoke the native seam.
	dprocedures.DoltProcedures = append(dprocedures.DoltProcedures, gmssql.ExternalStoredProcedureDetails{
		Name: "memdolt_remotes", AdminOnly: true,
		Schema: gmssql.Schema{&gmssql.Column{Name: "status", Type: types.Int64}}, Function: remoteProcedure,
	})
}

func (s *Store) remotes(ctx context.Context, add *Remote, save func(*env.RepoState, filesys.ReadWriteFS) error) (call engineRemotes, err error) {
	// This includes reads so no caller observes the save/read-back/cache window.
	// Transfers, direct commits and proposal mutations already share this lock.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	if err := ctx.Err(); err != nil {
		return call, err
	}
	db, err := s.handle()
	if err != nil {
		return call, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return call, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	head, err := branchHead(ctx, conn, MainBranch)
	if err != nil {
		return call, err
	}
	if err := validateTransferSchema(ctx, conn, head); err != nil {
		return call, fmt.Errorf("unsupported local store; inspect or explicitly initialize/upgrade it before configuring remotes: %w", err)
	}
	call.base, call.add, call.save = s.paths.Base(), add, save
	ctx = context.WithValue(ctx, remoteContextKey{}, &call)
	_, err = conn.ExecContext(ctx, "CALL memdolt_remotes()")
	return call, err
}

func remoteProcedure(ctx *gmssql.Context) (gmssql.RowIter, error) {
	call, ok := ctx.Value(remoteContextKey{}).(*engineRemotes)
	if !ok || call == nil || ctx.GetCurrentDatabase() != DatabaseName {
		return nil, errors.New("memdolt_remotes requires the owning Store configuration capability")
	}
	session := dsess.DSessFromSess(ctx.Session)
	data, ok := session.GetDbData(ctx, DatabaseName)
	if !ok {
		return nil, errors.New("owning configuration session has no memory database")
	}
	fs, err := session.Provider().FileSystemForDatabase(DatabaseName)
	if err != nil {
		return nil, errors.New("cannot locate native remote configuration")
	}
	path, err := fs.Abs(filepath.Join(".dolt", "repo_state.json"))
	if err != nil {
		return nil, errors.New("cannot locate native remote configuration file")
	}
	if err := clonePath(call.base, path); err != nil {
		return nil, errors.New("remote configuration path is unsafe; inspect it with the owner stopped")
	}
	state, err := env.LoadRepoState(fs)
	if err != nil {
		return nil, errors.New("cannot read native remote configuration; inspect it with the owner stopped")
	}
	call.remotes, err = sanitizedRemotes(state)
	if err != nil {
		return nil, err
	}
	cache, err := data.Rsr.GetRemotes()
	if err != nil {
		return nil, errors.New("cannot read the owner's remote configuration")
	}
	if call.add != nil {
		if _, exists := state.Remotes.Get(call.add.Name); exists {
			return nil, errors.New("remote name is already configured; inspect `memdolt repo remote list` and choose an unused name")
		}
		params := map[string]string{}
		if call.add.User != "" {
			params[dbfactory.GRPCUsernameAuthParam] = call.add.User
		}
		remote := env.NewRemote(call.add.Name, call.add.URL, params)
		if _, found := env.CheckRemoteAddressConflict(remote.Url, nil, state.Backups); found {
			return nil, errors.New("remote URL conflicts with a configured backup; inspect native configuration with the owner stopped")
		}
		// SessionStateAdapter.AddRemote publishes its cache entry before Save,
		// so a failed save can expose an unpersisted remote. Save the freshly
		// loaded native state first, then confirm it before publishing the cache.
		// Native Save uses a temporary file, sync and rename; no memory roots or
		// refs are written. proposalMu excludes cooperating memdolt mutations,
		// not a foreign Dolt process or platform-specific crash behavior.
		state.AddRemote(remote)
		save := (*env.RepoState).Save
		if call.save != nil {
			save = call.save
		}
		saveErr := save(state, fs)
		persisted, readErr := env.LoadRepoState(fs)
		if readErr != nil {
			return nil, errors.New("remote persistence could not be confirmed; inspect `memdolt repo remote list` before retrying")
		}
		stored, exists := persisted.Remotes.Get(remote.Name)
		if !exists || !reflect.DeepEqual(stored, remote) {
			return nil, errors.New("remote was not confirmed persisted; inspect `memdolt repo remote list` before retrying")
		}
		call.added = *call.add
		cache.Set(remote.Name, stored)
		if saveErr != nil {
			return nil, errors.New("remote is present but native save finalization failed; inspect `memdolt repo remote list` before retrying")
		}
	} else {
		// A previous response/read-back may have failed after persistence. A
		// successful list refreshes the owner's in-memory view from the verified
		// native file so subsequent transfers consume exactly what was listed.
		state.Remotes.Iter(func(name string, remote env.Remote) bool {
			cache.Set(name, remote)
			return true
		})
	}
	return gmssql.RowsToRowIter(gmssql.Row{int64(0)}), nil
}

func sanitizedRemotes(state *env.RepoState) ([]Remote, error) {
	remotes := []Remote{}
	var err error
	state.Remotes.Iter(func(name string, remote env.Remote) bool {
		if name != remote.Name {
			err = errors.New("stored remote name does not match its entry; inspect native configuration with the owner stopped")
			return false
		}
		var sanitized Remote
		sanitized, err = sanitizedRemote(name, remote.Url, remote.Params)
		if err != nil {
			return false
		}
		remotes = append(remotes, sanitized)
		return true
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(remotes, func(a, b Remote) int { return strings.Compare(a.Name, b.Name) })
	return remotes, nil
}
