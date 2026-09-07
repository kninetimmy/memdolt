package localdolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store"
)

type ExportMemoryOptions struct {
	File string `json:"file"`
}

type ImportMemoryOptions struct {
	File       string `json:"file"`
	FromMemhub bool   `json:"from_memhub"`
}

// IdentityMap describes the complete mapping, not a claim that every pending
// proposal exists. MainCommit and CreatedProposals are the confirmed effects;
// RemainingProposals lacks confirmed completion; a failed attempt can leave
// inspected branch residue. No automatic replay is safe.
type InteropResult struct {
	Operation          string            `json:"operation"`
	Status             string            `json:"status"`
	File               string            `json:"file"`
	Written            bool              `json:"written"`
	SourceDigest       string            `json:"source_digest,omitempty"`
	SourceCommit       string            `json:"source_commit,omitempty"`
	MainCommit         string            `json:"main_commit,omitempty"`
	Counts             map[string]int    `json:"counts,omitempty"`
	IdentityMap        []InteropIdentity `json:"identity_map,omitempty"`
	CreatedProposals   []StagedProposal  `json:"created_proposals"`
	RemainingProposals []string          `json:"remaining_proposals"`
	ProposalResidue    *InteropResidue   `json:"proposal_residue,omitempty"`
	RetainedDocuments  int               `json:"retained_documents"`
	RetainedDocChunks  int               `json:"retained_doc_chunks"`
	Guidance           string            `json:"guidance"`
	Error              string            `json:"error,omitempty"`
}

// A failed staging attempt can retain a branch without a complete proposal.
// Head is its inspected branch head, not a claim that its payload committed.
type InteropResidue struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

const interopGuidance = "Inspect `memdolt repo status --local`, `memdolt review list`, committed rows and reported hashes before any retry. Keep the original bundle. Imports require a fresh initialized target; no wipe or automatic replay. Documents are excluded; select sources explicitly with `memdolt doc add`. Rebuild with `memdolt index rebuild` and verify `memdolt eval retrieval`."

type interopHooks struct {
	afterCapture   func()
	beforeMain     func() error
	afterMain      func() error
	beforeProposal func(int) error
	finalizeMain   func(*sql.Tx) error
	finalizeStage  func(*sql.Tx) error
}

func (s *Store) ExportMemory(ctx context.Context, opts ExportMemoryOptions) (InteropResult, error) {
	return s.exportMemory(ctx, opts, interopHooks{})
}

func (s *Store) exportMemory(ctx context.Context, opts ExportMemoryOptions, hooks interopHooks) (result InteropResult, err error) {
	result = InteropResult{Operation: "export", Status: "refused", File: opts.File, CreatedProposals: []StagedProposal{}, RemainingProposals: []string{}, Guidance: interopGuidance}
	if err := render.BundlePath(opts.File); err != nil {
		return result, err
	}
	// ponytail: reuse the whole-operation mutation lock, including publication;
	// split capture from publication only if large exports delay normal writes.
	// Foreign Dolt sessions do not participate; every read below pins a hash.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	db, err := s.handle()
	if err != nil {
		return result, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, err
	}
	bundle, err := captureInterop(ctx, conn, hooks)
	if err == nil {
		err = protectInteropDocumentPath(ctx, conn, bundle.MainCommit, opts.File)
	}
	if err = errors.Join(err, conn.Close()); err != nil {
		return result, err // close failure precedes any output preparation
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return result, err
	}
	data = append(data, '\n')
	result.SourceCommit, result.SourceDigest, result.Counts = bundle.MainCommit, fmt.Sprintf("sha256:%x", sha256.Sum256(data)), interopCounts(bundle)
	metadata, err := s.documentConfigRoot()
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, metadata.Close()) }()
	result.Written, err = render.PublishBundle(ctx, opts.File, data, func(info os.FileInfo) error {
		return checkDocumentOwnerFile(metadata, info)
	}, func(previous []byte) error {
		var old InteropBundle
		if err := decodeInteropJSON(previous, &old); err != nil {
			return err
		}
		return validateInteropHeader(old)
	})
	if result.Written {
		result.Status = "exported"
	}
	return result, err
}

