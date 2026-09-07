package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/kninetimmy/memdolt/internal/store"
)

// RepoStatusOptions selects inspection, never promotion. Empty Remote selects
// origin if present; an explicitly named missing remote is an error.
type RepoStatusOptions struct {
	Remote string `json:"remote,omitempty" jsonschema:"configured remote name; defaults to origin when present"`
	Local  bool   `json:"local,omitempty" jsonschema:"offline only; incompatible with remote, user and diff"`
	Diff   bool   `json:"diff,omitempty" jsonschema:"exact committed differences from local main to remote main, including row values"`
	User   string `json:"user,omitempty" jsonschema:"SQL username override; password comes only from the owner's environment"`
}

func (o RepoStatusOptions) Validate() error {
	if o.Local && (o.Remote != "" || o.User != "" || o.Diff) {
		return errors.New("--local cannot be combined with a remote, --user or --diff; see `memdolt repo status --help`")
	}
	if o.Remote != "" && !transferRemoteName.MatchString(o.Remote) {
		return errors.New("invalid remote name; select one configured name; see `memdolt repo status --help`")
	}
	if o.User != "" && (!cloneUser.MatchString(o.User) || strings.HasPrefix(o.User, "-")) {
		return errors.New("invalid --user; use 1-32 ASCII letters, digits, dots, underscores or hyphens without a leading hyphen")
	}
	return nil
}

type RepoTableChange struct {
	Table  string `json:"table"`
	Staged bool   `json:"staged"`
	Status string `json:"status"`
}

type RepoStatusReport struct {
	LocalOnly        bool              `json:"localOnly"`
	Store            string            `json:"store"`
	MainCommit       string            `json:"mainCommit"`
	SchemaVersion    int               `json:"schemaVersion"`
	Clean            bool              `json:"clean"`
	Changes          []RepoTableChange `json:"changes"`
	PendingProposals struct {
		Repo   int `json:"repo"`
		Global int `json:"global"`
	} `json:"pendingProposals"`
	Remote       string         `json:"remote,omitempty"`
	RemoteCommit string         `json:"remoteCommit,omitempty"`
	MergeBase    string         `json:"mergeBase,omitempty"`
	Status       string         `json:"status"`
	Assessment   string         `json:"assessment"`
	Remedy       string         `json:"remedy,omitempty"`
	Conflicts    []RepoConflict `json:"conflicts,omitempty"`
	Diff         *RepoDiff      `json:"diff,omitempty"`
}

type RepoConflict struct {
	Table       string `json:"table"`
	Data        int    `json:"data"`
	Constraints int    `json:"constraints"`
}

type RepoDiff struct {
	Direction  string          `json:"direction"`
	FromCommit string          `json:"fromCommit"`
	ToCommit   string          `json:"toCommit"`
	Tables     []RepoTableDiff `json:"tables"`
}

type RepoTableDiff struct {
	Table      string        `json:"table"`
	FromSchema string        `json:"fromSchema,omitempty"`
	ToSchema   string        `json:"toSchema,omitempty"`
	Rows       []RepoRowDiff `json:"rows"`
}

// SQL-rendered values avoid the pinned driver's NULL ENUM panic. A missing
// image is omitted; every column in an existing image is present, including NULL.
type RepoRowDiff struct {
	Type string             `json:"type"`
	From map[string]*string `json:"from,omitempty"`
	To   map[string]*string `json:"to,omitempty"`
}

func (s *Store) RepoStatus(ctx context.Context, opts RepoStatusOptions) (RepoStatusReport, error) {
	return s.repoStatus(ctx, opts, repoStatusHooks{})
}

type repoStatusHooks struct {
	afterCapture func()
	afterMerge   func() error
	rollback     func(*sql.Tx) error
}

