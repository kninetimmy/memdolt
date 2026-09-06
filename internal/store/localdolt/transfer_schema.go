package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kninetimmy/memdolt/internal/denylist"
	"github.com/kninetimmy/memdolt/internal/store"
)

// transferTables is the maintained transfer contract for schema v4. It lists
// every application column and its type; ! means NOT NULL, the first column is
// the single-column primary key. Unknown tables/columns/types fail closed.
// Text names every persisted user-authored string/provenance column, including
// IDs and metadata. Dates/counters and the derived live_key are not prose.
// This is a column-shape check, not a full index/constraint or history audit.
var transferTables = []struct{ name, columns, text string }{
	{"facts", "id:char(26)! key:varchar(255) value:text source:varchar(64) kind:varchar(64) evidence:varchar(1024) verified_at:datetime created_at:datetime superseded_by:char(26) live_key:varchar(255)", "id key value source kind evidence superseded_by"},
	{"decisions", "id:char(26)! title:varchar(512) rationale:text summary:text alternatives_rejected:text evidence:varchar(1024) status:enum('active','superseded','draft') source:varchar(64) decided_at:datetime superseded_by:char(26)", "id title rationale summary alternatives_rejected evidence status source superseded_by"},
	{"tasks", "id:char(26)! title:varchar(512) status:enum('open','done','blocked') notes:text created_at:datetime updated_at:datetime", "id title status notes"},
	{"session_notes", "id:char(26)! actor:varchar(64) actor_raw:varchar(255) text:text created_at:datetime session_id:varchar(255) agent_id:varchar(255) provider_id:varchar(255) model_id:varchar(255) variant:varchar(255)", "id actor actor_raw text session_id agent_id provider_id model_id variant"},
	{"commands", "kind:enum('build','test','run','lint','other')! cmdline:text last_exit_code:int last_run_at:datetime success_count:int fail_count:int", "kind cmdline"},
	{"project_state", "id:char(26)! body:text actor:varchar(64) actor_raw:varchar(255) created_at:datetime", "id body actor actor_raw"},
	{"project_arch", "id:char(26)! body:text actor:varchar(64) actor_raw:varchar(255) created_at:datetime", "id body actor actor_raw"},
	{"documents", "id:char(26)! path:varchar(1024) title:varchar(512) content_hash:char(64) byte_len:bigint source:varchar(64) ingested_at:datetime", "id path title content_hash source"},
	{"doc_chunks", "id:char(26)! doc_id:char(26) ord:int heading_path:varchar(1024) body:text", "id doc_id heading_path body"},
	{"meta", "k:varchar(64)! v:text", "k v"},
	{"proposals", "id:char(26)! kind:enum('fact','decision','supersede') rationale:text! actor:varchar(64)! created_at:datetime target:enum('repo','global')!", "id kind rationale actor target"},
}

func validateTransferSchema(ctx context.Context, conn *sql.Conn, hash string) error {
	// The pinned planbuilder panics on AS OF ?. Like proposalBranchOf, validate
	// before constructing the unavoidable revision identifier; values stay bound.
	if !transferHash.MatchString(hash) {
		return errors.New("invalid immutable Dolt commit hash")
	}
	database := quoteIdentifier(DatabaseName + "/" + hash)
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT CAST(v AS CHAR) FROM "+database+".meta WHERE k = ?", store.SchemaVersionKey).Scan(&version); err != nil {
		return errors.New("missing or malformed committed meta.schema_version")
	}
	if version != fmt.Sprint(store.LatestSchemaVersion()) {
		return fmt.Errorf("committed meta.schema_version must be exactly %d; use a compatible initialized memdolt store", store.LatestSchemaVersion())
	}
	rows, err := conn.QueryContext(ctx, "SHOW TABLES FROM "+database)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return errors.Join(err, rows.Close())
		}
		names[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(names) != len(transferTables) {
		return errors.New("unsupported application table set in committed store")
	}
	for _, table := range transferTables {
		if !names[table.name] {
			return fmt.Errorf("committed store lacks %s", table.name)
		}
		if err := transferColumnShape(ctx, conn, database, table.name, table.columns); err != nil {
			return err
		}
	}
	return nil
}

func transferColumnShape(ctx context.Context, conn *sql.Conn, database, table, spec string) (err error) {
	expected := map[string]string{}
	fields := strings.Fields(spec)
	primary, _, _ := strings.Cut(fields[0], ":")
	for _, field := range fields {
		name, shape, _ := strings.Cut(field, ":")
		expected[name] = shape
	}
	rows, err := conn.QueryContext(ctx, "SHOW COLUMNS FROM "+database+"."+quoteIdentifier(table))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name, kind, nullable, key, extra string
		var def sql.NullString
		if err := rows.Scan(&name, &kind, &nullable, &key, &def, &extra); err != nil {
			return err
		}
		if nullable == "NO" {
			kind += "!"
		}
		if want, ok := expected[name]; !ok || kind != want || (key == "PRI") != (name == primary) {
			return fmt.Errorf("unsupported committed column shape in %s", table)
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) != 0 {
		return fmt.Errorf("missing committed columns in %s", table)
	}
	return nil
}

func (s *Store) scanTransfer(ctx context.Context, conn *sql.Conn, from, to string) error {
	list, err := denylist.Load(s.paths.ConfigFile())
	if err != nil {
		return fmt.Errorf("read transfer deny-list: %w", err)
	}
	for _, table := range transferTables {
		columns := strings.Fields(table.text)
		if from != "" {
			changes, err := tableChanges(ctx, conn, table.name, from, to)
			if err != nil {
				return err
			}
			for _, change := range changes {
				for _, column := range columns {
					value, present := change.To[column]
					before, hadBefore := change.From[column]
					if present && (!hadBefore || value != before) {
						if err := list.Check(value); err != nil {
							return err
						}
					}
				}
			}
			continue
		}
		for i, column := range columns {
			// CAST also avoids the pinned driver's NULL-enum row conversion panic.
			columns[i] = "CAST(" + quoteIdentifier(column) + " AS CHAR)"
		}
		// to was validated by validateTransferSchema before this scan.
		query := "SELECT " + strings.Join(columns, ", ") + " FROM " + quoteIdentifier(DatabaseName+"/"+to) + "." + quoteIdentifier(table.name)
		if err := scanTransferRows(ctx, conn, list, query, len(columns)); err != nil {
			return err
		}
	}
	return nil
}

func scanTransferRows(ctx context.Context, conn *sql.Conn, list *denylist.List, query string, width int) (err error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	cells := make([]sql.NullString, width)
	targets := make([]any, width)
	for i := range cells {
		targets[i] = &cells[i]
	}
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		for _, cell := range cells {
			if cell.Valid {
				if err := list.Check(cell.String); err != nil {
					return err
				}
			}
		}
	}
	return rows.Err()
}
