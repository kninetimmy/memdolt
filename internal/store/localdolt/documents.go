package localdolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

type Document struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Title       string    `json:"title"`
	ContentHash string    `json:"contentHash"`
	ByteLen     int64     `json:"byteLen"`
	Source      string    `json:"source"`
	IngestedAt  time.Time `json:"ingestedAt"`
	ChunkCount  int       `json:"chunkCount"`
}

type DocChunk struct {
	ID          string `json:"id"`
	DocID       string `json:"docId"`
	Ord         int    `json:"ord"`
	HeadingPath string `json:"headingPath"`
	Body        string `json:"body"`
}

type DocAddOptions struct {
	File     string       `json:"file"`
	Title    string       `json:"title,omitempty"`
	Actor    memory.Actor `json:"actor"`
	Confined bool         `json:"confined"`
}

// DocResult preserves confirmed data when config, connection, transport or
// output finalization fails. A not-found result has no Document. Error is used
// by the CLI/MCP output surfaces alongside their non-success indication.
type DocResult struct {
	Status               string     `json:"status"`
	Document             *Document  `json:"document,omitempty"`
	Chunks               []DocChunk `json:"chunks,omitempty"`
	Commit               string     `json:"commit,omitempty"`
	EnabledDefaultRecall bool       `json:"enabledDefaultRecall"`
	Error                string     `json:"error,omitempty"`
}

// DocAdd is the whole direct-lane operation, also submitted once over owner
// IPC. Source is always user (PRD §7.6); the normalized caller authors the
// commit. Neither this source label nor any path grants review authority.
func (s *Store) DocAdd(ctx context.Context, opts DocAddOptions) (DocResult, error) {
	return s.docAdd(ctx, opts, enableDocumentRecall)
}

func (s *Store) docAdd(ctx context.Context, opts DocAddOptions, finalize func(*os.Root) (bool, error)) (result DocResult, err error) {
	return s.docAddFinalize(ctx, opts, finalize, (*sql.Tx).Commit)
}

func (s *Store) docAddFinalize(ctx context.Context, opts DocAddOptions, finalize func(*os.Root) (bool, error), finalizeCommit func(*sql.Tx) error) (result DocResult, err error) {
	if err := validateDocumentActor(opts.Actor); err != nil {
		return result, err
	}
	// ponytail: hold the existing mutation mutex through file IO and config
	// finalization. Split preparation out only if document ingestion blocks
	// ordinary writes measurably. Foreign Dolt/config writers do not share it.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	conn, head, err := s.initializedMainConn(ctx, "document")
	if err != nil {
		return result, err
	}
	defer func() { err = documentFinalError(result, errors.Join(err, conn.Close())) }()
	configRoot, err := s.documentConfigRoot()
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, configRoot.Close()) }()
	cfg, _, _, err := readDocumentConfig(configRoot)
	if err != nil {
		return result, err
	}
	path, data, err := s.readDocumentFile(opts, cfg, configRoot)
	if err != nil {
		return result, err
	}
	sections := chunkMarkdown(string(data))
	title := opts.Title
	if strings.TrimSpace(title) == "" {
		title = documentTitle(sections, path)
	}
	if err := documentString("title", title, 512); err != nil {
		return result, err
	}
	text := []string{opts.File, path, opts.Title, title, "user", string(data), opts.Actor.Name, opts.Actor.Raw}
	for _, section := range sections {
		if err := documentString("heading breadcrumb", section.heading, 1024); err != nil {
			return result, err
		}
		if len(section.body) > 65535 {
			return result, errors.New("document paragraph or fenced block exceeds the 65535-byte TEXT column; add paragraph or section boundaries")
		}
		text = append(text, section.heading)
	}
	// Even unchanged input is checked before returning. The commit scans the
	// same declared text again, closing the config-change gap before durability.
	if err := s.checkDenyList(text); err != nil {
		return result, err
	}
	docs, err := readDocuments(ctx, conn, head, "d.path = ?", path)
	if err != nil {
		return result, err
	}
	doc := Document{
		ID: newID(), Path: path, Title: title, ContentHash: fmt.Sprintf("%x", sha256.Sum256(data)),
		ByteLen: int64(len(data)), Source: "user", IngestedAt: time.Now().UTC().Truncate(time.Second),
		ChunkCount: len(sections),
	}
	status := "created"
	if len(docs) > 0 {
		old := docs[0]
		// The existing SQL path index may equate paths that the local filesystem
		// distinguishes. Never replace another file merely due to collation.
		if rel, err := filepath.Rel(old.Path, path); err != nil || rel != "." {
			return result, fmt.Errorf("document path collides with document %s under the existing database path index; use an unambiguous source path", old.ID)
		}
		if old.ContentHash == doc.ContentHash {
			return DocResult{Status: "unchanged", Document: &old}, nil
		}
		doc.ID, doc.Path, status = old.ID, old.Path, "updated"
	}
	if err := requireCleanWorkingSet(ctx, conn, "ingest the document"); err != nil {
		return result, err
	}
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(DatabaseName+"/"+head)+".documents").Scan(&count); err != nil {
		return result, fmt.Errorf("count committed documents: %w", err)
	}
	req := store.CommitRequest{
		Text: text, RequireClean: true, Author: opts.Actor.CommitAuthor(),
		Message: fmt.Sprintf("doc add %s (%d chunks)", doc.ID, doc.ChunkCount),
	}
	if status == "updated" {
		req.Statements = append(req.Statements,
			store.Statement{SQL: "DELETE FROM doc_chunks WHERE doc_id = ?", Args: []any{doc.ID}},
			store.Statement{SQL: "UPDATE documents SET title = ?, content_hash = ?, byte_len = ?, source = 'user', ingested_at = ? WHERE id = ?",
				Args: []any{doc.Title, doc.ContentHash, doc.ByteLen, doc.IngestedAt, doc.ID}})
	} else {
		req.Statements = append(req.Statements, store.Statement{
			SQL:  "INSERT INTO documents (id, path, title, content_hash, byte_len, source, ingested_at) VALUES (?, ?, ?, ?, ?, 'user', ?)",
			Args: []any{doc.ID, doc.Path, doc.Title, doc.ContentHash, doc.ByteLen, doc.IngestedAt},
		})
	}
	chunks := make([]DocChunk, len(sections))
	for i, section := range sections {
		// Replacement never reuses a chunk ID: a vector for the previous row
		// can only be an orphan, never a vector for different current content.
		chunk := DocChunk{newID(), doc.ID, i, section.heading, section.body}
		chunks[i] = chunk
		req.Statements = append(req.Statements, store.Statement{
			SQL:  "INSERT INTO doc_chunks (id, doc_id, ord, heading_path, body) VALUES (?, ?, ?, ?, ?)",
			Args: []any{chunk.ID, doc.ID, i, chunk.HeadingPath, chunk.Body},
		})
	}
	commit, err := s.commitConnFinalize(ctx, conn, req, finalizeCommit)
	if commit.Hash != "" {
		result = DocResult{Status: status, Document: &doc, Chunks: chunks, Commit: commit.Hash}
	}
	if err != nil {
		if commit.Hash != "" && count == 0 {
			err = fmt.Errorf("default-recall configuration was not finalized; inspect .memdolt/config.toml and set [retrieval] include_docs_in_default = true explicitly; do not replay ingestion to repair configuration: %w", err)
		}
		return result, fmt.Errorf("document commit failed; inspect `memdolt doc show %s` before retrying: %w", doc.ID, err)
	}
	if count == 0 {
		result.EnabledDefaultRecall, err = finalize(configRoot)
		if err != nil {
			return result, fmt.Errorf("document committed but default-recall configuration finalization failed; inspect .memdolt/config.toml and set [retrieval] include_docs_in_default = true explicitly; do not replay ingestion to repair configuration: %w", err)
		}
	}
	return result, nil
}