func (s *Store) repoStatus(ctx context.Context, opts RepoStatusOptions, hooks repoStatusHooks) (report RepoStatusReport, err error) {
	report = RepoStatusReport{LocalOnly: opts.Local, Store: s.DataDir(), Changes: []RepoTableChange{}, Status: "refused", Assessment: "unassessed"}
	if err := opts.Validate(); err != nil {
		return report, err
	}
	// ponytail: hold the existing mutation lock through fetch and rollback.
	// Split this only if remote latency warrants a more complex capture protocol.
	// Foreign Dolt processes do not participate in this Store's mutex.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	db, err := s.handle()
	if err != nil {
		return report, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return report, err
	}
	user := opts.User
	defer func() {
		err = errors.Join(err, conn.Close())
		if err != nil {
			report.Status, report.Assessment, report.Diff = "refused", "unassessed", nil
			if !opts.Local {
				err = fmt.Errorf("repo status: fetched objects and the selected tracking ref may remain; inspect `memdolt repo status --local` and remote main before retrying: %w", err)
			}
			err = redactCloneError(err, user)
		}
	}()
	if err := readLocalRepoStatus(ctx, conn, &report); err != nil {
		return report, err
	}
	if opts.Local {
		report.Status, report.Assessment = "offline", "not-requested"
		return report, nil
	}
	localSchema, err := statusSchema(ctx, conn, report.MainCommit)
	if err != nil {
		return report, fmt.Errorf("unsupported local committed schema: %w", err)
	}
	// Refresh from validated native configuration just as ListRemotes does.
	// Reuse its capability on this connection without re-entering proposalMu.
	configured := engineRemotes{base: s.paths.Base()}
	if _, err := conn.ExecContext(context.WithValue(ctx, remoteContextKey{}, &configured), "CALL memdolt_remotes()"); err != nil {
		return report, err
	}
	report.Remote = opts.Remote
	if report.Remote == "" {
		report.Remote = "origin"
	}
	if !slices.ContainsFunc(configured.remotes, func(remote Remote) bool { return remote.Name == report.Remote }) {
		if opts.Remote != "" {
			return report, errors.New("the explicitly selected remote is not configured; inspect `memdolt repo remote list` or add it with `memdolt repo remote add`")
		}
		report.LocalOnly, report.Status, report.Assessment = true, "no-remote", "not-requested"
		report.Remedy = "Configure origin with `memdolt repo remote add origin <absolute-url>`, or select a configured remote by name."
		return report, nil
	}
	remoteURL, selectedUser, err := configuredTransferRemote(ctx, conn, TransferOptions{Remote: report.Remote, User: opts.User})
	user = selectedUser
	if err != nil {
		return report, err
	}
	if err := validateTransferFile(remoteURL); err != nil {
		return report, err
	}
	if hooks.afterCapture != nil {
		hooks.afterCapture()
	}
	if _, err := runEngineTransfer(ctx, conn, "pull", report.Remote, remoteURL, user, report.MainCommit); err != nil {
		return report, fmt.Errorf("fetch remote main; check its main branch, connectivity and owner credentials: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", "remotes/"+report.Remote+"/main").Scan(&report.RemoteCommit); err != nil {
		return report, fmt.Errorf("read fetched main: %w", err)
	}
	remoteSchema, err := statusSchema(ctx, conn, report.RemoteCommit)
	if err != nil {
		return report, fmt.Errorf("incompatible incoming committed schema; repair/migrate the remote with a compatible client before retrying: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_MERGE_BASE(?, ?)", report.MainCommit, report.RemoteCommit).Scan(&report.MergeBase); err != nil {
		return report, fmt.Errorf("cannot establish committed-main ancestry; inspect and reconcile the histories with Dolt: %w", err)
	}
	switch {
	case report.MainCommit == report.RemoteCommit:
		report.Status = "current"
	case report.MergeBase == report.RemoteCommit:
		report.Status = "ahead"
	case report.MergeBase == report.MainCommit:
		report.Status = "behind"
	default:
		report.Status = "diverged-unassessed"
	}
	report.Assessment = "not-needed"
	if report.Status == "diverged-unassessed" {
		report.Assessment = "unassessed"
		if !report.Clean {
			report.Remedy = "Mergeability requires clean main; finish or resolve staged and working changes, then run repo status again."
		} else if err := previewRepoMerge(ctx, conn, &report, localSchema, remoteSchema, hooks); err != nil {
			return report, err
		}
	}
	if opts.Diff {
		report.Diff, err = repoDiff(ctx, conn, report.MainCommit, report.RemoteCommit, localSchema, remoteSchema)
	}
	return report, err
}

func readLocalRepoStatus(ctx context.Context, conn *sql.Conn, report *RepoStatusReport) error {
	var err error
	report.MainCommit, err = branchHead(ctx, conn, MainBranch)
	if err != nil {
		return err
	}
	if !transferHash.MatchString(report.MainCommit) {
		return errors.New("main is not an immutable Dolt commit hash")
	}
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT v FROM "+quoteIdentifier(DatabaseName+"/"+report.MainCommit)+".meta WHERE k = ?", store.SchemaVersionKey).Scan(&version); err != nil {
		return fmt.Errorf("read committed schema version: %w", err)
	}
	report.SchemaVersion, err = strconv.Atoi(version)
	if err != nil {
		return errors.New("invalid committed schema version; inspect the store with Dolt")
	}
	if err := store.CheckSchemaVersion(report.SchemaVersion); err != nil {
		return err
	}
	if report.SchemaVersion < store.LatestSchemaVersion() {
		return errors.New("run `memdolt init` to apply the missing migrations")
	}
	changes, err := conn.QueryContext(ctx, "SELECT table_name, staged, status FROM `memory/main`.dolt_status ORDER BY table_name, staged, status")
	if err != nil {
		return fmt.Errorf("read the main working set: %w", err)
	}
	for changes.Next() {
		var change RepoTableChange
		if err := changes.Scan(&change.Table, &change.Staged, &change.Status); err != nil {
			return errors.Join(err, changes.Close())
		}
		report.Changes = append(report.Changes, change)
	}
	if err := errors.Join(changes.Err(), changes.Close()); err != nil {
		return err
	}
	report.Clean = len(report.Changes) == 0
	proposals, err := pendingProposals(ctx, conn)
	if err != nil {
		return fmt.Errorf("read pending proposals: %w", err)
	}
	for _, proposal := range proposals {
		switch proposal.Target {
		case TargetRepo:
			report.PendingProposals.Repo++
		case TargetGlobal:
			report.PendingProposals.Global++
		default:
			return errors.New("a pending proposal has an unknown target; inspect `memdolt review`")
		}
	}
	return nil
}

// Status verifies the constraints it can assess as well as transfer's column
// contract. This tighter inspection rule does not change Push/Pull validation.
func statusSchema(ctx context.Context, conn *sql.Conn, hash string) (map[string]string, error) {
	if err := validateTransferSchema(ctx, conn, hash); err != nil {
		return nil, err
	}
	definitions := map[string]string{}
	for _, table := range transferTables {
		var name, ddl string
		if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+quoteIdentifier(DatabaseName+"/"+hash)+"."+quoteIdentifier(table.name)).Scan(&name, &ddl); err != nil {
			return nil, err
		}
		if err := statusConstraints(table.name, ddl); err != nil {
			return nil, err
		}
		definitions[table.name] = ddl
	}
	return definitions, nil
}

