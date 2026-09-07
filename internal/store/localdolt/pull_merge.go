package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

// PullResolution is one complete operator decision over exactly two displayed
// commits. It contains no SQL, revision expression, destination, or password.
type PullResolution struct {
	LocalCommit  string       `json:"localCommit"`
	RemoteCommit string       `json:"remoteCommit"`
	Choices      []PullChoice `json:"choices"`
}

type PullChoice struct {
	Conflict string             `json:"conflict"`
	Take     string             `json:"take"`
	Winner   string             `json:"winner,omitempty"`
	Row      map[string]*string `json:"row,omitempty"`
	Reopen   bool               `json:"reopen,omitempty"`
}

type PullConflict struct {
	ID    string            `json:"id"`
	Kind  string            `json:"kind"`
	Table string            `json:"table"`
	Rows  []PullConflictRow `json:"rows"`
}

type PullConflictRow struct {
	ID          string             `json:"id"`
	Base        map[string]*string `json:"base"`
	Ours        map[string]*string `json:"ours"`
	Theirs      map[string]*string `json:"theirs"`
	Merged      map[string]*string `json:"merged"`
	BaseBlame   *PullProvenance    `json:"baseBlame,omitempty"`
	OursBlame   *PullProvenance    `json:"oursBlame,omitempty"`
	TheirsBlame *PullProvenance    `json:"theirsBlame,omitempty"`
}

