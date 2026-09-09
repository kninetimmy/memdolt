package localdolt

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dprocedures"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/store/datas/pull"
	gmssql "github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
)

type transferContextKey struct{}

type engineTransfer struct {
	remote    env.Remote
	push      bool
	captured  string
	identity  ProjectIdentity
	attempted bool
	completed bool
	changed   bool
}

func init() {
	// The driver does not expose its engine/session. Dolt's supported external
	// procedure registry gives this private bridge the already-owned DbData.
	// Ordinary SQL, including owner Commit requests, lacks the unexported context
	// capability and cannot use it. Register once, before any engine is opened.
	dprocedures.DoltProcedures = append(dprocedures.DoltProcedures, gmssql.ExternalStoredProcedureDetails{
		Name: "memdolt_transfer", AdminOnly: true,
		Schema:   gmssql.Schema{&gmssql.Column{Name: "status", Type: types.Int64}},
		Function: transferProcedure,
	})
}

func runEngineTransfer(ctx context.Context, conn *sql.Conn, operation, name, rawURL, user, captured string, identity ProjectIdentity) (call *engineTransfer, err error) {
	params := map[string]string{}
	if user != "" {
		params[dbfactory.GRPCUsernameAuthParam] = user
	}
	call = &engineTransfer{remote: env.NewRemote(name, rawURL, params), push: operation == "push", captured: captured, identity: identity}
	ctx = context.WithValue(ctx, transferContextKey{}, call)
	_, err = conn.ExecContext(ctx, "CALL memdolt_transfer()")
	return call, err
}

func transferProcedure(ctx *gmssql.Context) (iter gmssql.RowIter, err error) {
	call, ok := ctx.Value(transferContextKey{}).(*engineTransfer)
	if !ok || call == nil || ctx.GetCurrentDatabase() != DatabaseName || !transferHash.MatchString(call.captured) {
		return nil, errors.New("memdolt_transfer requires the owning Store transfer capability")
	}
	data, ok := dsess.DSessFromSess(ctx.Session).GetDbData(ctx, DatabaseName)
	if !ok {
		return nil, errors.New("owning transfer session has no memory database")
	}
	var remote *doltdb.DoltDB
	if call.push {
		// No cached or personal credential provider: unlike native SQL --user,
		// this fresh Remote cannot inherit a stored username over the override.
		remote, err = call.remote.GetRemoteDBWithoutCaching(ctx, data.Ddb.ValueReadWriter().Format(), env.NewGRPCDialProvider())
	} else {
		remote, err = openCloneRemote(ctx, call.remote)
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, remote.Close()) }()
	if err := remote.Rebase(ctx); err != nil {
		return nil, err
	}
	if call.push {
		exists, err := remote.HasRef(ctx, ref.NewBranchRef(MainBranch))
		if err != nil {
			return nil, err
		}
		if exists {
			main, err := remote.ResolveCommitRef(ctx, ref.NewBranchRef(MainBranch))
			if err != nil {
				return nil, err
			}
			root, err := main.GetRootValue(ctx)
			if err != nil {
				return nil, err
			}
			metadata, err := cloneMetadata(ctx, root)
			if err != nil {
				return nil, err
			}
			identity, err := identityFromMetadata(metadata)
			if err != nil {
				return nil, err
			}
			// An explicit fast-forward push may publish initial identity adoption;
			// a nonempty existing identity must never be changed or removed.
			if identity.ProjectID != "" {
				if err := matchProjectIdentity(call.identity, identity); err != nil {
					return nil, err
				}
			}
		}
		tmp, err := data.Rsw.TempTableFilesDir()
		if err != nil {
			return nil, err
		}
		call.attempted = true
		pull.WithDiscardingStatsCh(func(stats chan pull.Stats) {
			err = actions.PushToRemoteBranch(ctx, data.Rsr, tmp, ref.UpdateMode{},
				ref.NewBranchRef(call.captured), ref.NewBranchRef(MainBranch),
				ref.NewRemoteRef(call.remote.Name, MainBranch), data.Ddb, remote, call.remote, stats)
		})
		if err != nil && !errors.Is(err, doltdb.ErrUpToDate) {
			return nil, err
		}
		call.changed = err == nil
	} else {
		tmp, err := data.Rsw.TempTableFilesDir()
		if err != nil {
			return nil, err
		}
		var commit *doltdb.Commit
		pull.WithDiscardingStatsCh(func(stats chan pull.Stats) {
			// FetchRefSpecs also follows/replaces tags and prints to stdout.
			// Fetch only main's commit/history, without those tag side effects.
			commit, err = actions.FetchRemoteBranch(ctx, tmp, call.remote, remote, data.Ddb, ref.NewBranchRef(MainBranch), stats)
		})
		if err != nil {
			return nil, err
		}
		// Only the selected tracking ref may be replaced. Main is promoted
		// separately after immutable schema, ancestry and changed-text validation.
		if err := data.Ddb.SetHeadToCommit(ctx, ref.NewRemoteRef(call.remote.Name, MainBranch), commit); err != nil {
			return nil, err
		}
	}
	call.completed = true
	return gmssql.RowsToRowIter(gmssql.Row{int64(0)}), nil
}