var statusUniqueKeys = map[string]struct{ name, columns string }{
	"facts":      {"uk_fact_live_key", "live_key"},
	"documents":  {"uk_doc_path", "path"},
	"doc_chunks": {"uk_chunk", "doc_id ord"},
}

func statusConstraints(table, definition string) error {
	statement, err := sqlparser.Parse(definition)
	if err != nil {
		return errors.New("cannot parse committed schema for constraint assessment")
	}
	ddl, ok := statement.(*sqlparser.DDL)
	if !ok || ddl.TableSpec == nil {
		return errors.New("missing committed table definition")
	}
	want, required := statusUniqueKeys[table]
	unique := 0
	for _, index := range ddl.TableSpec.Indexes {
		if !index.Info.Unique || index.Info.Primary {
			continue
		}
		unique++
		var columns []string
		for _, field := range index.Fields {
			if field.Length != nil || field.Expression != nil {
				return errors.New("unsupported unique index expression or prefix; inspect the committed schema")
			}
			columns = append(columns, field.Column.String())
		}
		if !required || index.Info.Name.String() != want.name || strings.Join(columns, " ") != want.columns {
			return fmt.Errorf("unsupported unique constraint in %s; inspect the committed schema", table)
		}
	}
	if required && unique != 1 {
		return fmt.Errorf("missing required unique constraint in %s", table)
	}
	foreign := 0
	for _, constraint := range ddl.TableSpec.Constraints {
		fk, ok := constraint.Details.(*sqlparser.ForeignKeyDefinition)
		if !ok || table != "doc_chunks" || len(fk.Source) != 1 || fk.Source[0].String() != "doc_id" ||
			fk.ReferencedTable.Name.String() != "documents" || !fk.ReferencedTable.DbQualifier.IsEmpty() || !fk.ReferencedTable.SchemaQualifier.IsEmpty() ||
			len(fk.ReferencedColumns) != 1 || fk.ReferencedColumns[0].String() != "id" || fk.OnDelete != sqlparser.Cascade ||
			(fk.OnUpdate != sqlparser.DefaultAction && fk.OnUpdate != sqlparser.Restrict && fk.OnUpdate != sqlparser.NoAction) {
			return fmt.Errorf("unsupported constraint in %s; inspect the committed schema", table)
		}
		foreign++
	}
	if table == "doc_chunks" && foreign != 1 {
		return errors.New("missing document chunk foreign key in committed schema")
	}
	return nil
}