type PullProvenance struct {
	Commit  string    `json:"commit"`
	Author  string    `json:"author"`
	Email   string    `json:"email"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
}

const pullRemedy = "Review every conflict's base/ours/theirs rows and blame. Save {\"localCommit\":\"<displayed local hash>\",\"remoteCommit\":\"<displayed remote hash>\",\"choices\":[...]} as a JSON file, then run `memdolt pull [remote] --resolve <file>`. See `memdolt pull --help`. Main is unchanged; fetched objects and the tracking ref may remain."

func (s *Store) pullMerge(ctx context.Context, conn *sql.Conn, opts TransferOptions, result *TransferResult, hooks transferHooks) (err error) {
	var schemas []map[string]string
	for _, hash := range []string{result.MergeBase, result.LocalCommit, result.RemoteCommit} {
		definition, err := statusSchema(ctx, conn, hash)
		if err != nil {
			return fmt.Errorf("incompatible merge schema; upgrade or repair with a compatible client: %w", err)
		}
		schemas = append(schemas, definition)
	}
	if !maps.Equal(schemas[0], schemas[1]) || !maps.Equal(schemas[0], schemas[2]) {
		return errors.New("schema changes cannot be resolved by pull; upgrade and reconcile committed schemas with Dolt")
	}
	author := opts.Author
	if author == (store.Actor{}) {
		author = s.cfg.Actor
		if opts.Resolution != nil {
			author = store.Actor{Name: "user", Email: "user@memdolt.invalid"}
		}
	}
	if err := author.Validate(); err != nil {
		return err
	}
	if opts.Resolution != nil && author.Name != "user" {
		return errors.New("conflict resolution requires an explicit human decision attributed to user")
	}
	if err := s.checkDenyList([]string{author.Name, author.Email}); err != nil {
		return err
	}
	if hooks.beforeMove != nil {
		hooks.beforeMove()
	}
	tx, finish, err := repoMergeTransaction(ctx, conn, result.LocalCommit, nil)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			err = errors.Join(err, finish(false))
			result.Cleared = nil
		}
	}()
	if err := doltMerge(ctx, tx, PendingProposal{Commit: result.RemoteCommit}); err != nil {
		return err
	}
	result.Conflicts, result.Cleared, err = pullConflicts(ctx, tx, *result)
	if err != nil {
		return err
	}
	if len(result.Conflicts) != 0 {
		finished = true
		if err := finish(false); err != nil {
			return err
		}
		result.Cleared = nil
		if err := fillPullBlame(ctx, conn, result); err != nil {
			return err
		}
		if opts.Resolution == nil {
			result.Status, result.Remedy = "conflicted", pullRemedy
			return nil
		}
		// Recreate the same merge after reading the immutable blame views. The
		// shared transaction gate revalidates clean main; no user wait occurs here.
		tx, finish, err = repoMergeTransaction(ctx, conn, result.LocalCommit, nil)
		if err != nil {
			return err
		}
		finished = false
		if err := doltMerge(ctx, tx, PendingProposal{Commit: result.RemoteCommit}); err != nil {
			return err
		}
	}
	if opts.Resolution != nil {
		if err := s.resolvePullConflicts(ctx, tx, result.Conflicts, opts.Resolution.Choices); err != nil {
			return err
		}
	}
	// Re-read and classify every remaining record after the repairs, then verify
	// both surfaces and all maintained UNIQUE/FK and supersession invariants.
	cleared, err := clearPullViolations(ctx, tx, *result)
	if err != nil {
		return err
	}
	result.Cleared = append(result.Cleared, cleared...)
	if err := requireSurfacesEmpty(ctx, tx); err != nil {
		return err
	}
	if err := requirePullInvariants(ctx, tx); err != nil {
		return err
	}
	if hooks.beforeCommit != nil {
		if err := hooks.beforeCommit(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var hash string
	result.Status = "unknown"
	if err := tx.QueryRowContext(ctx, "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)",
		"pull "+opts.Remote+" merge "+result.RemoteCommit, author.String()).Scan(&hash); err != nil {
		return fmt.Errorf("pull merge promotion was not confirmed; inspect local main before retrying: %w", err)
	}
	// The pinned DOLT_COMMIT concludes the Dolt transaction itself. Its returned
	// hash is already durable: database/sql Commit is finalization, not a point
	// after which rollback could retract the merge.
	result.MainCommit, result.Changed, result.Status, result.Remedy = hash, true, "changed", ""
	finished = true
	var commitErr error
	if hooks.finalize != nil {
		commitErr = hooks.finalize(tx)
	} else {
		commitErr = finish(true)
	}
	for _, entry := range result.Cleared {
		s.logger.Info("cleared verified pull constraint records", "commit", hash, "table", entry.Table, "constraint", entry.Constraint, "rows", entry.Rows)
	}
	if hooks.afterMove != nil {
		commitErr = errors.Join(commitErr, hooks.afterMove())
	}
	return commitErr
}

// repoMergeTransaction is shared by status preview and pull. Callers promote
// only via DOLT_COMMIT after validation; finish(true) finalizes the SQL handle.
// On pre-promotion refusal, rollback verifies main and both captured roots,
// including after cancellation.
func repoMergeTransaction(ctx context.Context, conn *sql.Conn, local string, rollback func(*sql.Tx) error) (*sql.Tx, func(bool) error, error) {
	if err := requireTransferClean(ctx, conn); err != nil {
		return nil, nil, err
	}
	var working, staged string
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED')").Scan(&working, &staged); err != nil {
		return nil, nil, err
	}
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, nil, err
	}
	finish := func(commit bool) error {
		if commit {
			return tx.Commit()
		}
		rollbackErr := error(nil)
		if rollback != nil {
			rollbackErr = rollback(tx)
		} else {
			rollbackErr = tx.Rollback()
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		var afterWorking, afterStaged string
		rootErr := conn.QueryRowContext(cleanup, "SELECT DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED')").Scan(&afterWorking, &afterStaged)
		head, headErr := branchHead(cleanup, conn, MainBranch)
		cleanErr := requireTransferClean(cleanup, conn)
		if rollbackErr != nil || rootErr != nil || headErr != nil || cleanErr != nil || head != local || working != afterWorking || staged != afterStaged {
			return errors.Join(errors.New("repository merge restoration could not be verified; stop and inspect main and working/staged roots with Dolt"), rollbackErr, rootErr, headErr, cleanErr)
		}
		return nil
	}
	head, err := branchHead(ctx, tx, MainBranch)
	if err != nil || head != local {
		return nil, nil, errors.Join(errors.New("local main changed after capture; obtain a fresh review"), err, finish(false))
	}
	return tx, finish, nil
}

// Names originate only from the maintained schema, never from a choice or an
// unchecked system-table record. The same allow-list gates all pull queries.
func pullColumns(table string) ([]string, error) {
	for _, entry := range transferTables {
		if entry.name == table {
			var columns []string
			for _, field := range strings.Fields(entry.columns) {
				column, _, _ := strings.Cut(field, ":")
				columns = append(columns, column)
			}
			return columns, nil
		}
	}
	return nil, errors.New("merge reported an unknown table; inspect with Dolt")
}

func pullRows(ctx context.Context, q branchQuerier, query string, columns []string, args ...any) (out []map[string]*string, err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		cells, targets := make([]sql.NullString, len(columns)), make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := map[string]*string{}
		for i, column := range columns {
			row[column] = nil
			if cells[i].Valid {
				row[column] = &cells[i].String
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func pullProjection(columns []string, prefix string) string {
	fields := make([]string, len(columns))
	for i, column := range columns {
		fields[i] = "CAST(" + quoteIdentifier(prefix+column) + " AS CHAR)"
	}
	return strings.Join(fields, ", ")
}

func pullRow(ctx context.Context, q branchQuerier, table, hash, id string) (map[string]*string, error) {
	columns, err := pullColumns(table)
	if err != nil {
		return nil, err
	}
	from := quoteIdentifier(table)
	if hash != "" {
		if !transferHash.MatchString(hash) {
			return nil, errors.New("invalid immutable row revision")
		}
		from += " AS OF '" + hash + "'"
	}
	rows, err := pullRows(ctx, q, "SELECT "+pullProjection(columns, "")+" FROM "+from+" WHERE "+quoteIdentifier(columns[0])+" = ?", columns, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("conflict row identity is not unique")
	}
	return rows[0], nil
}

func pullConflictRow(ctx context.Context, tx *sql.Tx, table, id string, result TransferResult) (PullConflictRow, error) {
	row := PullConflictRow{ID: id}
	var err error
	for _, side := range []struct {
		hash  string
		image *map[string]*string
	}{
		{result.MergeBase, &row.Base},
		{result.LocalCommit, &row.Ours},
		{result.RemoteCommit, &row.Theirs},
	} {
		*side.image, err = pullRow(ctx, tx, table, side.hash, id)
		if err != nil {
			return row, err
		}
	}
	row.Merged, err = pullRow(ctx, tx, table, "", id)
	return row, err
}

// Dolt's revision-qualified blame view changes database context internally.
// Read it only after verified rollback, never during the merge transaction.
func fillPullBlame(ctx context.Context, conn *sql.Conn, result *TransferResult) error {
	for i := range result.Conflicts {
		conflict := &result.Conflicts[i]
		columns, err := pullColumns(conflict.Table)
		if err != nil {
			return err
		}
		for j := range conflict.Rows {
			row := &conflict.Rows[j]
			for _, side := range []struct {
				hash  string
				image map[string]*string
				blame **PullProvenance
			}{
				{result.MergeBase, row.Base, &row.BaseBlame},
				{result.LocalCommit, row.Ours, &row.OursBlame},
				{result.RemoteCommit, row.Theirs, &row.TheirsBlame},
			} {
				if side.image == nil {
					continue
				}
				provenance := &PullProvenance{}
				if err := conn.QueryRowContext(ctx, "SELECT commit FROM "+quoteIdentifier(DatabaseName+"/"+side.hash)+"."+quoteIdentifier("dolt_blame_"+conflict.Table)+" WHERE "+quoteIdentifier(columns[0])+" = ?", row.ID).Scan(&provenance.Commit); err != nil {
					return fmt.Errorf("cannot attribute conflict row in %s: %w", conflict.Table, err)
				}
				if !transferHash.MatchString(provenance.Commit) {
					return errors.New("conflict row has no immutable blame commit")
				}
				if err := conn.QueryRowContext(ctx, "SELECT committer, email, date, message FROM DOLT_LOG(?) WHERE commit_hash = ?", side.hash, provenance.Commit).
					Scan(&provenance.Author, &provenance.Email, &provenance.Date, &provenance.Message); err != nil {
					return fmt.Errorf("cannot read conflict blame history: %w", err)
				}
				if strings.TrimSpace(provenance.Author) == "" || strings.TrimSpace(provenance.Email) == "" {
					return errors.New("conflict row has unattributed commit provenance")
				}
				*side.blame = provenance
			}
		}
	}
	return nil
}

func pullConflicts(ctx context.Context, tx *sql.Tx, result TransferResult) ([]PullConflict, []ClearedViolation, error) {
	var schemaConflicts int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_schema_conflicts").Scan(&schemaConflicts); err != nil {
		return nil, nil, err
	}
	if schemaConflicts != 0 {
		return nil, nil, errors.New("schema conflicts require upgrade or repair with a compatible client")
	}
	data, err := surfaceSummary(ctx, tx, "dolt_conflicts", "num_conflicts")
	if err != nil {
		return nil, nil, err
	}
	var conflicts []PullConflict
	for _, table := range slices.Sorted(maps.Keys(data)) {
		columns, err := pullColumns(table)
		if err != nil {
			return nil, nil, err
		}
		if table == "meta" {
			return nil, nil, errors.New("metadata/schema conflicts require repair or upgrade with a compatible client")
		}
		var projection, fields []string
		for _, prefix := range []string{"base_", "our_", "their_"} {
			projection = append(projection, pullProjection(columns, prefix))
			for _, column := range columns {
				fields = append(fields, prefix+column)
			}
		}
		records, err := pullRows(ctx, tx, "SELECT "+strings.Join(projection, ", ")+" FROM "+quoteIdentifier("dolt_conflicts_"+table), fields)
		if err != nil {
			return nil, nil, err
		}
		if len(records) != data[table] {
			return nil, nil, errors.New("conflict summary and rows disagree")
		}
		touched, err := rowsTouchedByMerge(ctx, tx, table, columns[0], result.MergeBase, result.LocalCommit, result.RemoteCommit)
		if err != nil {
			return nil, nil, err
		}
		for _, record := range records {
			id := ""
			for _, prefix := range []string{"base_", "our_", "their_"} {
				if key := record[prefix+columns[0]]; key != nil {
					if id != "" && id != *key {
						return nil, nil, errors.New("identity-changing data conflict cannot be resolved by pull")
					}
					id = *key
				}
			}
			if _, known := touched[id]; !known || id == "" {
				return nil, nil, errors.New("merge reported an unattributed data conflict")
			}
			row, err := pullConflictRow(ctx, tx, table, id, result)
			if err != nil {
				return nil, nil, err
			}
			for side, image := range []map[string]*string{row.Base, row.Ours, row.Theirs} {
				prefix := []string{"base_", "our_", "their_"}[side]
				for _, column := range columns {
					if !samePullCell(record[prefix+column], image[column]) {
						return nil, nil, errors.New("conflict rows do not match the captured histories")
					}
				}
			}
			conflicts = append(conflicts, PullConflict{ID: table + ":row:" + id, Kind: "data", Table: table, Rows: []PullConflictRow{row}})
		}
	}
	violations, err := surfaceSummary(ctx, tx, "dolt_constraint_violations", "num_violations")
	if err != nil {
		return nil, nil, err
	}
	var cleared []ClearedViolation
	for _, table := range slices.Sorted(maps.Keys(violations)) {
		columns, err := pullColumns(table)
		if err != nil {
			return nil, nil, err
		}
		entry, err := verifyMergeViolations(ctx, tx, table, columns[0], result.MergeBase, result.LocalCommit, result.RemoteCommit)
		if err == nil {
			cleared = append(cleared, entry)
			continue
		}
		if table != "facts" || !errors.Is(err, errConstraintViolation) {
			return nil, nil, fmt.Errorf("unsupported or unattributed constraint conflict; inspect with Dolt: %w", err)
		}
		groups, err := pullFactConflicts(ctx, tx, result)
		if err != nil {
			return nil, nil, err
		}
		conflicts = append(conflicts, groups...)
	}
	slices.SortFunc(conflicts, func(a, b PullConflict) int { return strings.Compare(a.ID, b.ID) })
	return conflicts, cleared, nil
}

func pullFactConflicts(ctx context.Context, tx *sql.Tx, result TransferResult) ([]PullConflict, error) {
	representatives, err := pullRows(ctx, tx, "SELECT MIN(id) FROM facts WHERE live_key IS NOT NULL GROUP BY live_key HAVING COUNT(*) > 1 ORDER BY MIN(id)", []string{"id"})
	if err != nil {
		return nil, err
	}
	var conflicts []PullConflict
	for _, representative := range representatives {
		ids, err := pullRows(ctx, tx, "SELECT id FROM facts WHERE live_key = (SELECT live_key FROM facts WHERE id = ?) ORDER BY id", []string{"id"}, *representative["id"])
		if err != nil {
			return nil, err
		}
		conflict := PullConflict{ID: "facts:live-key:" + *representative["id"], Kind: "live-fact-key", Table: "facts"}
		for _, id := range ids {
			row, err := pullConflictRow(ctx, tx, "facts", *id["id"], result)
			if err != nil {
				return nil, err
			}
			conflict.Rows = append(conflict.Rows, row)
		}
		conflicts = append(conflicts, conflict)
	}
	return conflicts, nil
}

func samePullCell(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func clearPullViolations(ctx context.Context, tx *sql.Tx, result TransferResult) ([]ClearedViolation, error) {
	violations, err := surfaceSummary(ctx, tx, "dolt_constraint_violations", "num_violations")
	if err != nil {
		return nil, err
	}
	var cleared []ClearedViolation
	for _, table := range slices.Sorted(maps.Keys(violations)) {
		columns, err := pullColumns(table)
		if err != nil {
			return nil, err
		}
		entry, err := verifyMergeViolations(ctx, tx, table, columns[0], result.MergeBase, result.LocalCommit, result.RemoteCommit)
		if err != nil {
			return nil, err
		}
		cleared = append(cleared, entry)
	}
	return cleared, nil
}

func requirePullInvariants(ctx context.Context, tx *sql.Tx) error {
	for _, table := range slices.Sorted(maps.Keys(statusUniqueKeys)) {
		if err := requireConstraintHolds(ctx, tx, table, statusUniqueKeys[table].name); err != nil {
			return err
		}
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_chunks c LEFT JOIN documents d ON c.doc_id = d.id WHERE c.doc_id IS NOT NULL AND d.id IS NULL").Scan(&orphans); err != nil {
		return err
	}
	if orphans != 0 {
		return errors.New("merged document chunks have missing documents")
	}
	for _, table := range []string{"facts", "decisions"} {
		rows, err := pullRows(ctx, tx, "SELECT id, superseded_by FROM "+quoteIdentifier(table), []string{"id", "superseded_by"})
		if err != nil {
			return err
		}
		links := map[string]*string{}
		for _, row := range rows {
			links[*row["id"]] = row["superseded_by"]
		}
		checked := map[string]bool{}
		for id := range links {
			path := map[string]bool{}
			for current := id; !checked[current]; {
				link, exists := links[current]
				if !exists || path[current] {
					return fmt.Errorf("merged %s has a dangling or cyclic supersession link", table)
				}
				path[current] = true
				if link == nil {
					break
				}
				current = *link
			}
			for visited := range path {
				checked[visited] = true
			}
		}
	}
	return nil
}

func (s *Store) resolvePullConflicts(ctx context.Context, tx *sql.Tx, conflicts []PullConflict, choices []PullChoice) error {
	if len(conflicts) == 0 || len(choices) != len(conflicts) {
		return errors.New("resolution must contain exactly one choice for every displayed conflict")
	}
	byID := map[string]PullChoice{}
	for _, choice := range choices {
		if _, duplicate := byID[choice.Conflict]; duplicate {
			return errors.New("duplicate conflict choice")
		}
		byID[choice.Conflict] = choice
	}
	// Resolve final data images before applying the selected supersession links;
	// a whole-row choice must not silently undo the same merge's key repair.
	conflicts = slices.Clone(conflicts)
	slices.SortStableFunc(conflicts, func(a, b PullConflict) int { return strings.Compare(a.Kind, b.Kind) })
	for _, conflict := range conflicts {
		choice, ok := byID[conflict.ID]
		if !ok {
			return errors.New("incomplete or unknown conflict choices")
		}
		if conflict.Kind == "live-fact-key" {
			if choice.Take != "winner" && choice.Take != "manual" || choice.Reopen || choice.Take == "winner" && choice.Row != nil {
				return errors.New("live-fact-key choice requires take=winner or manual and one displayed winner ID")
			}
			index := slices.IndexFunc(conflict.Rows, func(row PullConflictRow) bool { return row.ID == choice.Winner })
			if index < 0 {
				return errors.New("fact winner is not a displayed offending row")
			}
			if err := s.checkDenyList([]string{choice.Winner}); err != nil {
				return err
			}
			for _, row := range conflict.Rows {
				current, err := pullRow(ctx, tx, "facts", "", row.ID)
				if err != nil {
					return err
				}
				if current == nil || !samePullCell(current["key"], row.Merged["key"]) {
					return errors.New("every fact in a reviewed live-key collision must retain its identity and row")
				}
				if row.ID == choice.Winner && current["superseded_by"] != nil {
					return errors.New("the selected durable fact winner must remain live")
				}
			}
			for _, row := range conflict.Rows {
				if row.ID != choice.Winner {
					if _, err := tx.ExecContext(ctx, "UPDATE facts SET superseded_by = ? WHERE id = ?", choice.Winner, row.ID); err != nil {
						return err
					}
				}
			}
			if choice.Take == "manual" {
				if choice.Row["superseded_by"] != nil {
					return errors.New("the selected durable fact winner must remain live")
				}
				if err := s.writePullRow(ctx, tx, conflict.Table, conflict.Rows[index], choice.Row, true); err != nil {
					return err
				}
			}
			continue
		}
		row := conflict.Rows[0]
		var final map[string]*string
		switch choice.Take {
		case "ours":
			final = row.Ours
		case "theirs":
			final = row.Theirs
		case "manual":
			final = choice.Row
		default:
			return errors.New("data conflict requires take=ours, theirs, or manual")
		}
		if choice.Winner != "" || choice.Take != "manual" && choice.Row != nil || choice.Reopen && conflict.Table != "tasks" {
			return errors.New("malformed conflict choice fields")
		}
		status := pullCell(final, "status")
		if conflict.Table == "tasks" && (pullCell(row.Ours, "status") == "done" || pullCell(row.Theirs, "status") == "done") && status != "done" && (!choice.Reopen || status != "open" && status != "blocked") {
			return errors.New("task completion must remain done unless the operator explicitly sets reopen=true")
		}
		if err := s.writePullRow(ctx, tx, conflict.Table, row, final, choice.Take == "manual"); err != nil {
			return err
		}
		columns, err := pullColumns(conflict.Table)
		if err != nil {
			return err
		}
		// SQL conflict resolution updates the chosen row, then deletes only its
		// reviewed record; native whole-table ours/theirs would lose other choices.
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+quoteIdentifier("dolt_conflicts_"+conflict.Table)+" WHERE COALESCE("+
			quoteIdentifier("base_"+columns[0])+", "+quoteIdentifier("our_"+columns[0])+", "+quoteIdentifier("their_"+columns[0])+") = ?", row.ID); err != nil {
			return err
		}
	}
	return nil
}

func pullCell(row map[string]*string, column string) string {
	if row[column] == nil {
		return ""
	}
	return *row[column]
}

func (s *Store) writePullRow(ctx context.Context, tx *sql.Tx, table string, shown PullConflictRow, final map[string]*string, manual bool) error {
	columns, err := pullColumns(table)
	if err != nil {
		return err
	}
	if final == nil {
		if manual {
			return errors.New("manual choice requires a complete final row")
		}
		if table == "documents" {
			var children int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM doc_chunks WHERE doc_id = ?", shown.ID).Scan(&children); err != nil {
				return err
			}
			if children != 0 {
				return errors.New("chosen document deletion would remove unrelated chunks; resolve with Dolt")
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM "+quoteIdentifier(table)+" WHERE "+quoteIdentifier(columns[0])+" = ?", shown.ID)
		return err
	}
	editable := slices.DeleteFunc(slices.Clone(columns), func(column string) bool { return table == "facts" && column == "live_key" })
	if manual {
		if len(final) != len(editable) {
			return errors.New("manual row must contain every writable column exactly once, including nulls, and omit generated live_key")
		}
		anchor := shown.Ours
		if anchor == nil {
			anchor = shown.Theirs
		}
		for _, column := range []string{columns[0], "key", "source", "actor", "actor_raw", "created_at", "decided_at", "ingested_at", "doc_id", "ord", "path", "target"} {
			if _, exists := anchor[column]; exists && !samePullCell(anchor[column], final[column]) {
				return fmt.Errorf("manual choice cannot change row identity or provenance column %s", column)
			}
		}
	}
	if pullCell(final, columns[0]) != shown.ID {
		return errors.New("choice cannot change the displayed row identity")
	}
	var names, placeholders, updates, text []string
	var args []any
	for _, column := range editable {
		value, present := final[column]
		if !present {
			return fmt.Errorf("final row is missing %s", column)
		}
		if value != nil && !samePullCell(value, shown.Merged[column]) {
			text = append(text, *value)
		}
		names, placeholders = append(names, quoteIdentifier(column)), append(placeholders, "?")
		if value == nil {
			args = append(args, nil)
		} else {
			args = append(args, *value)
		}
		if column != columns[0] {
			updates = append(updates, quoteIdentifier(column)+" = VALUES("+quoteIdentifier(column)+")")
		}
	}
	if err := s.checkDenyList(text); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+quoteIdentifier(table)+" ("+strings.Join(names, ", ")+") VALUES ("+
		strings.Join(placeholders, ", ")+") ON DUPLICATE KEY UPDATE "+strings.Join(updates, ", "), args...); err != nil {
		return fmt.Errorf("write chosen final row: %w", err)
	}
	actual, err := pullRow(ctx, tx, table, "", shown.ID)
	if err != nil {
		return err
	}
	for _, column := range editable {
		if !samePullCell(final[column], actual[column]) {
			return fmt.Errorf("final row value for %s was coerced or truncated; supply its exact SQL text representation", column)
		}
	}
	return nil
}