func captureInterop(ctx context.Context, conn *sql.Conn, hooks interopHooks) (InteropBundle, error) {
	bundle := InteropBundle{Version: InteropVersion, SchemaVersion: store.LatestSchemaVersion(), ExportedAt: time.Now().UTC().Format(time.RFC3339Nano), Tables: map[string][]InteropRow{}, Proposals: []InteropProposal{}}
	// Capture main and all proposal heads in one branch-table read. Neither
	// hydration, reachability nor payload reads consult a moving branch again.
	rows, err := conn.QueryContext(ctx, "SELECT name, hash FROM dolt_branches ORDER BY name")
	if err != nil {
		return bundle, err
	}
	var branches []branchRecord
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return bundle, errors.Join(err, rows.Close())
		}
		if name == MainBranch {
			bundle.MainCommit = hash
		} else if id, ok := strings.CutPrefix(name, ProposalBranchPrefix); ok {
			if !validInteropID(id) || !transferHash.MatchString(hash) {
				return bundle, errors.Join(errors.New("unsupported proposal branch identity"), rows.Close())
			}
			branches = append(branches, branchRecord{name: name, id: id, commit: hash})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return bundle, err
	}
	if hooks.afterCapture != nil {
		hooks.afterCapture()
	}
	if err := validateTransferSchema(ctx, conn, bundle.MainCommit); err != nil {
		return bundle, err
	}
	for _, table := range interopTables {
		bundle.Tables[table], err = readInteropRows(ctx, conn, bundle.MainCommit, table)
		if err != nil {
			return bundle, err
		}
	}
	for _, branch := range branches {
		var commits int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM DOLT_LOG(?, '--not', ?)", branch.commit, bundle.MainCommit).Scan(&commits); err != nil {
			return bundle, err
		}
		if commits == 0 {
			continue // already-merged cleanup residue
		}
		if commits != 1 {
			return bundle, errors.New("export requires each pending proposal to be one commit off captured main ancestry")
		}
		parent, err := commitParent(ctx, conn, branch)
		if err != nil {
			return bundle, err
		}
		beforeSchema, err := statusSchema(ctx, conn, parent)
		if err != nil {
			return bundle, err
		}
		afterSchema, err := statusSchema(ctx, conn, branch.commit)
		if err != nil || !maps.Equal(beforeSchema, afterSchema) {
			return bundle, errors.Join(errors.New("proposal schema changes are not supported by memory interop"), err)
		}
		proposal := InteropProposal{ID: branch.id, Head: branch.commit, Parent: parent, Changes: []InteropChange{}}
		for _, table := range transferTables {
			changes, err := repoDiffRows(ctx, conn, table.name, parent, branch.commit)
			if err != nil {
				return bundle, err
			}
			for _, change := range changes {
				delete(change.From, "live_key")
				delete(change.To, "live_key")
				switch table.name {
				case "proposals":
					if change.From != nil || interopValue(change.To, "id") != branch.id || proposal.Metadata != nil {
						return bundle, errors.New("proposal must add exactly its own metadata row")
					}
					proposal.Metadata = change.To
				case "facts", "decisions":
					proposal.Changes = append(proposal.Changes, InteropChange{Table: table.name, From: change.From, To: change.To})
				default:
					return bundle, errors.New("proposal changes content outside the reviewed memory lane")
				}
			}
		}
		bundle.Proposals = append(bundle.Proposals, proposal)
	}
	return bundle, validateInteropMemory(bundle)
}

