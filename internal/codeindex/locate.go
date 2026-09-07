package codeindex

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/retrieval"
)

const DefaultLimit = 10

type Options struct {
	Query       string
	Limit       int
	UseReranker bool
	NoRefresh   bool
}

type Hit struct {
	Rank        int      `json:"rank"`
	Path        string   `json:"path"`
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
	Symbol      *string  `json:"symbol"`
	Kind        string   `json:"kind"`
	Score       float64  `json:"score"`
	FTSScore    float64  `json:"ftsScore"`
	VectorScore float64  `json:"vectorScore"`
	RerankScore *float32 `json:"rerankScore,omitempty"`
	Snippet     string   `json:"snippet"`
}

type Response struct {
	Query             string          `json:"query"`
	Mode              retrieval.Mode  `json:"mode"`
	Results           []Hit           `json:"results"`
	CandidateCount    int             `json:"candidateCount"`
	ReturnedCount     int             `json:"returnedCount"`
	Reranked          bool            `json:"reranked"`
	FilesTotal        int             `json:"filesTotal"`
	ChunksTotal       int             `json:"chunksTotal"`
	Head              *string         `json:"head"`
	CorruptEmbeddings int             `json:"corruptEmbeddings"`
	ElapsedMS         int64           `json:"elapsedMs"`
	Refresh           *RefreshSummary `json:"refresh,omitempty"`
	NoRefresh         bool            `json:"noRefresh"`
	Warnings          []string        `json:"warnings"`
}

// Locate is the production CLI/MCP/eval path. A nil engine selects verified
// local inference. Explicit no-refresh retains old ranking/line metadata but
// still checks current paths, owner-file identity and deny rules before reads.
func Locate(ctx context.Context, start string, engine retrieval.Inference, opts Options) (response Response, err error) {
	started := time.Now()
	if strings.TrimSpace(opts.Query) == "" || !utf8.ValidString(opts.Query) || strings.ContainsRune(opts.Query, 0) {
		return response, errors.New("locate query must be nonempty UTF-8 without NUL")
	}
	if opts.Limit < 0 {
		return response, errors.New("locate limit must be nonnegative")
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultLimit
	}
	r, err := openRepository(start, !opts.NoRefresh)
	if err != nil {
		return response, err
	}
	defer func() { err = errors.Join(err, r.close()) }()
	if err := r.acquire(); err != nil {
		return response, err
	}
	db, created, err := r.openDB(ctx, !opts.NoRefresh)
	if err != nil {
		return response, err
	}
	if db == nil {
		return response, errors.New("code index is missing; run `memdolt code index` or omit --no-refresh")
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	lazy := &localInference{ctx: ctx}
	if engine == nil {
		engine = lazy
	}
	defer func() { err = errors.Join(err, lazy.close()) }()
	response = Response{Query: opts.Query, Mode: r.cfg.Retrieval.Mode, Results: []Hit{}, Warnings: []string{}, NoRefresh: opts.NoRefresh}
	if !opts.NoRefresh {
		if err := bootstrap(ctx, db, created); err != nil {
			return response, err
		}
		summary, err := r.refresh(ctx, db, engine)
		response.Refresh = &summary
		if err != nil {
			return response, err
		}
	} else {
		v, err := storedVersion(ctx, db)
		if err != nil {
			return response, err
		}
		if err := needsRebuild(v); err != nil {
			return response, err
		}
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return response, err
	}
	defer rollback(tx, &err)
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM indexed_files").Scan(&response.FilesTotal); err != nil {
		return response, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM code_chunks").Scan(&response.ChunksTotal); err != nil {
		return response, err
	}
	response.Head, err = metaValue(ctx, tx, "last_head")
	if err != nil {
		return response, err
	}
	candidates, invalid, err := r.gather(ctx, tx, engine, opts.Query)
	if err != nil {
		return response, err
	}
	response.CandidateCount, response.CorruptEmbeddings = len(candidates), invalid
	if invalid > 0 {
		response.Warnings = append(response.Warnings, "invalid or missing code vectors excluded; run `memdolt code index`")
	}
	scoreCandidates(candidates, r.cfg.Code)
	slices.SortFunc(candidates, compareFusion)
	if opts.UseReranker && response.Mode == retrieval.ModeHybrid && len(candidates) > 0 {
		pool := min(len(candidates), max(opts.Limit, r.cfg.Retrieval.RerankPool))
		for i := range pool {
			if err := ctx.Err(); err != nil {
				return response, err
			}
			logit, err := engine.Rerank(opts.Query, candidates[i].text)
			if err != nil {
				return response, fmt.Errorf("rerank code chunk: %w", err)
			}
			if math.IsNaN(float64(logit)) || math.IsInf(float64(logit), 0) {
				return response, errors.New("code reranker returned a non-finite score")
			}
			candidates[i].RerankScore = &logit
		}
		// Stable ties preserve the deterministic fusion/id ordering, as baseline.
		slices.SortStableFunc(candidates[:pool], func(a, b candidate) int {
			return compareFloat(float64(*b.RerankScore), float64(*a.RerankScore))
		})
		response.Reranked = true
	}
	candidates = candidates[:min(opts.Limit, len(candidates))]
	cache := map[string][]string{}
	for _, candidate := range candidates {
		lines, ok := cache[candidate.Path]
		if !ok {
			f, info, openErr := r.openSource(candidate.Path)
			if openErr != nil {
				if opts.NoRefresh && os.IsNotExist(openErr) {
					response.Warnings = append(response.Warnings, "a stale-by-choice source is absent; its snippet is empty")
				} else {
					return response, fmt.Errorf("locate source refused: %w", openErr)
				}
			} else {
				data, readErr := readSource(f, info)
				if err := errors.Join(readErr, f.Close()); err != nil {
					return response, err
				}
				if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) || r.cfg.denied.Check(string(data)) != nil {
					return response, errors.New("locate source is binary or denied; no snippet returned")
				}
				lines = sourceLines(string(data))
			}
			cache[candidate.Path] = lines
		}
		hit := candidate.Hit
		hit.Rank = len(response.Results) + 1
		hit.Snippet = clipSnippet(lines, hit.StartLine, hit.EndLine)
		response.Results = append(response.Results, hit)
	}
	response.ReturnedCount, response.ElapsedMS = len(response.Results), time.Since(started).Milliseconds()
	return response, tx.Commit()
}