// DocList and DocShow pin all their table reads to one captured main hash.
// Dirty/proposal contents remain outside that snapshot.
func (s *Store) DocList(ctx context.Context) (docs []Document, err error) {
	conn, head, err := s.initializedMainConn(ctx, "document")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	return readDocuments(ctx, conn, head, "")
}

func (s *Store) DocShow(ctx context.Context, ident string) (result DocResult, err error) {
	if err := documentPathArgument(ident); err != nil {
		return result, err
	}
	conn, head, err := s.initializedMainConn(ctx, "document")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	doc, err := s.resolveDocument(ctx, conn, head, ident)
	if err != nil || doc == nil {
		return DocResult{Status: "not-found"}, err
	}
	result = DocResult{Status: "found", Document: doc, Chunks: []DocChunk{}}
	rows, err := conn.QueryContext(ctx, "SELECT id, doc_id, ord, COALESCE(heading_path, ''), COALESCE(body, '') FROM "+quoteIdentifier(DatabaseName+"/"+head)+".doc_chunks WHERE doc_id = ? ORDER BY ord", doc.ID)
	if err != nil {
		return result, fmt.Errorf("read document chunks: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var chunk DocChunk
		if err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.Ord, &chunk.HeadingPath, &chunk.Body); err != nil {
			return result, fmt.Errorf("scan document chunk: %w", err)
		}
		result.Chunks = append(result.Chunks, chunk)
	}
	return result, rows.Err()
}

func (s *Store) DocRemove(ctx context.Context, ident string, actor memory.Actor) (result DocResult, err error) {
	return s.docRemove(ctx, ident, actor, (*sql.Tx).Commit)
}

