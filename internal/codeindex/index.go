package codeindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

type FileSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type RefreshSummary struct {
	Committed      bool       `json:"committed"`
	FilesTotal     int        `json:"filesTotal"`
	ChunksTotal    int        `json:"chunksTotal"`
	TrackedTotal   int        `json:"trackedTotal"`
	NewFiles       int        `json:"newFiles"`
	ChangedFiles   int        `json:"changedFiles"`
	UnchangedFiles int        `json:"unchangedFiles"`
	DeletedFiles   int        `json:"deletedFiles"`
	SkippedFiles   int        `json:"skippedFiles"`
	ExcludedFiles  int        `json:"excludedFiles"`
	DeniedFiles    int        `json:"deniedFiles"`
	BinarySkipped  int        `json:"binarySkipped"`
	EmbeddedChunks int        `json:"embeddedChunks"`
	Head           *string    `json:"head"`
	Skipped        []FileSkip `json:"skipped"`
}

type StatusReport struct {
	Path              string         `json:"path"`
	Exists            bool           `json:"exists"`
	SchemaVersion     *int           `json:"schemaVersion"`
	NeedsRebuild      bool           `json:"needsRebuild"`
	Mode              retrieval.Mode `json:"mode"`
	FilesTotal        int            `json:"filesTotal"`
	ChunksTotal       int            `json:"chunksTotal"`
	EmbeddingsTotal   int            `json:"embeddingsTotal"`
	InvalidEmbeddings int            `json:"invalidEmbeddings"`
	LastHead          *string        `json:"lastHead"`
	CurrentHead       *string        `json:"currentHead"`
	HeadStale         bool           `json:"headStale"`
}

type RemoveResult struct {
	Path    string `json:"path"`
	Removed bool   `json:"removed"`
}

type existingFile struct {
	id, mtime, size int64
	hash            string
}

// Refresh commits the working-tree diff, then fills current vectors. A failed
// embed leaves a coherent FTS index and is reported; the next refresh retries.
// A nil engine selects the checksum-verified production inference pipeline.
func Refresh(ctx context.Context, start string, engine retrieval.Inference) (summary RefreshSummary, err error) {
	r, err := openRepository(start, true)
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	if err := r.acquire(); err != nil {
		return summary, err
	}
	db, created, err := r.openDB(ctx, true)
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := bootstrap(ctx, db, created); err != nil {
		return summary, fmt.Errorf("initialize code index: %w", err)
	}
	lazy := &localInference{ctx: ctx}
	if engine == nil {
		engine = lazy
	}
	defer func() { err = errors.Join(err, lazy.close()) }()
	return r.refresh(ctx, db, engine)
}

