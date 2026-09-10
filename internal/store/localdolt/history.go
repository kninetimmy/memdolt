package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/store"
)

// HistoryOptions selects repository memory, never arbitrary tables or refs.
// Pointers distinguish omitted selections from explicitly empty ones.
type HistoryOptions struct {
	Subject string  `json:"subject"`
	ID      *string `json:"id,omitempty"`
	AsOf    *string `json:"asOf,omitempty"`
	Limit   int     `json:"limit"`
}

func (o HistoryOptions) Validate() error {
	if o.Limit < 1 || o.Limit > store.DefaultMaxRows {
		return fmt.Errorf("history limit must be 1 through %d", store.DefaultMaxRows)
	}
	switch o.Subject {
	case "fact", "decision":
		if o.ID == nil || !utf8.ValidString(*o.ID) || strings.TrimSpace(*o.ID) == "" || utf8.RuneCountInString(*o.ID) > 26 {
			return errors.New("history fact/decision requires one explicit row id of 1 through 26 UTF-8 characters")
		}
	case "state", "arch":
		if o.ID != nil {
			return errors.New("history state/arch takes no row id")
		}
	default:
		return errors.New("history subject must be fact, decision, state or arch")
	}
	if o.AsOf != nil && !transferHash.MatchString(*o.AsOf) {
		return errors.New("history as-of requires a full 32-character Dolt commit hash")
	}
	return nil
}

// HistoryCommit keeps Dolt's author separate from its committer. Dates here
// describe native commits; stored source/actor/times remain in the row images.
type HistoryCommit struct {
	Hash           string     `json:"hash"`
	Author         *string    `json:"author"`
	AuthorEmail    *string    `json:"authorEmail"`
	AuthorDate     *time.Time `json:"authorDate"`
	Committer      *string    `json:"committer"`
	CommitterEmail *string    `json:"committerEmail"`
	Date           *time.Time `json:"date"`
	Message        *string    `json:"message"`
}

// HistoryChange is a native parent-to-commit difference. Merges can have a
// difference against each parent; Parent identifies that evidence explicitly.
type HistoryChange struct {
	Commit HistoryCommit      `json:"commit"`
	Parent string             `json:"parent"`
	Type   string             `json:"type"`
	From   map[string]*string `json:"from"`
	To     map[string]*string `json:"to"`
}

type HistoryResult struct {
	Subject    string             `json:"subject"`
	ID         *string            `json:"id,omitempty"`
	MainCommit string             `json:"mainCommit"`
	Revision   string             `json:"revision"`
	Current    map[string]*string `json:"current"`
	Blame      *HistoryCommit     `json:"blame"`
	Changes    []HistoryChange    `json:"changes"`
}

type historyLogEntry struct {
	HistoryCommit
	parents []string
}

// History captures main once and uses only immutable members of its native
// ancestry. It neither requires nor changes clean working/staged roots.
func (s *Store) History(ctx context.Context, opts HistoryOptions) (result HistoryResult, err error) {
	return s.history(ctx, opts, nil)
}

