package embedding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kninetimmy/memdolt/internal/store"
	_ "modernc.org/sqlite"
)

const embeddingsSchema = `CREATE TABLE IF NOT EXISTS embeddings (
  source_type TEXT NOT NULL,
  source_id TEXT NOT NULL,
  model_name TEXT NOT NULL,
  vector BLOB NOT NULL,
  content_hash TEXT NOT NULL,
  dimension INTEGER NOT NULL,
  PRIMARY KEY (source_type, source_id, model_name)
) WITHOUT ROWID`

const recallObservabilitySchema = `CREATE TABLE IF NOT EXISTS recall_observability (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  recall_count INTEGER NOT NULL,
  empty_count INTEGER NOT NULL
)`

const RebuildRemedy = "run `memdolt index rebuild`"

// RebuildResult reports the derived rows changed by Rebuild. Unchanged rows do
// not invoke the embedding function or receive a SQLite UPDATE.
type RebuildResult struct {
	Path      string `json:"path"`
	ModelName string `json:"modelName"`
	Dimension int    `json:"dimension"`
	Eligible  int    `json:"eligible"`
	Created   int    `json:"created"`
	Refreshed int    `json:"refreshed"`
	Unchanged int    `json:"unchanged"`
	Removed   int    `json:"removed"`
}

// StatusState classifies one expected or orphaned side-store row.
type StatusState string

const (
	StatusCurrent               StatusState = "current"
	StatusMissing               StatusState = "missing"
	StatusContentHashMismatched StatusState = "content_hash_mismatched"
	StatusWrongByteLength       StatusState = "wrong_byte_length"
	StatusOrphaned              StatusState = "orphaned"
)

// StatusEntry describes one source's active-model embedding, or one orphaned
// row that a rebuild will remove.
type StatusEntry struct {
	SourceType      string      `json:"sourceType"`
	SourceID        string      `json:"sourceId"`
	ModelName       string      `json:"modelName"`
	State           StatusState `json:"state"`
	ExpectedHash    string      `json:"expectedHash,omitempty"`
	ActualHash      string      `json:"actualHash,omitempty"`
	ExpectedBytes   int         `json:"expectedBytes,omitempty"`
	ActualBytes     int         `json:"actualBytes,omitempty"`
	ActualDimension int         `json:"actualDimension,omitempty"`
}

// StatusReport compares committed source text with the derived side-store.
type StatusReport struct {
	Path                  string        `json:"path"`
	ModelName             string        `json:"modelName"`
	Dimension             int           `json:"dimension"`
	Eligible              int           `json:"eligible"`
	Current               int           `json:"current"`
	Missing               int           `json:"missing"`
	ContentHashMismatched int           `json:"contentHashMismatched"`
	WrongByteLength       int           `json:"wrongByteLength"`
	Orphaned              int           `json:"orphaned"`
	NeedsRebuild          bool          `json:"needsRebuild"`
	Remedy                string        `json:"remedy,omitempty"`
	Entries               []StatusEntry `json:"entries"`
}

type rowKey struct {
	sourceType string
	sourceID   string
	modelName  string
}

type storedEmbedding struct {
	contentHash string
	dimension   int
	byteLength  int
}

// Vector is one current active-model vector read from the derived side-store.
// Stale and malformed rows are never returned here; StatusReport explains why
// each one was skipped.
type Vector struct {
	SourceType string
	SourceID   string
	Values     []float32
}

// Observability reports the local empty-recall count and rate. It is derived,
// machine-local state in embeddings.sqlite, never a Dolt row or commit.
type Observability struct {
	RecallCount int64   `json:"recallCount"`
	EmptyCount  int64   `json:"emptyCount"`
	EmptyRate   float64 `json:"emptyRate"`
}

