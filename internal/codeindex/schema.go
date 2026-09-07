package codeindex

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kninetimmy/memdolt/internal/layout"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1
const applicationID = 0x4d444349 // MDCI; an unrelated SQLite file is never disposable.

const schemaDDL = `CREATE TABLE index_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE indexed_files (
 id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, mtime INTEGER NOT NULL,
 size INTEGER NOT NULL, content_hash TEXT NOT NULL, language TEXT NOT NULL
);
CREATE TABLE code_chunks (
 id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL REFERENCES indexed_files(id) ON DELETE CASCADE,
 start_line INTEGER NOT NULL, end_line INTEGER NOT NULL, symbol TEXT, kind TEXT NOT NULL,
 content_hash TEXT NOT NULL, embed_text TEXT NOT NULL
);
CREATE INDEX idx_code_chunks_file ON code_chunks(file_id);
CREATE TABLE code_embeddings (
 chunk_id INTEGER PRIMARY KEY REFERENCES code_chunks(id) ON DELETE CASCADE,
 model_name TEXT NOT NULL, dimension INTEGER NOT NULL, vector BLOB NOT NULL,
 content_hash TEXT NOT NULL, vector_hash TEXT NOT NULL
);
CREATE VIRTUAL TABLE code_chunks_fts USING fts5(symbol, embed_text, content='code_chunks', content_rowid='id');
CREATE TRIGGER code_chunks_fts_ai AFTER INSERT ON code_chunks BEGIN
 INSERT INTO code_chunks_fts(rowid, symbol, embed_text) VALUES (new.id, new.symbol, new.embed_text);
END;
CREATE TRIGGER code_chunks_fts_ad AFTER DELETE ON code_chunks BEGIN
 INSERT INTO code_chunks_fts(code_chunks_fts, rowid, symbol, embed_text) VALUES ('delete', old.id, old.symbol, old.embed_text);
END;
CREATE TRIGGER code_chunks_fts_au AFTER UPDATE ON code_chunks BEGIN
 INSERT INTO code_chunks_fts(code_chunks_fts, rowid, symbol, embed_text) VALUES ('delete', old.id, old.symbol, old.embed_text);
 INSERT INTO code_chunks_fts(rowid, symbol, embed_text) VALUES (new.id, new.symbol, new.embed_text);
END;`

func (r *repository) indexPath() string { return filepath.Join(r.base, layout.DirName, indexName) }

// Refuse linked files, hard-link aliases and unexpected SQLite companions
// before allowing SQLite's path-based opener to write any byte.
func (r *repository) checkIndexFiles() (os.FileInfo, error) {
	if err := r.verifyMetadata(); err != nil {
		return nil, err
	}
	var main os.FileInfo
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		f, info, err := openRegular(r.metadata, indexName+suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect code-index file: %w", err)
		}
		err = layout.CheckOwnerSource(r.metadata, info)
		one, linkErr := singleLink(f)
		if linkErr == nil && !one {
			linkErr = errors.New("code-index files must not have hard-link aliases")
		}
		err = errors.Join(err, linkErr, f.Close())
		if err != nil {
			return nil, err
		}
		if suffix == "" {
			main = info
		} else {
			return nil, errors.New("code index has a journal or WAL companion; an operation may be active, or crash residue needs inspection")
		}
	}
	return main, nil
}