func (s *Store) history(ctx context.Context, opts HistoryOptions, afterBlame func()) (result HistoryResult, err error) {
	if err := opts.Validate(); err != nil {
		return result, err
	}
	if s.globalRepo != nil {
		return result, errors.New("history supports repository memory only")
	}
	// ponytail: reuse the cooperating-write lock for the memory-sized native
	// history walk; narrow the capture lock if long histories delay writers.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	conn, main, err := s.initializedMainConn(ctx, "history")
	if err != nil {
		return result, err
	}
	defer func() {
		// Revision-qualified blame changes Dolt's session database internally.
		// Retire this read session, including on cancellation/refusal, so it
		// can never lend historical context to an ordinary read or write.
		discardConn(conn)
		// The embedded driver can wrap cancellation as a native SQL error.
		err = errors.Join(err, ctx.Err())
		if err != nil {
			result = HistoryResult{} // no partial contents on a refused capture
		}
	}()
	revision := main
	if opts.AsOf != nil {
		revision = *opts.AsOf
		var found string
		if err := conn.QueryRowContext(ctx, "SELECT commit_hash FROM DOLT_LOG(?) WHERE commit_hash = ?", main, revision).Scan(&found); err != nil {
			return result, fmt.Errorf("history revision is not verified ancestry of captured main: %w", err)
		}
	}
	log, err := historyLog(ctx, conn, revision)
	if err != nil {
		return result, err
	}
	table := map[string]string{"fact": "facts", "decision": "decisions", "state": "project_state", "arch": "project_arch"}[opts.Subject]
	commits := make(map[string]HistoryCommit, len(log))
	present := make(map[string]bool, len(log))
	// Check every reached historical column shape before native diff can coerce
	// or silently skip an incompatible old schema. An absent bootstrap table is
	// an empty side of a diff, not a reason to initialize or migrate anything.
	for _, entry := range log {
		commits[entry.Hash] = entry.HistoryCommit
		present[entry.Hash], err = historySchema(ctx, conn, entry.Hash, table)
		if err != nil {
			return result, fmt.Errorf("unsupported history schema at %s: %w", entry.Hash, err)
		}
	}
	if !present[revision] {
		return result, errors.New("unsupported history revision: selected memory table is absent")
	}
	result = HistoryResult{Subject: opts.Subject, ID: opts.ID, MainCommit: main, Revision: revision, Changes: []HistoryChange{}}
	columns, err := pullColumns(table)
	if err != nil {
		return result, err
	}
	query := "SELECT " + pullProjection(columns, "") + " FROM " + quoteIdentifier(DatabaseName+"/"+revision) + "." + quoteIdentifier(table)
	var args []any
	if opts.ID != nil {
		query += " WHERE BINARY id = ?"
		args = append(args, *opts.ID)
	} else {
		query += " ORDER BY created_at DESC, id DESC LIMIT 1"
	}
	current, err := pullRows(ctx, conn, query, columns, args...)
	if err != nil {
		return result, err
	}
	if len(current) > 1 {
		return result, errors.New("history row identity is not unique")
	}
	if len(current) == 1 {
		result.Current = current[0]
		if err := historyText(result.Current); err != nil {
			return result, err
		}
		var blamed string
		if err := conn.QueryRowContext(ctx, "SELECT commit FROM "+quoteIdentifier(DatabaseName+"/"+revision)+"."+quoteIdentifier("dolt_blame_"+table)+" WHERE BINARY id = ?", *result.Current["id"]).Scan(&blamed); err != nil {
			return result, fmt.Errorf("read native history blame: %w", err)
		}
		commit, ok := commits[blamed]
		if !ok {
			return result, errors.New("history blame is outside selected revision ancestry")
		}
		result.Blame = &commit
	}
	if afterBlame != nil {
		afterBlame()
	}
	for _, entry := range log {
		for _, parent := range entry.parents {
			if _, ok := commits[parent]; !ok {
				return result, errors.New("history parent is outside selected revision ancestry")
			}
			if !present[entry.Hash] && !present[parent] {
				continue
			}
			changes, err := historyDiff(ctx, conn, table, columns, revision, parent, entry.HistoryCommit, opts.ID, opts.Limit-len(result.Changes))
			if err != nil {
				return result, err
			}
			result.Changes = append(result.Changes, changes...)
			if len(result.Changes) == opts.Limit {
				return result, nil
			}
		}
	}
	return result, nil
}