// Rebuild creates or refreshes the active model's vectors, skips rows whose
// hash and shape are already current, and removes rows whose source no longer
// exists. It writes only path's SQLite side-store; sources are values already
// read from Dolt and cannot be modified by this function.
func Rebuild(ctx context.Context, path string, sources []store.EmbeddingSource, embed func(string) ([]float32, error)) (result RebuildResult, err error) {
	result = RebuildResult{Path: path, ModelName: EmbeddingModelName, Dimension: EmbeddingDim, Eligible: len(sources)}
	if embed == nil {
		return result, errors.New("embedding index rebuild requires an embedding function")
	}
	if err := validateSources(sources); err != nil {
		return result, err
	}
	if path == "" {
		return result, errors.New("embedding side-store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, fmt.Errorf("create embedding side-store directory: %w", err)
	}
	dsn, err := sqliteURI(path, false)
	if err != nil {
		return result, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return result, fmt.Errorf("open embedding side-store %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := initializeSideStore(ctx, db); err != nil {
		return result, fmt.Errorf("initialize embedding side-store %s: %w", path, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin embedding index rebuild: %w", err)
	}
	defer func() {
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, rollbackErr)
			}
		}
	}()
	existing, err := readStoredEmbeddings(ctx, tx)
	if err != nil {
		return result, err
	}

	wantedSources := make(map[[2]string]bool, len(sources))
	for _, source := range sources {
		key := rowKey{source.SourceType, source.SourceID, EmbeddingModelName}
		wantedSources[[2]string{source.SourceType, source.SourceID}] = true
		hash := contentHash(source.Text)
		if row, ok := existing[key]; ok && row.contentHash == hash && row.dimension == EmbeddingDim && row.byteLength == EmbeddingDim*4 {
			result.Unchanged++
			continue
		}

		vector, embedErr := embed(source.Text)
		if embedErr != nil {
			return result, fmt.Errorf("embed %s/%s: %w", source.SourceType, source.SourceID, embedErr)
		}
		blob, encodeErr := encodeVector(vector)
		if encodeErr != nil {
			return result, fmt.Errorf("embed %s/%s: %w", source.SourceType, source.SourceID, encodeErr)
		}
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO embeddings
  (source_type, source_id, model_name, vector, content_hash, dimension)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (source_type, source_id, model_name) DO UPDATE SET
  vector = excluded.vector,
  content_hash = excluded.content_hash,
  dimension = excluded.dimension`,
			source.SourceType, source.SourceID, EmbeddingModelName, blob, hash, EmbeddingDim); execErr != nil {
			return result, fmt.Errorf("store embedding %s/%s: %w", source.SourceType, source.SourceID, execErr)
		}
		if _, ok := existing[key]; ok {
			result.Refreshed++
		} else {
			result.Created++
		}
	}

	for key := range existing {
		if wantedSources[[2]string{key.sourceType, key.sourceID}] {
			continue
		}
		res, execErr := tx.ExecContext(ctx,
			"DELETE FROM embeddings WHERE source_type = ? AND source_id = ? AND model_name = ?",
			key.sourceType, key.sourceID, key.modelName)
		if execErr != nil {
			return result, fmt.Errorf("remove orphaned embedding %s/%s/%s: %w", key.sourceType, key.sourceID, key.modelName, execErr)
		}
		removed, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return result, fmt.Errorf("count removed orphaned embedding %s/%s/%s: %w", key.sourceType, key.sourceID, key.modelName, rowsErr)
		}
		result.Removed += int(removed)
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit embedding index rebuild: %w", err)
	}
	return result, nil
}

// CurrentVectors returns only hash- and shape-current vectors for sources.
// Its StatusReport retains every missing, drifted, malformed, and orphaned
// classification so retrieval can warn instead of silently degrading.
func CurrentVectors(ctx context.Context, path string, sources []store.EmbeddingSource) ([]Vector, StatusReport, error) {
	report, err := Status(ctx, path, sources)
	if err != nil || report.Current == 0 {
		return nil, report, err
	}
	current := make(map[[2]string]bool, report.Current)
	for _, entry := range report.Entries {
		if entry.State == StatusCurrent {
			current[[2]string{entry.SourceType, entry.SourceID}] = true
		}
	}
	dsn, err := sqliteURI(path, true)
	if err != nil {
		return nil, report, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, report, fmt.Errorf("open embedding side-store %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT source_type, source_id, vector
FROM embeddings WHERE model_name = ? AND dimension = ? ORDER BY source_type, source_id`,
		EmbeddingModelName, EmbeddingDim)
	if err != nil {
		return nil, report, fmt.Errorf("read current embedding vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	vectors := make([]Vector, 0, report.Current)
	for rows.Next() {
		var vector Vector
		var blob []byte
		if err := rows.Scan(&vector.SourceType, &vector.SourceID, &blob); err != nil {
			return nil, report, fmt.Errorf("scan current embedding vector: %w", err)
		}
		if !current[[2]string{vector.SourceType, vector.SourceID}] {
			continue
		}
		vector.Values = decodeVector(blob)
		vectors = append(vectors, vector)
	}
	if err := rows.Err(); err != nil {
		return nil, report, fmt.Errorf("read current embedding vectors: %w", err)
	}
	return vectors, report, nil
}

// RecordRecall increments the local denominator for every call and the empty
// numerator when no result cleared the configured floors. It initializes the
// existing side-store, never the Dolt source store.
func RecordRecall(ctx context.Context, path string, empty bool) (err error) {
	if path == "" {
		return errors.New("embedding side-store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create embedding side-store directory: %w", err)
	}
	dsn, err := sqliteURI(path, false)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open recall observability store %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := initializeSideStore(ctx, db); err != nil {
		return fmt.Errorf("initialize recall observability store %s: %w", path, err)
	}
	emptyIncrement := 0
	if empty {
		emptyIncrement = 1
	}
	_, err = db.ExecContext(ctx, `INSERT INTO recall_observability (id, recall_count, empty_count)
VALUES (1, 1, ?) ON CONFLICT (id) DO UPDATE SET
recall_count = recall_count + 1, empty_count = empty_count + excluded.empty_count`, emptyIncrement)
	if err != nil {
		return fmt.Errorf("record recall observability: %w", err)
	}
	return nil
}

// ReadObservability reads the local counters without creating a missing file
// or upgrading an older side-store that has no observability table yet.
func ReadObservability(ctx context.Context, path string) (result Observability, err error) {
	if path == "" {
		return result, errors.New("embedding side-store path is required")
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return result, nil
	} else if statErr != nil {
		return result, fmt.Errorf("stat recall observability store %s: %w", path, statErr)
	}
	dsn, err := sqliteURI(path, true)
	if err != nil {
		return result, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return result, fmt.Errorf("open recall observability store %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'recall_observability'").Scan(&tables); err != nil {
		return result, fmt.Errorf("look for recall observability counters: %w", err)
	}
	if tables == 0 {
		return result, nil
	}
	err = db.QueryRowContext(ctx,
		"SELECT recall_count, empty_count FROM recall_observability WHERE id = 1").
		Scan(&result.RecallCount, &result.EmptyCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Observability{}, nil
	}
	if err != nil {
		return result, fmt.Errorf("read recall observability counters: %w", err)
	}
	if result.RecallCount > 0 {
		result.EmptyRate = float64(result.EmptyCount) / float64(result.RecallCount)
	}
	return result, nil
}

// Status compares every source with its active-model row. It never creates or
// changes the side-store; a missing file is reported as missing embeddings.
func Status(ctx context.Context, path string, sources []store.EmbeddingSource) (report StatusReport, err error) {
	report = StatusReport{
		Path:      path,
		ModelName: EmbeddingModelName,
		Dimension: EmbeddingDim,
		Eligible:  len(sources),
		Entries:   []StatusEntry{},
	}
	if err := validateSources(sources); err != nil {
		return report, err
	}
	if path == "" {
		return report, errors.New("embedding side-store path is required")
	}

	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		for _, source := range sources {
			report.add(sourceStatus(source, storedEmbedding{}, false))
		}
		report.finish()
		return report, nil
	} else if statErr != nil {
		return report, fmt.Errorf("stat embedding side-store %s: %w", path, statErr)
	}

	dsn, err := sqliteURI(path, true)
	if err != nil {
		return report, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return report, fmt.Errorf("open embedding side-store %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	existing, err := readStoredEmbeddings(ctx, db)
	if err != nil {
		return report, err
	}

	wantedSources := make(map[[2]string]bool, len(sources))
	for _, source := range sources {
		wantedSources[[2]string{source.SourceType, source.SourceID}] = true
		key := rowKey{source.SourceType, source.SourceID, EmbeddingModelName}
		row, ok := existing[key]
		report.add(sourceStatus(source, row, ok))
	}
	for key, row := range existing {
		if wantedSources[[2]string{key.sourceType, key.sourceID}] {
			continue
		}
		report.add(StatusEntry{
			SourceType:      key.sourceType,
			SourceID:        key.sourceID,
			ModelName:       key.modelName,
			State:           StatusOrphaned,
			ActualHash:      row.contentHash,
			ActualBytes:     row.byteLength,
			ActualDimension: row.dimension,
		})
	}
	report.finish()
	return report, nil
}

type embeddingRows interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readStoredEmbeddings(ctx context.Context, q embeddingRows) (map[rowKey]storedEmbedding, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT source_type, source_id, model_name, content_hash, dimension, length(vector) FROM embeddings ORDER BY source_type, source_id, model_name")
	if err != nil {
		return nil, fmt.Errorf("read embedding side-store: %w", err)
	}
	defer func() { _ = rows.Close() }()
	existing := make(map[rowKey]storedEmbedding)
	for rows.Next() {
		var key rowKey
		var row storedEmbedding
		if err := rows.Scan(&key.sourceType, &key.sourceID, &key.modelName, &row.contentHash, &row.dimension, &row.byteLength); err != nil {
			return nil, fmt.Errorf("scan embedding side-store: %w", err)
		}
		existing[key] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read embedding side-store: %w", err)
	}
	return existing, nil
}

func validateSources(sources []store.EmbeddingSource) error {
	seen := make(map[[2]string]bool, len(sources))
	for _, source := range sources {
		if source.SourceType == "" || source.SourceID == "" {
			return errors.New("embedding source type and id are required")
		}
		key := [2]string{source.SourceType, source.SourceID}
		if seen[key] {
			return fmt.Errorf("duplicate embedding source %s/%s", source.SourceType, source.SourceID)
		}
		seen[key] = true
	}
	return nil
}

func sourceStatus(source store.EmbeddingSource, row storedEmbedding, exists bool) StatusEntry {
	expectedHash := contentHash(source.Text)
	entry := StatusEntry{
		SourceType:    source.SourceType,
		SourceID:      source.SourceID,
		ModelName:     EmbeddingModelName,
		ExpectedHash:  expectedHash,
		ExpectedBytes: EmbeddingDim * 4,
	}
	if !exists {
		entry.State = StatusMissing
		return entry
	}
	entry.ActualHash = row.contentHash
	entry.ActualBytes = row.byteLength
	entry.ActualDimension = row.dimension
	switch {
	case row.contentHash != expectedHash:
		entry.State = StatusContentHashMismatched
	case row.dimension != EmbeddingDim || row.byteLength != EmbeddingDim*4:
		entry.State = StatusWrongByteLength
	default:
		entry.State = StatusCurrent
	}
	return entry
}

func (r *StatusReport) add(entry StatusEntry) {
	r.Entries = append(r.Entries, entry)
	switch entry.State {
	case StatusCurrent:
		r.Current++
	case StatusMissing:
		r.Missing++
	case StatusContentHashMismatched:
		r.ContentHashMismatched++
	case StatusWrongByteLength:
		r.WrongByteLength++
	case StatusOrphaned:
		r.Orphaned++
	}
}

func (r *StatusReport) finish() {
	sort.Slice(r.Entries, func(i, j int) bool {
		left, right := r.Entries[i], r.Entries[j]
		if left.SourceType != right.SourceType {
			return left.SourceType < right.SourceType
		}
		if left.SourceID != right.SourceID {
			return left.SourceID < right.SourceID
		}
		return left.ModelName < right.ModelName
	})
	r.NeedsRebuild = r.Missing+r.ContentHashMismatched+r.WrongByteLength+r.Orphaned > 0
	if r.NeedsRebuild {
		r.Remedy = RebuildRemedy
	}
}

func contentHash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func encodeVector(vector []float32) ([]byte, error) {
	if len(vector) != EmbeddingDim {
		return nil, fmt.Errorf("embedding has dimension %d, want %d", len(vector), EmbeddingDim)
	}
	blob := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(value))
	}
	return blob, nil
}

func decodeVector(blob []byte) []float32 {
	vector := make([]float32, len(blob)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vector
}

func initializeSideStore(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{embeddingsSchema, recallObservabilitySchema} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func sqliteURI(path string, readOnly bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve embedding side-store %s: %w", path, err)
	}
	path = filepath.ToSlash(abs)
	if filepath.VolumeName(abs) != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := &url.URL{Scheme: "file", Path: path}
	if readOnly {
		query := u.Query()
		query.Set("mode", "ro")
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}