type candidate struct {
	Hit
	id   int64
	text string
	fts  *float64
}

func (r *repository) gather(ctx context.Context, tx *sql.Tx, engine retrieval.Inference, query string) (_ []candidate, invalid int, err error) {
	fts := map[int64]float64{}
	if match := ftsMatch(query); match != "" {
		rows, err := tx.QueryContext(ctx, `SELECT c.id,bm25(code_chunks_fts) AS score
FROM code_chunks_fts JOIN code_chunks c ON c.id=code_chunks_fts.rowid
WHERE code_chunks_fts MATCH ? ORDER BY score,c.id LIMIT 100`, match)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var id int64
			var raw float64
			if err := rows.Scan(&id, &raw); err != nil {
				return nil, 0, errors.Join(err, rows.Close())
			}
			fts[id] = raw
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, 0, err
		}
	}
	var queryVector []float32
	if r.cfg.Retrieval.Mode == retrieval.ModeHybrid {
		queryVector, err = engine.Embed(query)
		if err != nil {
			return nil, 0, fmt.Errorf("embed locate query: %w", err)
		}
		if len(queryVector) != embedding.EmbeddingDim || !finiteVector(queryVector) {
			return nil, 0, errors.New("locate query embedding must have 384 finite components with nonzero norm")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.embed_text,c.content_hash,e.model_name,e.dimension,e.vector,e.content_hash,e.vector_hash,
f.path,c.start_line,c.end_line,c.symbol,c.kind FROM code_chunks c JOIN indexed_files f ON f.id=c.file_id
LEFT JOIN code_embeddings e ON e.chunk_id=c.id ORDER BY c.id`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var candidates []candidate
	for rows.Next() {
		var row vectorRow
		var hit Hit
		if err := rows.Scan(&row.id, &row.text, &row.hash, &row.model, &row.dimension, &row.blob, &row.storedHash, &row.vectorHash,
			&hit.Path, &hit.StartLine, &hit.EndLine, &hit.Symbol, &hit.Kind); err != nil {
			return nil, 0, err
		}
		if validateSourcePath(hit.Path) != nil || !indexable(hit.Path) || hit.StartLine < 1 || hit.EndLine < hit.StartLine || row.hash != hashBytes([]byte(row.text)) {
			return nil, 0, errors.New("unsafe or corrupt code-index row; run `memdolt code rm` then `memdolt code index`")
		}
		if defaultDenied(hit.Path) || r.cfg.denied.Check(hit.Path, row.text) != nil {
			continue // Updated deny rules also apply to stale-by-choice queries.
		}
		c := candidate{Hit: hit, id: row.id, text: row.text}
		if raw, ok := fts[row.id]; ok {
			c.fts = &raw
		}
		current := false
		if queryVector != nil {
			current = row.current()
			if current {
				vec := make([]float32, embedding.EmbeddingDim)
				if _, err := binary.Decode(row.blob, binary.LittleEndian, vec); err != nil {
					return nil, 0, err
				}
				c.VectorScore = max(0, min(1, cosine(queryVector, vec)))
			} else {
				invalid++
			}
		}
		if c.fts != nil || current {
			candidates = append(candidates, c)
		}
	}
	return candidates, invalid, rows.Err()
}

func finiteVector(vec []float32) bool {
	var norm float64
	for _, component := range vec {
		v := float64(component)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		norm += v * v
	}
	return norm > math.Nextafter(1, 2)-1
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i, x := range a {
		xf, yf := float64(x), float64(b[i])
		dot += xf * yf
		na += xf * xf
		nb += yf * yf
	}
	if na <= math.Nextafter(1, 2)-1 || nb <= math.Nextafter(1, 2)-1 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func ftsMatch(query string) string {
	var terms []string
	for _, term := range strings.Fields(query) {
		term = strings.Trim(term, "\"',.:;")
		if term != "" {
			terms = append(terms, "\""+strings.ReplaceAll(term, "\"", "\"\"")+"\"")
		}
	}
	return strings.Join(terms, " AND ")
}

func scoreCandidates(candidates []candidate, cfg Config) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, c := range candidates {
		if c.fts != nil {
			lo, hi = min(lo, -*c.fts), max(hi, -*c.fts)
		}
	}
	for i := range candidates {
		c := &candidates[i]
		if c.fts != nil {
			c.FTSScore = normalizeFTS(-*c.fts, lo, hi)
		}
		c.Score = cfg.FTSWeight*c.FTSScore + cfg.VectorWeight*c.VectorScore
		if isTestPath(c.Path) {
			c.Score *= cfg.TestPathPenalty
		}
	}
}

func normalizeFTS(value, lo, hi float64) float64 {
	if math.IsNaN(value) || math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(value, 0) || math.IsInf(lo, 0) || math.IsInf(hi, 0) {
		return 0
	}
	if math.Abs(hi-lo) < math.Nextafter(1, 2)-1 {
		return 1
	}
	return max(0, min(1, (value-lo)/(hi-lo)))
}

func compareFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareFusion(a, b candidate) int {
	if order := compareFloat(b.Score, a.Score); order != 0 {
		return order
	}
	if a.id < b.id {
		return -1
	}
	if a.id > b.id {
		return 1
	}
	return 0
}

func clipSnippet(lines []string, start, end int) string {
	if start < 1 || start > len(lines) || end < start {
		return ""
	}
	to := min(end, len(lines), start+5)
	text := strings.Join(lines[start-1:to], "\n")
	runes := []rune(text)
	if len(runes) > 400 || to < min(end, len(lines)) {
		// Include the ellipsis in the cap, rather than producing a 401st char.
		return string(runes[:min(len(runes), 399)]) + "…"
	}
	return text
}