func historyLog(ctx context.Context, conn *sql.Conn, revision string) (entries []historyLogEntry, err error) {
	// Preserve the native log iterator's ordering (including merge ancestry),
	// rather than sorting by imported row timestamps or inventing a DAG walker.
	rows, err := conn.QueryContext(ctx, "SELECT commit_hash, author, author_email, author_date, committer, email, date, message, parents FROM DOLT_LOG(?, '--parents')", revision)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var entry historyLogEntry
		var parents string
		if err := rows.Scan(&entry.Hash, &entry.Author, &entry.AuthorEmail, &entry.AuthorDate, &entry.Committer, &entry.CommitterEmail, &entry.Date, &entry.Message, &parents); err != nil {
			return nil, err
		}
		if !transferHash.MatchString(entry.Hash) {
			return nil, errors.New("history log returned an invalid native commit")
		}
		if err := historyText(map[string]*string{"author": entry.Author, "authorEmail": entry.AuthorEmail, "committer": entry.Committer, "committerEmail": entry.CommitterEmail, "message": entry.Message}); err != nil {
			return nil, err
		}
		if parents != "" {
			entry.parents = strings.Split(parents, ", ")
			for _, parent := range entry.parents {
				if !transferHash.MatchString(parent) {
					return nil, errors.New("history log returned an invalid native parent")
				}
			}
			// Native parent order is stable; it also identifies merge edges.
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func historySchema(ctx context.Context, conn *sql.Conn, hash, table string) (bool, error) {
	database := quoteIdentifier(DatabaseName + "/" + hash)
	rows, err := conn.QueryContext(ctx, "SHOW TABLES FROM "+database)
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, errors.Join(err, rows.Close())
		}
		found = found || name == table
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || !found {
		return found, err
	}
	for _, entry := range transferTables {
		if entry.name == table {
			if err := transferColumnShape(ctx, conn, database, table, entry.columns); err != nil {
				return false, err
			}
		}
	}
	if table == "facts" {
		if err := transferLiveKey(ctx, conn, database); err != nil {
			return false, err
		}
	}
	return true, nil
}

func historyDiff(ctx context.Context, conn *sql.Conn, table string, columns []string, revision, parent string, commit HistoryCommit, id *string, limit int) ([]HistoryChange, error) {
	var names, projection []string
	for _, prefix := range []string{"from_", "to_"} {
		for _, column := range columns {
			name := prefix + column
			names = append(names, name)
			// Materialize lazy TEXT before ORDER BY closes the native iterator,
			// exactly as repoDiffRows does; CAST preserves nullable ENUMs.
			projection = append(projection, "CONCAT(CAST("+quoteIdentifier(name)+" AS CHAR), '')")
		}
	}
	names, projection = append(names, "diff_type"), append(projection, "diff_type")
	query := "SELECT " + strings.Join(projection, ", ") + " FROM " + quoteIdentifier(DatabaseName+"/"+revision) + "." + quoteIdentifier("dolt_commit_diff_"+table) + " WHERE from_commit = ? AND to_commit = ?"
	args := []any{parent, commit.Hash}
	if id != nil {
		query += " AND (BINARY from_id = ? OR BINARY to_id = ?)"
		args = append(args, *id, *id)
	}
	query += " ORDER BY COALESCE(from_id, to_id), diff_type LIMIT ?"
	args = append(args, limit)
	rows, err := pullRows(ctx, conn, query, names, args...)
	if err != nil {
		return nil, fmt.Errorf("read native history diff: %w", err)
	}
	// Dolt can return no rows with only a warning when historical primary-key
	// identities cannot be diffed, even if their visible column types match.
	warnings, err := conn.QueryContext(ctx, "SHOW WARNINGS")
	if err != nil {
		return nil, err
	}
	warned := warnings.Next()
	if err := errors.Join(warnings.Err(), warnings.Close()); err != nil {
		return nil, err
	}
	if warned {
		return nil, errors.New("unsupported native history difference: Dolt reported a diff warning")
	}
	changes := make([]HistoryChange, 0, len(rows))
	for _, row := range rows {
		if err := historyText(row); err != nil {
			return nil, err
		}
		change := HistoryChange{Commit: commit, Parent: parent}
		switch interopValue(row, "diff_type") {
		case "added":
			change.Type, change.To = "added", map[string]*string{}
		case "removed":
			change.Type, change.From = "deleted", map[string]*string{}
		case "modified":
			change.Type, change.From, change.To = "modified", map[string]*string{}, map[string]*string{}
		default:
			return nil, errors.New("unsupported native history difference classification")
		}
		for _, column := range columns {
			if change.From != nil {
				change.From[column] = row["from_"+column]
			}
			if change.To != nil {
				change.To[column] = row["to_"+column]
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func historyText(row map[string]*string) error {
	for column, value := range row {
		if value != nil && !utf8.ValidString(*value) {
			return fmt.Errorf("unsupported history text in %s: invalid UTF-8 would require lossy output", column)
		}
	}
	return nil
}