func (s *Store) docRemove(ctx context.Context, ident string, actor memory.Actor, finalizeCommit func(*sql.Tx) error) (result DocResult, err error) {
	if err := documentPathArgument(ident); err != nil {
		return result, err
	}
	if err := validateDocumentActor(actor); err != nil {
		return result, err
	}
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	conn, head, err := s.initializedMainConn(ctx, "document")
	if err != nil {
		return result, err
	}
	defer func() { err = documentFinalError(result, errors.Join(err, conn.Close())) }()
	doc, err := s.resolveDocument(ctx, conn, head, ident)
	if err != nil || doc == nil {
		return DocResult{Status: "not-found"}, err
	}
	if err := requireCleanWorkingSet(ctx, conn, "remove the document"); err != nil {
		return result, err
	}
	commit, err := s.commitConnFinalize(ctx, conn, store.CommitRequest{
		Statements: []store.Statement{
			{SQL: "DELETE FROM doc_chunks WHERE doc_id = ?", Args: []any{doc.ID}},
			{SQL: "DELETE FROM documents WHERE id = ?", Args: []any{doc.ID}},
		},
		Text: []string{ident, doc.ID, actor.Name, actor.Raw}, RequireClean: true,
		Message: "doc rm " + doc.ID, Author: actor.CommitAuthor(),
	}, finalizeCommit)
	if commit.Hash != "" {
		result = DocResult{Status: "removed", Document: doc, Commit: commit.Hash}
	}
	if err != nil {
		return result, fmt.Errorf("document removal failed; inspect `memdolt doc show %s` before retrying: %w", doc.ID, err)
	}
	return result, nil
}

func validateDocumentActor(actor memory.Actor) error {
	normalized, err := memory.NormalizeActor(actor.Name)
	if err != nil || normalized.Name != actor.Name {
		return errors.New("document caller must carry an already-normalized actor")
	}
	return documentString("raw actor", actor.Raw, 255)
}

func documentFinalError(result DocResult, err error) error {
	if err != nil && result.Commit != "" {
		return fmt.Errorf("document %s confirmed %s in commit %s; inspect `memdolt doc show %s` before retrying: %w",
			result.Document.ID, result.Status, result.Commit, result.Document.ID, err)
	}
	return err
}

func (s *Store) initializedMainConn(ctx context.Context, purpose string) (*sql.Conn, string, error) {
	conn, err := s.committedMainConn(ctx, purpose)
	if err != nil {
		return nil, "", err
	}
	head, err := branchHead(ctx, conn, MainBranch)
	if err == nil && !transferHash.MatchString(head) {
		err = errors.New("invalid immutable Dolt commit hash")
	}
	if err == nil {
		var version string
		err = conn.QueryRowContext(ctx, "SELECT v FROM "+quoteIdentifier(DatabaseName+"/"+head)+".meta WHERE k = ?", store.SchemaVersionKey).Scan(&version)
		if err == nil && version != strconv.Itoa(store.LatestSchemaVersion()) {
			err = fmt.Errorf("%s operations require the current schema", purpose)
		}
	}
	if err != nil {
		return nil, "", errors.Join(fmt.Errorf("read initialized %s store; use `memdolt init` for an older store or a newer binary for a newer store: %w", purpose, err), conn.Close())
	}
	return conn, head, nil
}

func readDocuments(ctx context.Context, conn *sql.Conn, head, where string, args ...any) (docs []Document, err error) {
	// Like transfer schema reads, use a revision-qualified database: the
	// pinned planbuilder cannot bind AS OF ?. No caller text enters the SQL.
	if !transferHash.MatchString(head) {
		return nil, errors.New("invalid immutable Dolt commit hash")
	}
	database := quoteIdentifier(DatabaseName + "/" + head)
	query := `SELECT d.id, COALESCE(d.path, ''), COALESCE(d.title, ''), COALESCE(d.content_hash, ''),
COALESCE(d.byte_len, 0), COALESCE(d.source, ''), d.ingested_at,
(SELECT COUNT(*) FROM ` + database + `.doc_chunks AS c WHERE c.doc_id = d.id)
FROM ` + database + `.documents AS d`
	// where is supplied only by the fixed queries below, never by a caller.
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY d.ingested_at DESC, d.id DESC"
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read committed documents: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	docs = []Document{}
	for rows.Next() {
		var doc Document
		var ingested sql.NullTime
		if err := rows.Scan(&doc.ID, &doc.Path, &doc.Title, &doc.ContentHash, &doc.ByteLen, &doc.Source, &ingested, &doc.ChunkCount); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		doc.IngestedAt = timeValue(ingested)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) resolveDocument(ctx context.Context, conn *sql.Conn, head, ident string) (*Document, error) {
	canonical, pathErr := s.documentIdentityPath(ident)
	if pathErr != nil {
		// Exact stored paths and IDs do not require a usable source filesystem.
		// Repeat the supplied value so failed resolution cannot match an empty
		// path belonging to another document.
		canonical = ident
	}
	docs, err := readDocuments(ctx, conn, head, "d.id = ? OR d.path = ? OR d.path = ?", ident, ident, canonical)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc.ID == ident {
			return &doc, nil
		}
	}
	if len(docs) > 1 {
		return nil, errors.New("document path is ambiguous; use an exact document id from `memdolt doc ls`")
	}
	if len(docs) == 0 {
		return nil, pathErr
	}
	return &docs[0], nil
}
