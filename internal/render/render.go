// Package render prepares local Markdown views of one immutable Dolt main.
// It never opens a store, commits memory, or flushes pending session work.
package render

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

const Marker = "<!-- memdolt:rendered -->"

// Result reports confirmed effects even when a later step fails. Before #145's
// review correction it carried only file effects; the session wrapper now also
// supplies note-ID to commit-hash mappings for this call's flush. Run itself
// still writes no memory. Written means both replacements completed, not that
// the pair was atomically replaced.
type Result struct {
	SourceCommit  string            `json:"sourceCommit"`
	SchemaVersion int               `json:"schemaVersion"`
	GeneratedAt   time.Time         `json:"generatedAt"`
	OutputDir     string            `json:"outputDir"`
	WrittenFiles  []string          `json:"writtenFiles"`
	BackupFiles   []string          `json:"backupFiles"`
	NoteCommits   map[string]string `json:"noteCommits,omitempty"`
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
}

// NoteCommitError preserves this invocation's confirmed notes through later
// rendering, close or output failures. An empty map makes no durability claim.
func (r Result) NoteCommitError(err error) error {
	if err != nil && len(r.NoteCommits) != 0 {
		return fmt.Errorf("confirmed note commits %v; inspect `memdolt note list` and Dolt history before retrying: %w", r.NoteCommits, err)
	}
	return err
}

// Query is the existing owning-store read seam, not a second database opener.
type Query interface {
	Query(context.Context, string, ...any) (store.Rows, error)
}

type row map[string]string

type snapshot struct {
	commit string
	schema int
	at     time.Time
	tables map[string][]row
}

// Run performs the complete operation inside the owner. Every table read and
// the history walk use the captured hash, including when main subsequently
// moves. The filesystem lock refuses overlapping output generations.
func Run(ctx context.Context, baseDir string, source Query) (Result, error) {
	return run(ctx, baseDir, source, fileHooks{})
}