func readInteropRows(ctx context.Context, conn *sql.Conn, hash, table string) (result []InteropRow, err error) {
	columns := interopColumns(table)
	if !transferHash.MatchString(hash) || len(columns) == 0 {
		return nil, errors.New("invalid interop snapshot/table")
	}
	projected := make([]string, len(columns))
	for i, column := range columns {
		projected[i] = "CAST(" + quoteIdentifier(column) + " AS CHAR)"
	}
	// Only validated immutable hashes and maintained column names enter SQL.
	rows, err := conn.QueryContext(ctx, "SELECT "+strings.Join(projected, ", ")+" FROM "+quoteIdentifier(DatabaseName+"/"+hash)+"."+quoteIdentifier(table)+" ORDER BY "+quoteIdentifier(columns[0]))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	result = []InteropRow{}
	for rows.Next() {
		cells, targets := make([]sql.NullString, len(columns)), make([]any, len(columns))
		for i := range targets {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := emptyInteropRow(table)
		for i, cell := range cells {
			if cell.Valid {
				row[columns[i]] = interopString(cell.String)
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func protectInteropDocumentPath(ctx context.Context, conn *sql.Conn, hash, output string) (err error) {
	rows, err := conn.QueryContext(ctx, "SELECT path FROM "+quoteIdentifier(DatabaseName+"/"+hash)+".documents")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var path sql.NullString
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if path.Valid && strings.EqualFold(filepath.Clean(path.String), filepath.Clean(output)) {
			return errors.New("export destination is a registered document source; select another file")
		}
	}
	return rows.Err()
}

func (s *Store) ImportMemory(ctx context.Context, opts ImportMemoryOptions) (InteropResult, error) {
	return s.importMemory(ctx, opts, interopHooks{})
}

func (s *Store) importMemory(ctx context.Context, opts ImportMemoryOptions, hooks interopHooks) (result InteropResult, err error) {
	result = InteropResult{Operation: "import", Status: "refused", File: opts.File, CreatedProposals: []StagedProposal{}, RemainingProposals: []string{}, Guidance: interopGuidance}
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	db, err := s.handle()
	if err != nil {
		return result, err
	}
	metadata, err := s.documentConfigRoot()
	if err != nil {
		return result, err
	}
	data, err := render.ReadBundle(opts.File, func(info os.FileInfo) error { return checkDocumentOwnerFile(metadata, info) })
	if err = errors.Join(err, metadata.Close()); err != nil {
		return result, err
	}
	result.SourceDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	var bundle InteropBundle
	var identities []InteropIdentity
	if opts.FromMemhub {
		var history legacyHistory
		bundle, identities, history, err = decodeMemhubExport(data)
		if err == nil {
			var annotation []byte
			annotation, err = json.MarshalIndent(history, "", "  ")
			if err == nil {
				note := emptyInteropRow("session_notes")
				note["id"], note["created_at"] = interopString(newID()), interopString(datetime(time.Now()))
				note["actor"], note["actor_raw"] = interopString(memory.UserActor.Name), interopString(memory.UserActor.Raw)
				note["text"] = interopString("memhub import/genesis annotation\nSource " + result.SourceDigest + "\nLegacy confidence intentionally omitted. Source writes_log and historical pending statuses are counts, not replayed Dolt history. Original actors/timestamps below are source metadata, not commit authorship. The source root path was not imported.\n" + string(annotation))
				bundle.Tables["session_notes"] = append(bundle.Tables["session_notes"], note)
				identities = append(identities, InteropIdentity{"session_notes", "import/genesis", "id", interopValue(note, "id")})
			}
		}
	} else {
		err = decodeInteropJSON(data, &bundle)
		if err == nil {
			err = validateInteropHeader(bundle)
		}
		if err == nil {
			identities = nativeInteropIdentities(bundle)
		}
	}
	if err != nil {
		return result, err
	}
	if err := validateInteropMemory(bundle); err != nil {
		return result, err
	}
	// Every row and proposal is translated and scanned before the first write.
	// Source provenance remains opaque text; actual commits belong to the human
	// importer now, never to a historical source actor or timestamp.
	author := memory.UserActor.CommitAuthor()
	text := interopText(bundle)
	text = append(text, author.Name, author.Email)
	if opts.FromMemhub {
		// Pending raw provenance is persisted in the annotation as JSON text.
		// Scan its decoded strings too; escaping must not hide denied content.
		var source memhubExport
		if err := decodeInteropJSON(data, &source); err != nil {
			return result, err
		}
		text = append(text, *source.ExportedBy)
		for _, pending := range source.Pending {
			if memhubString(pending, "status") == "pending" {
				text = append(text, memhubString(pending, "actor_raw"), memhubString(pending, "provenance_json"))
				var provenance any
				if err := decodeInteropJSON([]byte(memhubString(pending, "provenance_json")), &provenance); err != nil {
					return result, err
				}
				text = append(text, interopJSONText(provenance)...)
			}
		}
	}
	if err := s.checkDenyList(text); err != nil {
		return result, err
	}
	stages := make([]stagedWrite, len(bundle.Proposals))
	for i, proposal := range bundle.Proposals {
		stages[i], err = interopStagedWrite(proposal, author)
		if err != nil {
			return result, err
		}
		stages[i].finalizeCommit = hooks.finalizeStage
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err := requireTransferClean(ctx, conn); err != nil {
		return result, fmt.Errorf("import preflight: %w", err)
	}
	main, err := branchHead(ctx, conn, MainBranch)
	if err != nil {
		return result, err
	}
	if err := validateTransferSchema(ctx, conn, main); err != nil {
		return result, err
	}
	for _, table := range interopTables {
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return result, err
		}
		if count != 0 {
			return result, errors.New("import requires no existing durable memory; initialize a fresh repository target and retain this store; no force/wipe option exists")
		}
	}
	branches, err := proposalBranches(ctx, conn)
	if err != nil {
		return result, err
	}
	if len(branches) != 0 {
		return result, errors.New("import requires no proposal branches, including cleanup residue; inspect source and destination and use a fresh initialized target")
	}
	if err := validateInteropFactKeys(ctx, conn, bundle); err != nil {
		return result, err
	}
	for _, table := range []struct {
		name string
		dest *int
	}{{"documents", &result.RetainedDocuments}, {"doc_chunks", &result.RetainedDocChunks}} {
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(table.dest); err != nil {
			return result, err
		}
	}
	if hooks.beforeMain != nil {
		if err := hooks.beforeMain(); err != nil {
			return result, err
		}
	}
	current, err := branchHead(ctx, conn, MainBranch)
	if err != nil || current != main {
		return result, errors.Join(errors.New("main changed while preparing the import; inspect and use a fresh initialized target"), err)
	}
	var statements []store.Statement
	for _, table := range interopTables {
		for _, row := range bundle.Tables[table] {
			statements = append(statements, interopInsert(table, row))
		}
	}
	result.SourceCommit, result.Counts = bundle.MainCommit, interopCounts(bundle)
	if len(statements) != 0 {
		req := store.CommitRequest{
			Statements: statements, Text: text, RequireClean: true, Author: author,
			Message: "import memory " + result.SourceDigest,
		}
		finalize := hooks.finalizeMain
		if finalize == nil {
			finalize = (*sql.Tx).Commit
		}
		commit, commitErr := s.commitConnFinalize(ctx, conn, req, finalize)
		if commit.Hash != "" {
			result.MainCommit, result.Status, result.IdentityMap = commit.Hash, "partial", identities
			result.RemainingProposals = interopProposalIDs(bundle)
		}
		if commitErr != nil {
			if commit.Hash == "" {
				result.Status, result.IdentityMap, result.RemainingProposals = "unknown", identities, interopProposalIDs(bundle)
			}
			return result, fmt.Errorf("import commit failed or finalization incomplete; inspect confirmed main and proposal prefix before retry: %w", commitErr)
		}
	}
	result.IdentityMap, result.RemainingProposals, result.Status = identities, interopProposalIDs(bundle), "partial"
	if hooks.afterMain != nil {
		if err := hooks.afterMain(); err != nil {
			return result, err
		}
	}
	for i, staged := range stages {
		if hooks.beforeProposal != nil {
			if err := hooks.beforeProposal(i); err != nil {
				return result, err
			}
		}
		created, stageErr := s.stageLocked(ctx, staged)
		if created.Commit != "" {
			result.CreatedProposals = append(result.CreatedProposals, created)
			result.RemainingProposals = slices.Clone(result.RemainingProposals[1:])
		}
		if stageErr != nil {
			branch := ProposalBranch(staged.id)
			head, inspectErr := branchHead(context.WithoutCancel(ctx), conn, branch)
			if inspectErr == nil {
				result.ProposalResidue = &InteropResidue{ID: staged.id, Branch: branch, Head: head}
				if created.Commit == "" {
					result.Status = "unknown"
				}
			} else if !errors.Is(inspectErr, sql.ErrNoRows) {
				result.Status = "unknown"
				stageErr = errors.Join(stageErr, fmt.Errorf("inspect failed imported proposal residue: %w", inspectErr))
			}
			return result, fmt.Errorf("import stopped at proposal %d; main and the reported created prefix remain; inspect before retry: %w", i+1, stageErr)
		}
	}
	result.Status = "imported"
	return result, nil
}

func interopStagedWrite(p InteropProposal, author store.Actor) (stagedWrite, error) {
	actor := store.Actor{Name: interopValue(p.Metadata, "actor"), Email: "import-source@memdolt.invalid"}
	w := stagedWrite{kind: ProposalKind(interopValue(p.Metadata, "kind")), id: p.ID, commitAuthor: &author, imported: &p,
		proposal: Proposal{Rationale: interopValue(p.Metadata, "rationale"), Actor: actor, Target: TargetRepo},
		message:  "import pending proposal " + p.ID}
	if err := w.proposal.validate(); err != nil {
		return w, err
	}
	w.now, _ = time.Parse(time.DateTime, interopValue(p.Metadata, "created_at"))
	// Modifications precede inserts so facts.live_key is released first.
	changes := slices.Clone(p.Changes)
	slices.SortStableFunc(changes, func(a, b InteropChange) int {
		if (a.From == nil) != (b.From == nil) {
			if a.From != nil {
				return -1
			}
			return 1
		}
		return 0
	})
	for _, change := range changes {
		row := change.To
		if change.From == nil {
			w.rowID = interopValue(row, "id")
			w.statements = append(w.statements, interopInsert(change.Table, row))
		} else {
			w.rowID = interopValue(row, "id")
			var assignments []string
			var args []any
			for _, column := range interopColumns(change.Table)[1:] {
				assignments = append(assignments, quoteIdentifier(column)+" = ?")
				args = append(args, interopArgument(row[column]))
			}
			args = append(args, w.rowID)
			w.statements = append(w.statements, store.Statement{SQL: "UPDATE " + quoteIdentifier(change.Table) + " SET " + strings.Join(assignments, ", ") + " WHERE id = ?", Args: args})
		}
		for _, image := range []InteropRow{change.From, change.To} {
			for _, column := range interopColumns(change.Table) {
				if value := image[column]; value != nil {
					w.text = append(w.text, *value)
				}
			}
		}
	}
	return w, nil
}

// Compare again on the just-cut branch, as fact-conflict staging does. This
// catches a foreign main change before branch cut without overwriting its row
// in an imported proposal. The final interval is not a foreign-writer CAS.
func validateInteropStage(ctx context.Context, conn *sql.Conn, proposal InteropProposal) error {
	var head string
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&head); err != nil {
		return err
	}
	view := map[string]map[string]InteropRow{}
	for _, table := range []string{"facts", "decisions"} {
		rows, err := readInteropRows(ctx, conn, head, table)
		if err != nil {
			return err
		}
		view[table] = map[string]InteropRow{}
		for _, row := range rows {
			view[table][interopValue(row, "id")] = row
		}
	}
	return validateInteropProposal(proposal, view)
}

func interopInsert(table string, row InteropRow) store.Statement {
	columns := interopColumns(table)
	quoted, placeholders := make([]string, len(columns)), make([]string, len(columns))
	args := make([]any, len(columns))
	for i, column := range columns {
		quoted[i], placeholders[i], args[i] = quoteIdentifier(column), "?", interopArgument(row[column])
	}
	return store.Statement{SQL: "INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")", Args: args}
}

func interopArgument(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func interopCounts(bundle InteropBundle) map[string]int {
	counts := map[string]int{"pending_proposals": len(bundle.Proposals)}
	for _, table := range interopTables {
		counts[table] = len(bundle.Tables[table])
	}
	return counts
}

func interopProposalIDs(bundle InteropBundle) []string {
	ids := make([]string, len(bundle.Proposals))
	for i, p := range bundle.Proposals {
		ids[i] = p.ID
	}
	return ids
}

func nativeInteropIdentities(bundle InteropBundle) []InteropIdentity {
	var ids []InteropIdentity
	for _, table := range interopTables {
		key := interopColumns(table)[0]
		for _, row := range bundle.Tables[table] {
			id := interopValue(row, key)
			ids = append(ids, InteropIdentity{table, id, key, id})
		}
	}
	for _, p := range bundle.Proposals {
		ids = append(ids, InteropIdentity{"proposals", p.ID, "id", p.ID})
		for _, change := range p.Changes {
			if change.From == nil {
				id := interopValue(change.To, "id")
				ids = append(ids, InteropIdentity{change.Table, id, "id", id})
			}
		}
	}
	return ids
}

func interopText(bundle InteropBundle) []string {
	var text []string
	for _, table := range interopTables {
		for _, row := range bundle.Tables[table] {
			for _, column := range interopColumns(table) {
				if value := row[column]; value != nil {
					text = append(text, *value)
				}
			}
		}
	}
	for _, proposal := range bundle.Proposals {
		for _, value := range proposal.Metadata {
			if value != nil {
				text = append(text, *value)
			}
		}
		for _, change := range proposal.Changes {
			for _, image := range []InteropRow{change.From, change.To} {
				for _, value := range image {
					if value != nil {
						text = append(text, *value)
					}
				}
			}
		}
	}
	return text
}

func interopJSONText(value any) []string {
	var text []string
	switch value := value.(type) {
	case string:
		text = append(text, value)
	case []any:
		for _, item := range value {
			text = append(text, interopJSONText(item)...)
		}
	case map[string]any:
		for key, item := range value {
			text = append(text, key)
			text = append(text, interopJSONText(item)...)
		}
	}
	return text
}

// Exact Go strings alone do not establish uniqueness under a destination's
// SQL collation. Ask that collation about all main/proposal live-key sets with
// bound values, before any INSERT. IDs and enum keys have canonical ASCII forms.
func validateInteropFactKeys(ctx context.Context, conn *sql.Conn, bundle InteropBundle) error {
	var collation string
	if err := conn.QueryRowContext(ctx, "SELECT collation_name FROM information_schema.columns WHERE table_schema = ? AND table_name = 'facts' AND column_name = 'live_key'", DatabaseName).Scan(&collation); err != nil {
		return fmt.Errorf("read fact-key collation before import: %w", err)
	}
	main := map[string]InteropRow{}
	for _, row := range bundle.Tables["facts"] {
		main[interopValue(row, "id")] = row
	}
	for i := -1; i < len(bundle.Proposals); i++ {
		view := main
		if i >= 0 {
			view = maps.Clone(main)
			for _, change := range bundle.Proposals[i].Changes {
				if change.Table == "facts" {
					view[interopValue(change.To, "id")] = change.To
				}
			}
		}
		var terms []string
		var args []any
		for _, row := range view {
			if row["superseded_by"] == nil && row["key"] != nil {
				terms = append(terms, "SELECT CAST(? AS CHAR) COLLATE "+quoteIdentifier(collation)+" AS fact_key")
				args = append(args, *row["key"])
			}
		}
		if len(terms) < 2 {
			continue
		}
		var count int
		err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+strings.Join(terms, " UNION ALL ")+") AS candidate_keys GROUP BY fact_key HAVING COUNT(*) > 1 LIMIT 1", args...).Scan(&count)
		if err == nil {
			return errors.New("ambiguous live fact keys under the destination collation; reconcile source keys before import")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("validate imported fact-key uniqueness: %w", err)
		}
	}
	return nil
}