func previewRepoMerge(ctx context.Context, conn *sql.Conn, report *RepoStatusReport, localSchema, remoteSchema map[string]string, hooks repoStatusHooks) (err error) {
	if err := requireTransferClean(ctx, conn); err != nil {
		return fmt.Errorf("mergeability requires a clean main session without an active merge/conflict: %w", err)
	}
	baseSchema, err := statusSchema(ctx, conn, report.MergeBase)
	if err != nil || !maps.Equal(localSchema, remoteSchema) || !maps.Equal(localSchema, baseSchema) {
		return errors.Join(errors.New("mergeability of schema changes is unassessed; inspect and reconcile the committed schemas with Dolt"), err)
	}
	tx, finish, err := repoMergeTransaction(ctx, conn, report.MainCommit, hooks.rollback)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, finish(false)) }()
	var hash, message sql.NullString
	var fastForward, conflicts sql.NullInt64
	if err := tx.QueryRowContext(ctx, "CALL DOLT_MERGE('--no-ff', '--no-commit', ?)", report.RemoteCommit).Scan(&hash, &fastForward, &conflicts, &message); err != nil {
		return fmt.Errorf("merge preview failed; mergeability is unassessed: %w", err)
	}
	if hooks.afterMerge != nil {
		if err := hooks.afterMerge(); err != nil {
			return err
		}
	}
	data, err := surfaceSummary(ctx, tx, "dolt_conflicts", "num_conflicts")
	if err != nil {
		return err
	}
	violations, err := surfaceSummary(ctx, tx, "dolt_constraint_violations", "num_violations")
	if err != nil {
		return err
	}
	for table := range data {
		if _, known := localSchema[table]; !known {
			return errors.New("merge preview reported an unknown conflict table; inspect with Dolt")
		}
	}
	for _, table := range slices.Sorted(maps.Keys(violations)) {
		index := slices.IndexFunc(transferTables, func(entry struct{ name, columns, text string }) bool { return entry.name == table })
		if index < 0 {
			return errors.New("merge preview reported an unknown constraint table; inspect with Dolt")
		}
		key, _, _ := strings.Cut(transferTables[index].columns, ":")
		_, err := verifyMergeViolations(ctx, tx, table, key, report.MergeBase, report.MainCommit, report.RemoteCommit)
		if errors.Is(err, errConstraintViolation) {
			continue
		}
		if err != nil {
			return fmt.Errorf("merge constraint assessment refused; inspect with Dolt: %w", err)
		}
		delete(violations, table)
	}
	// Verify the maintained unique/FK invariants even if Dolt records no rows.
	for _, table := range slices.Sorted(maps.Keys(statusUniqueKeys)) {
		if err := requireConstraintHolds(ctx, tx, table, statusUniqueKeys[table].name); err != nil {
			if !errors.Is(err, errConstraintViolation) {
				return err
			}
			violations[table] = max(violations[table], 1)
		}
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_chunks c LEFT JOIN documents d ON c.doc_id = d.id WHERE c.doc_id IS NOT NULL AND d.id IS NULL").Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		violations["doc_chunks"] = max(violations["doc_chunks"], orphans)
	}
	for _, table := range slices.Sorted(maps.Keys(localSchema)) {
		if data[table] != 0 || violations[table] != 0 {
			report.Conflicts = append(report.Conflicts, RepoConflict{Table: table, Data: data[table], Constraints: violations[table]})
		}
	}
	if len(report.Conflicts) > 0 {
		report.Status, report.Assessment = "conflicted", "conflicted"
		report.Remedy = "Inspect the captured histories with Dolt and resolve conflicts manually; repo status never promotes or resolves remote changes."
		return nil
	}
	if err := requireSurfacesEmpty(ctx, tx); err != nil {
		return err
	}
	report.Status, report.Assessment = "diverged-mergeable", "mergeable"
	return nil
}