func (r *repository) refresh(ctx context.Context, db *sql.DB, engine retrieval.Inference) (summary RefreshSummary, err error) {
	files, err := trackedFiles(ctx, r.base)
	if err != nil {
		return summary, err
	}
	summary = RefreshSummary{TrackedTotal: len(files), Head: currentHead(ctx, r.base), Skipped: []FileSkip{}}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return summary, err
	}
	defer rollback(tx, &err)
	existing, err := loadFiles(ctx, tx)
	if err != nil {
		return summary, err
	}
	previousRules, err := metaValue(ctx, tx, "deny_hash")
	if err != nil {
		return summary, err
	}
	sameRules := previousRules != nil && *previousRules == r.cfg.ruleHash
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if validateSourcePath(file) != nil || defaultDenied(file) || r.cfg.denied.Check(file) != nil {
			summary.DeniedFiles++
			summary.Skipped = append(summary.Skipped, FileSkip{file, "denied path"})
			continue
		}
		if !indexable(file) {
			summary.ExcludedFiles++
			continue
		}
		f, info, openErr := r.openSource(file)
		if openErr != nil {
			var protection *protectionError
			if errors.As(openErr, &protection) {
				return summary, openErr
			}
			if errors.Is(openErr, layout.ErrOwnerSource) {
				summary.DeniedFiles++
				summary.Skipped = append(summary.Skipped, FileSkip{file, "protected owner metadata"})
			} else {
				summary.SkippedFiles++
				summary.Skipped = append(summary.Skipped, FileSkip{file, "absent, linked, non-regular or unreadable source"})
			}
			continue // Leave prior rows for cleanup, including unreadable sources.
		}
		prev, found := existing[file]
		mtime := info.ModTime().UnixMilli()
		knownTime := !info.ModTime().IsZero() && mtime >= 0
		if found && sameRules && knownTime && prev.mtime == mtime && prev.size == info.Size() {
			if err := f.Close(); err != nil {
				return summary, err
			}
			delete(existing, file)
			summary.UnchangedFiles++
			continue
		}
		data, readErr := readSource(f, info)
		if err := f.Close(); err != nil {
			return summary, errors.Join(readErr, err)
		}
		if readErr != nil {
			summary.SkippedFiles++
			summary.Skipped = append(summary.Skipped, FileSkip{file, "unreadable or changed during read"})
			continue
		}
		if r.cfg.denied.Check(string(data)) != nil {
			summary.DeniedFiles++
			summary.Skipped = append(summary.Skipped, FileSkip{file, "denied content"})
			continue
		}
		hash := hashBytes(data)
		delete(existing, file)
		if found && hash == prev.hash {
			if _, err := tx.ExecContext(ctx, "UPDATE indexed_files SET mtime=?,size=? WHERE id=?", mtime, info.Size(), prev.id); err != nil {
				return summary, err
			}
			summary.UnchangedFiles++
			continue
		}
		if found {
			if _, err := tx.ExecContext(ctx, "DELETE FROM code_chunks WHERE file_id=?", prev.id); err != nil {
				return summary, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE indexed_files SET mtime=?,size=?,content_hash=?,language=? WHERE id=?", mtime, info.Size(), hash, languageFor(file), prev.id); err != nil {
				return summary, err
			}
			summary.ChangedFiles++
		} else {
			res, err := tx.ExecContext(ctx, "INSERT INTO indexed_files(path,mtime,size,content_hash,language) VALUES (?,?,?,?,?)", file, mtime, info.Size(), hash, languageFor(file))
			if err != nil {
				return summary, err
			}
			prev.id, err = res.LastInsertId()
			if err != nil {
				return summary, err
			}
			summary.NewFiles++
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			summary.BinarySkipped++
			continue
		}
		chunks, err := ChunkFile(ctx, file, string(data))
		if err != nil {
			return summary, err
		}
		for _, chunk := range chunks {
			if _, err := tx.ExecContext(ctx, "INSERT INTO code_chunks(file_id,start_line,end_line,symbol,kind,content_hash,embed_text) VALUES (?,?,?,?,?,?,?)",
				prev.id, chunk.StartLine, chunk.EndLine, chunk.Symbol, chunk.Kind, hashBytes([]byte(chunk.EmbedText)), chunk.EmbedText); err != nil {
				return summary, err
			}
		}
	}
	for _, prev := range existing {
		if _, err := tx.ExecContext(ctx, "DELETE FROM indexed_files WHERE id=?", prev.id); err != nil {
			return summary, err
		}
		summary.DeletedFiles++
	}
	if err := setMeta(ctx, tx, "last_head", summary.Head); err != nil {
		return summary, err
	}
	if err := setMeta(ctx, tx, "deny_hash", &r.cfg.ruleHash); err != nil {
		return summary, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM indexed_files").Scan(&summary.FilesTotal); err != nil {
		return summary, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM code_chunks").Scan(&summary.ChunksTotal); err != nil {
		return summary, err
	}
	if err := tx.Commit(); err != nil {
		return summary, err
	}
	summary.Committed = true
	if r.cfg.Retrieval.Mode == retrieval.ModeHybrid {
		summary.EmbeddedChunks, err = embedMissing(ctx, db, engine)
	}
	return summary, err
}

func loadFiles(ctx context.Context, tx *sql.Tx) (_ map[string]existingFile, err error) {
	rows, err := tx.QueryContext(ctx, "SELECT id,path,mtime,size,content_hash FROM indexed_files")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	files := map[string]existingFile{}
	for rows.Next() {
		var file string
		var prev existingFile
		if err := rows.Scan(&prev.id, &file, &prev.mtime, &prev.size, &prev.hash); err != nil {
			return nil, err
		}
		files[file] = prev
	}
	return files, rows.Err()
}

type vectorRow struct {
	id                            int64
	text, hash                    string
	model, storedHash, vectorHash sql.NullString
	dimension                     sql.NullInt64
	blob                          []byte
}