func (r *repository) openDB(ctx context.Context, writable bool) (_ *sql.DB, created bool, err error) {
	if r.metadata == nil {
		return nil, false, nil
	}
	info, err := r.checkIndexFiles()
	if err != nil {
		return nil, false, err
	}
	if info == nil {
		if !writable {
			return nil, false, nil
		}
		f, err := r.metadata.OpenFile(indexName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, false, err
		}
		info, err = f.Stat()
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, true, err
		}
		created = true
	}
	if !created {
		f, opened, err := openRegular(r.metadata, indexName)
		if err != nil {
			return nil, false, err
		}
		if err := layout.CheckOwnerSource(r.metadata, opened); err != nil {
			return nil, false, errors.Join(err, f.Close())
		}
		var header [72]byte
		_, readErr := f.ReadAt(header[:], 0)
		if err := errors.Join(readErr, f.Close()); err != nil {
			return nil, false, errors.New("code_index.sqlite has no recognized index header; unrelated file retained")
		}
		if string(header[:16]) != "SQLite format 3\x00" || binary.BigEndian.Uint32(header[68:]) != applicationID {
			return nil, false, errors.New("code_index.sqlite is not a recognized memdolt code index; unrelated file retained")
		}
	}
	defer func() {
		if err != nil && created {
			current, inspectErr := r.metadata.Lstat(indexName)
			if inspectErr == nil && !unsafeLink(current) && os.SameFile(current, info) {
				err = errors.Join(err, r.metadata.Remove(indexName))
			} else {
				err = errors.Join(err, errors.New("failed new code-index identity changed; retained for inspection"), inspectErr)
			}
		}
	}()
	mode := "ro"
	if writable {
		mode = "rw"
	}
	uriPath := filepath.ToSlash(r.indexPath())
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "foreign_keys(ON)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, created, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	var app int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return nil, created, fmt.Errorf("read code-index identity: %w", err)
	}
	if !created && app != applicationID {
		return nil, false, errors.New("code_index.sqlite is not a recognized memdolt code index; unrelated file retained")
	}
	current, err := r.checkIndexFiles()
	if err != nil {
		return nil, created, err
	}
	if current == nil || !os.SameFile(info, current) {
		return nil, created, errors.New("code-index identity changed while opening")
	}
	if writable {
		// DELETE journaling avoids status creating WAL/shared-memory files.
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL"); err != nil {
			return nil, created, err
		}
	}
	return db, created, nil
}

func storedVersion(ctx context.Context, db *sql.DB) (*int, error) {
	var present int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='index_meta'").Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT value FROM index_meta WHERE key='schema_version'").Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil, nil
	}
	return &v, nil
}

// Do not delete a file which has acquired unrelated tables or triggers.
func checkOwnedSchema(ctx context.Context, db *sql.DB) (err error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		switch name {
		case "index_meta", "indexed_files", "code_chunks", "code_embeddings", "idx_code_chunks_file", "code_chunks_fts",
			"code_chunks_fts_data", "code_chunks_fts_idx", "code_chunks_fts_docsize", "code_chunks_fts_config",
			"code_chunks_fts_ai", "code_chunks_fts_ad", "code_chunks_fts_au":
		default:
			return errors.New("code index contains unrelated schema objects; retained for inspection")
		}
	}
	return rows.Err()
}

func bootstrap(ctx context.Context, db *sql.DB, created bool) (err error) {
	if err := checkOwnedSchema(ctx, db); err != nil {
		return err
	}
	v, err := storedVersion(ctx, db)
	if err != nil {
		return err
	}
	if v != nil && *v == schemaVersion {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx, &err)
	if !created {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS code_chunks_fts;
DROP TABLE IF EXISTS code_embeddings; DROP TABLE IF EXISTS code_chunks;
DROP TABLE IF EXISTS indexed_files; DROP TABLE IF EXISTS index_meta;`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, schemaDDL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", applicationID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO index_meta(key,value) VALUES ('schema_version',?)", strconv.Itoa(schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func rollback(tx *sql.Tx, err *error) {
	if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
		*err = errors.Join(*err, e)
	}
}

func metaValue(ctx context.Context, tx *sql.Tx, key string) (*string, error) {
	var value string
	err := tx.QueryRowContext(ctx, "SELECT value FROM index_meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &value, err
}

func setMeta(ctx context.Context, tx *sql.Tx, key string, value *string) error {
	if value == nil {
		_, err := tx.ExecContext(ctx, "DELETE FROM index_meta WHERE key=?", key)
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO index_meta(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, *value)
	return err
}

func needsRebuild(version *int) error {
	if version == nil || *version != schemaVersion {
		return errors.New("code index schema is incompatible; run `memdolt code index` to rebuild")
	}
	return nil
}

func isTestPath(file string) bool {
	return strings.HasPrefix(file, "tests/") || strings.HasPrefix(file, "benches/") || strings.HasPrefix(file, "examples/")
}