func repoDiff(ctx context.Context, conn *sql.Conn, from, to string, fromSchema, toSchema map[string]string) (*RepoDiff, error) {
	diff := &RepoDiff{Direction: "local-to-remote", FromCommit: from, ToCommit: to, Tables: []RepoTableDiff{}}
	for _, table := range slices.Sorted(maps.Keys(fromSchema)) {
		rows, err := repoDiffRows(ctx, conn, table, from, to)
		if err != nil {
			return nil, err
		}
		entry := RepoTableDiff{Table: table, Rows: rows}
		if fromSchema[table] != toSchema[table] {
			entry.FromSchema, entry.ToSchema = fromSchema[table], toSchema[table]
		}
		if len(rows) > 0 || entry.FromSchema != "" {
			diff.Tables = append(diff.Tables, entry)
		}
	}
	return diff, nil
}

func repoDiffRows(ctx context.Context, conn *sql.Conn, table, from, to string) (changes []RepoRowDiff, err error) {
	// The column contract comes from the validated immutable commits, never a
	// dirty working table. The revision-qualified diff also survives dirty DDL.
	index := slices.IndexFunc(transferTables, func(entry struct{ name, columns, text string }) bool { return entry.name == table })
	var columns, projected []string
	for _, field := range strings.Fields(transferTables[index].columns) {
		name, _, _ := strings.Cut(field, ":")
		columns = append(columns, name)
	}
	for _, side := range []string{"from_", "to_"} {
		for _, column := range columns {
			projected = append(projected, "CAST("+quoteIdentifier(side+column)+" AS CHAR)")
		}
	}
	projected = append(projected, "diff_type")
	query := "SELECT " + strings.Join(projected, ", ") + " FROM " + quoteIdentifier(DatabaseName+"/"+from) + "." + quoteIdentifier("dolt_commit_diff_"+table) +
		" WHERE from_commit = ? AND to_commit = ? ORDER BY COALESCE(" + quoteIdentifier("from_"+columns[0]) + ", " + quoteIdentifier("to_"+columns[0]) + "), diff_type"
	rows, err := conn.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, fmt.Errorf("diff committed %s: %w", table, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	changes = []RepoRowDiff{}
	for rows.Next() {
		cells := make([]sql.NullString, len(projected))
		targets := make([]any, len(cells))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		change := RepoRowDiff{Type: cells[len(cells)-1].String}
		switch change.Type {
		case "added":
			change.To = map[string]*string{}
		case "removed":
			change.Type, change.From = "deleted", map[string]*string{}
		case "modified":
			change.From, change.To = map[string]*string{}, map[string]*string{}
		default:
			return nil, errors.New("unknown Dolt row difference classification")
		}
		for side, image := range []map[string]*string{change.From, change.To} {
			if image == nil {
				continue
			}
			for i, column := range columns {
				cell := cells[side*len(columns)+i]
				image[column] = nil
				if cell.Valid {
					image[column] = &cell.String
				}
			}
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}