func readVectorRows(ctx context.Context, tx *sql.Tx) (_ []vectorRow, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.embed_text,c.content_hash,e.model_name,e.dimension,e.vector,e.content_hash,e.vector_hash
FROM code_chunks c LEFT JOIN code_embeddings e ON e.chunk_id=c.id ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []vectorRow
	for rows.Next() {
		var row vectorRow
		if err := rows.Scan(&row.id, &row.text, &row.hash, &row.model, &row.dimension, &row.blob, &row.storedHash, &row.vectorHash); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r vectorRow) current() bool {
	return r.model.String == embedding.EmbeddingModelName && r.dimension.Int64 == embedding.EmbeddingDim &&
		r.storedHash.String == r.hash && r.hash == hashBytes([]byte(r.text)) &&
		r.vectorHash.String == hashBytes(r.blob) && validVectorBlob(r.blob)
}

func embedMissing(ctx context.Context, db *sql.DB, engine retrieval.Inference) (count int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx, &err)
	rows, err := readVectorRows(ctx, tx)
	if err != nil {
		return 0, err
	}
	cache := map[string][]byte{}
	var missing []vectorRow
	for _, row := range rows {
		if row.current() {
			cache[row.hash] = row.blob
		} else {
			missing = append(missing, row)
		}
	}
	for _, row := range missing {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if row.hash != hashBytes([]byte(row.text)) {
			return 0, errors.New("corrupt code chunk text/hash; run `memdolt code rm` then `memdolt code index`")
		}
		blob, ok := cache[row.hash]
		if !ok {
			vec, err := engine.Embed(row.text)
			if err != nil {
				return 0, fmt.Errorf("embed code chunk: %w", err)
			}
			blob = make([]byte, len(vec)*4)
			if _, err := binary.Encode(blob, binary.LittleEndian, vec); err != nil {
				return 0, err
			}
			if !validVectorBlob(blob) {
				return 0, errors.New("code embedding must contain 384 finite components with nonzero norm")
			}
			cache[row.hash] = blob
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO code_embeddings(chunk_id,model_name,dimension,vector,content_hash,vector_hash) VALUES (?,?,?,?,?,?)
ON CONFLICT(chunk_id) DO UPDATE SET model_name=excluded.model_name,dimension=excluded.dimension,vector=excluded.vector,content_hash=excluded.content_hash,vector_hash=excluded.vector_hash`,
			row.id, embedding.EmbeddingModelName, embedding.EmbeddingDim, blob, row.hash, hashBytes(blob)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(missing), nil
}

func hashBytes(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func validVectorBlob(blob []byte) bool {
	if len(blob) != embedding.EmbeddingDim*4 {
		return false
	}
	var norm float64
	for i := 0; i < len(blob); i += 4 {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[i:])))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		norm += v * v
	}
	return norm > math.Nextafter(1, 2)-1
}

type localInference struct {
	ctx    context.Context
	engine *embedding.Engine
}

func (i *localInference) open() error {
	if err := i.ctx.Err(); err != nil {
		return err
	}
	if i.engine == nil {
		var err error
		i.engine, err = embedding.Open(i.ctx, embedding.Options{})
		return err
	}
	return nil
}

func (i *localInference) Embed(text string) ([]float32, error) {
	if err := i.open(); err != nil {
		return nil, err
	}
	return i.engine.Embed(text)
}

func (i *localInference) Rerank(query, text string) (float32, error) {
	if err := i.open(); err != nil {
		return 0, err
	}
	return i.engine.Rerank(query, text)
}

func (i *localInference) close() error {
	if i.engine != nil {
		return i.engine.Close()
	}
	return nil
}

// Status is read-only, including a missing index. It neither initializes an
// index nor takes/creates an application lock; SQLite provides one snapshot.
func Status(ctx context.Context, start string) (report StatusReport, err error) {
	r, err := openRepository(start, false)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	report.Path, report.Mode, report.CurrentHead = r.indexPath(), r.cfg.Retrieval.Mode, currentHead(ctx, r.base)
	db, _, err := r.openDB(ctx, false)
	if err != nil || db == nil {
		return report, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	report.Exists = true
	report.SchemaVersion, err = storedVersion(ctx, db)
	if err != nil {
		return report, err
	}
	report.NeedsRebuild = needsRebuild(report.SchemaVersion) != nil
	if report.NeedsRebuild {
		return report, nil
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer rollback(tx, &err)
	for query, count := range map[string]*int{
		"SELECT COUNT(*) FROM indexed_files":   &report.FilesTotal,
		"SELECT COUNT(*) FROM code_chunks":     &report.ChunksTotal,
		"SELECT COUNT(*) FROM code_embeddings": &report.EmbeddingsTotal,
	} {
		if err := tx.QueryRowContext(ctx, query).Scan(count); err != nil {
			return report, err
		}
	}
	report.LastHead, err = metaValue(ctx, tx, "last_head")
	if err != nil {
		return report, err
	}
	report.HeadStale = report.LastHead != nil && report.CurrentHead != nil && *report.LastHead != *report.CurrentHead
	rows, err := readVectorRows(ctx, tx)
	if err != nil {
		return report, err
	}
	for _, row := range rows {
		if !row.current() {
			report.InvalidEmbeddings++
		}
	}
	return report, tx.Commit()
}

// Remove deletes only a recognized code index, under the same operation lock.
// Unknown files, links, journal residue and unrelated schema are preserved.
func Remove(ctx context.Context, start string) (result RemoveResult, err error) {
	r, err := openRepository(start, false)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	result.Path = r.indexPath()
	if r.metadata == nil {
		return result, nil
	}
	if err := r.acquire(); err != nil {
		return result, err
	}
	info, err := r.checkIndexFiles()
	if err != nil || info == nil {
		return result, err
	}
	db, _, err := r.openDB(ctx, false)
	if err != nil {
		return result, err
	}
	err = checkOwnedSchema(ctx, db)
	err = errors.Join(err, db.Close())
	if err != nil {
		return result, err
	}
	current, err := r.checkIndexFiles()
	if err != nil {
		return result, err
	}
	if current == nil || !os.SameFile(info, current) {
		return result, errors.New("code index changed before removal; file retained")
	}
	if err := r.metadata.Remove(indexName); err != nil {
		return result, err
	}
	result.Removed = true
	return result, nil
}