func run(ctx context.Context, baseDir string, source Query, hooks fileHooks) (result Result, err error) {
	result.Status = "refused"
	result.WrittenFiles, result.BackupFiles = []string{}, []string{}
	defer func() {
		if err != nil {
			err = fmt.Errorf("render: inspect reported outputs and backups before retrying: %w", err)
			result.Error = err.Error()
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cfg, err := loadConfig(baseDir)
	if err != nil {
		return result, err
	}
	result.OutputDir = cfg.output
	snap, err := capture(ctx, source)
	if err != nil {
		return result, err
	}
	result.SourceCommit, result.SchemaVersion, result.GeneratedAt = snap.commit, snap.schema, snap.at
	project, ledger, err := formatSnapshot(snap, cfg)
	if err != nil {
		return result, err
	}
	err = writeFiles(ctx, cfg, [2]string{project, ledger}, &result, hooks)
	return result, err
}

func capture(ctx context.Context, source Query) (snapshot, error) {
	s := snapshot{at: time.Now().UTC(), tables: make(map[string][]row)}
	heads, err := readRows(ctx, source, "SELECT hash FROM dolt_branches WHERE name = ?", "main")
	if err != nil {
		return s, fmt.Errorf("capture committed main: %w", err)
	}
	if len(heads) != 1 || !validHash(heads[0]["hash"]) {
		return s, errors.New("main must identify exactly one immutable Dolt commit")
	}
	s.commit = heads[0]["hash"]
	// The pinned driver panics while preparing AS OF ?. Its revision clause
	// needs a literal, constructed only after the exact Dolt hash grammar check.
	asOf := " AS OF '" + s.commit + "'"
	versions, err := readRows(ctx, source, "SELECT v FROM meta"+asOf+" WHERE k = ?", "schema_version")
	if err != nil {
		return s, fmt.Errorf("read committed schema: %w", err)
	}
	if len(versions) != 1 {
		return s, errors.New("missing committed schema version; explicitly run `memdolt init`")
	}
	s.schema, err = strconv.Atoi(versions[0]["v"])
	if err != nil || s.schema != store.LatestSchemaVersion() {
		return s, errors.New("unsupported committed schema; explicitly initialize or upgrade with a compatible memdolt binary")
	}
	for _, table := range []struct{ name, columns, order string }{
		{"project_state", "id, body, actor, actor_raw, created_at", "created_at DESC, id DESC LIMIT 1"},
		{"project_arch", "id, body, actor, actor_raw, created_at", "created_at DESC, id DESC LIMIT 1"},
		{"session_notes", "id, text, actor, actor_raw, created_at, session_id, agent_id, provider_id, model_id, variant", "created_at DESC, id DESC LIMIT 10"},
		{"decisions", "id, title, rationale, summary, alternatives_rejected, evidence, status, source, decided_at, superseded_by", "decided_at DESC, id DESC"},
		{"tasks", "id, title, status, notes, created_at, updated_at", "CASE status WHEN 'open' THEN 0 WHEN 'blocked' THEN 1 ELSE 2 END, updated_at DESC, id DESC"},
		{"facts", "id, `key`, value, source, kind, evidence, verified_at, created_at, superseded_by", "`key`, created_at, id"},
	} {
		// Identifiers and ordering are solely the fixed table above. No caller
		// can supply SQL, a branch, or an AS OF clause.
		rows, err := readRows(ctx, source, "SELECT "+table.columns+" FROM "+table.name+asOf+" ORDER BY "+table.order)
		if err != nil {
			return s, fmt.Errorf("read committed %s: %w", table.name, err)
		}
		s.tables[table.name] = rows
	}
	s.tables["activity"], err = readRows(ctx, source,
		"SELECT commit_hash, committer, email, date, message FROM DOLT_LOG(?) WHERE date >= ? ORDER BY date DESC, commit_hash LIMIT 50",
		s.commit, s.at.AddDate(0, 0, -30))
	if err != nil {
		return s, fmt.Errorf("read committed history: %w", err)
	}
	return s, nil
}

func validHash(hash string) bool {
	if len(hash) != 32 {
		return false
	}
	for _, ch := range hash {
		if ch < '0' || ch > '9' && ch < 'a' || ch > 'v' {
			return false
		}
	}
	return true
}

func readRows(ctx context.Context, source Query, query string, args ...any) (out []row, err error) {
	rows, err := source.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		values, dest := make([]sql.NullString, len(columns)), make([]any, len(columns))
		for i := range dest {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		entry := row{}
		for i, name := range columns {
			if values[i].Valid {
				entry[name] = values[i].String
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func formatSnapshot(s snapshot, cfg config) (string, string, error) {
	header := fmt.Sprintf("%s\n<!-- DO NOT EDIT. Generated from committed memdolt main. -->\n<!-- Use memdolt memory commands, then `memdolt render`. -->\n<!-- Source schema v%d; main commit %s; generated at %s. -->\n\n",
		Marker, s.schema, s.commit, s.at.Format(time.RFC3339Nano))
	var project, ledger strings.Builder
	project.WriteString(header)
	fmt.Fprintf(&project, "# %s\n\n", cfg.project)
	for _, section := range []struct{ table, heading, command string }{
		{"project_state", "Currently building", "state"}, {"project_arch", "Architecture", "arch"},
	} {
		fmt.Fprintf(&project, "## %s\n\n", section.heading)
		if rows := s.tables[section.table]; len(rows) != 0 {
			n := rows[0]
			project.WriteString(n["body"] + "\n\n")
			fmt.Fprintf(&project, "_Last updated %s by %s; %s._\n\n", inline(n["created_at"]), inline(n["actor"]), n["id"])
			field(&project, n, "actor_raw", "Raw actor")
		} else {
			fmt.Fprintf(&project, "_No %s recorded. Use `memdolt %s set <body>` to populate._\n\n", section.table, section.command)
		}
	}
	project.WriteString("## Recent session notes\n\n")
	if len(s.tables["session_notes"]) == 0 {
		project.WriteString("_No session notes recorded._\n\n")
	}
	for _, n := range s.tables["session_notes"] {
		fmt.Fprintf(&project, "### %s — %s (%s)\n\n", n["id"], inline(n["created_at"]), inline(n["actor"]))
		project.WriteString(n["text"] + "\n\n")
		for _, key := range []string{"actor_raw", "session_id", "agent_id", "provider_id", "model_id", "variant"} {
			field(&project, n, key, key)
		}
	}
	ledger.WriteString(header)
	fmt.Fprintf(&ledger, "# %s — Ledger\n\n## Decisions\n\n", cfg.project)
	if len(s.tables["decisions"]) == 0 {
		ledger.WriteString("_No decisions recorded._\n\n")
	}
	for _, d := range s.tables["decisions"] {
		fmt.Fprintf(&ledger, "### %s — %s\n\n", d["id"], inline(d["title"]))
		fmt.Fprintf(&ledger, "**Status:** %s • **Decided:** %s • **Source:** %s\n\n", inline(d["status"]), inline(d["decided_at"]), inline(d["source"]))
		field(&ledger, d, "superseded_by", "Superseded by (decision ULID)")
		body(&ledger, d["rationale"], "No rationale recorded.")
		field(&ledger, d, "summary", "Summary")
		field(&ledger, d, "alternatives_rejected", "Alternatives rejected")
		field(&ledger, d, "evidence", "Evidence")
		ledger.WriteString("---\n\n")
	}
	ledger.WriteString("## Backlog\n\n")
	if len(s.tables["tasks"]) == 0 {
		ledger.WriteString("_No tasks recorded._\n\n")
	} else {
		ledger.WriteString("_Open first, then blocked, then done; newest updates first within each status._\n\n")
	}
	for _, task := range s.tables["tasks"] {
		fmt.Fprintf(&ledger, "### %s — %s\n\n", task["id"], inline(task["title"]))
		fmt.Fprintf(&ledger, "**Status:** %s • **Updated:** %s • **Created:** %s\n\n", inline(task["status"]), inline(task["updated_at"]), inline(task["created_at"]))
		body(&ledger, task["notes"], "No notes.")
		ledger.WriteString("---\n\n")
	}
	ledger.WriteString("## Facts\n\n")
	if len(s.tables["facts"]) == 0 {
		ledger.WriteString("_No facts recorded._\n\n")
	}
	for _, fact := range s.tables["facts"] {
		stale := true
		if stamp, ok := fact["verified_at"]; ok {
			verified, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				return "", "", fmt.Errorf("invalid verification timestamp for fact %s: %w", fact["id"], err)
			}
			// Match recall's day comparison without importing retrieval/inference
			// into the store or overflowing a duration for large configured horizons.
			stale = s.at.Sub(verified).Hours()/24 > float64(cfg.staleDays)
		}
		fmt.Fprintf(&ledger, "### %s — %s\n\n", fact["id"], inline(fact["key"]))
		ledger.WriteString(fact["value"] + "\n\n")
		fmt.Fprintf(&ledger, "**Source:** %s • **Created:** %s • **Stale:** %t\n\n", inline(fact["source"]), inline(fact["created_at"]), stale)
		if _, ok := fact["verified_at"]; !ok {
			ledger.WriteString("**Verified:** never\n\n")
		}
		field(&ledger, fact, "verified_at", "Verified")
		field(&ledger, fact, "superseded_by", "Superseded by (fact ULID)")
		field(&ledger, fact, "kind", "Kind")
		field(&ledger, fact, "evidence", "Evidence")
		ledger.WriteString("---\n\n")
	}
	ledger.WriteString("## Recent activity (last 30 days)\n\n_Up to 50 real commits reachable from the source main, newest first._\n\n")
	if len(s.tables["activity"]) == 0 {
		ledger.WriteString("_No commit activity in window._\n")
	}
	for _, commit := range s.tables["activity"] {
		fmt.Fprintf(&ledger, "### %s\n\n**When:** %s • **Actor:** %s • **Email:** %s\n\n", commit["commit_hash"], inline(commit["date"]), inline(commit["committer"]), inline(commit["email"]))
		ledger.WriteString(commit["message"] + "\n\n")
	}
	return project.String(), ledger.String(), nil
}

// Multiline content stays verbatim in its own block; only heading/metadata
// line breaks become Markdown breaks. No body or provenance is summarized.
func inline(s string) string {
	return strings.NewReplacer("\r\n", "<br>", "\n", "<br>", "\r", "<br>").Replace(s)
}

func field(out *strings.Builder, entry row, key, label string) {
	if value, ok := entry[key]; ok {
		fmt.Fprintf(out, "**%s:**\n\n%s\n\n", label, value)
	}
}

func body(out *strings.Builder, text, empty string) {
	if text == "" {
		fmt.Fprintf(out, "_%s_\n\n", empty)
	} else {
		out.WriteString(text + "\n\n")
	}
}
