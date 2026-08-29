package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

var validSourceTypes = map[string]bool{
	"fact": true, "decision": true, "task": true, "doc_chunk": true,
}

var defaultSourceTypes = []string{"fact", "decision", "task"}

// Inference is the production embedding.Engine surface recall consumes.
// Keeping the boundary to these two calls lets deterministic tests exercise
// the complete retrieval path without a second implementation of its math.
type Inference interface {
	Embed(string) ([]float32, error)
	Rerank(string, string) (float32, error)
}

// Options contains per-call overrides. Zero values use Config defaults.
type Options struct {
	Query          string
	Mode           Mode
	MaxResults     int
	SourceTypes    []string
	IncludeStale   *bool
	AcceptedOnly   *bool
	UseReranker    *bool
	MinRerankScore *float32
	Provenance     bool
}

type Warning struct {
	Kind       string `json:"kind"`
	StaleCount int    `json:"staleCount"`
	TotalCount int    `json:"totalCount"`
	Reason     string `json:"reason"`
	Fix        string `json:"fix"`
}

type Hit struct {
	Rank         int                     `json:"rank"`
	SourceType   string                  `json:"sourceType"`
	SourceID     string                  `json:"sourceId"`
	Title        string                  `json:"title"`
	Body         string                  `json:"body"`
	Score        float64                 `json:"score"`
	FTSScore     float64                 `json:"ftsScore"`
	VectorScore  float64                 `json:"vectorScore"`
	Stale        bool                    `json:"stale"`
	SupersededBy string                  `json:"supersededBy,omitempty"`
	Source       string                  `json:"source,omitempty"`
	CreatedAt    time.Time               `json:"createdAt"`
	RerankScore  *float32                `json:"rerankScore,omitempty"`
	Kind         string                  `json:"kind,omitempty"`
	LastChanged  *store.CommitProvenance `json:"lastChanged,omitempty"`
}

type Response struct {
	Query          string    `json:"query"`
	Mode           Mode      `json:"mode"`
	Results        []Hit     `json:"results"`
	Warnings       []Warning `json:"warnings"`
	Matcher        string    `json:"matcher"`
	CandidateCount int       `json:"candidateCount"`
	ReturnedCount  int       `json:"returnedCount"`
	ElapsedMS      int64     `json:"elapsedMs"`
	AvailableDocs  int       `json:"availableDocs"`
}

type resolvedOptions struct {
	query, matcher                           string
	mode                                     Mode
	maxResults, rerankPool                   int
	sourceTypes                              []string
	includeStale, acceptedOnly, useReranker  bool
	minRerankScore, docMinRerankScore        float32
	docsViaDefault, explicitlyDocumentScoped bool
	provenance                               bool
}

type sourceKey struct{ sourceType, sourceID string }

type candidate struct {
	source      store.RecallSource
	ftsRaw      *float64
	ftsScore    float64
	vectorScore float64
	score       float64
	stale       bool
	ageDays     *float64
	rerankScore *float32
}

// Recall executes the production retrieval path. Normal hybrid assembly is
// vector-only; lexical candidates are consulted in hybrid mode only for rows
// whose derived vector is missing or invalid, and that exception is always
// accompanied by stale_embeddings.
func Recall(ctx context.Context, st *localdolt.Store, embeddingsPath string, inference Inference, cfg Config, options Options) (Response, error) {
	started := time.Now()
	resolved, err := resolve(cfg, options)
	if err != nil {
		return Response{}, err
	}
	sources, err := st.RecallSources(ctx)
	if err != nil {
		return Response{}, err
	}
	docTotal := 0
	for _, source := range sources {
		if source.SourceType == "doc_chunk" {
			docTotal++
		}
	}

	now := time.Now().UTC()
	allowed := make(map[string]bool, len(resolved.sourceTypes))
	for _, sourceType := range resolved.sourceTypes {
		allowed[sourceType] = true
	}
	sourceByKey := make(map[sourceKey]store.RecallSource)
	ageByKey := make(map[sourceKey]*float64)
	staleByKey := make(map[sourceKey]bool)
	for _, source := range sources {
		if !allowed[source.SourceType] || (resolved.acceptedOnly && !acceptedSource(source.Source)) {
			continue
		}
		key := sourceKey{source.SourceType, source.SourceID}
		stale, age := sourceAge(source, now, cfg.FactStaleAfterDays)
		if stale && !resolved.includeStale {
			continue
		}
		sourceByKey[key] = source
		ageByKey[key] = age
		staleByKey[key] = stale
	}

	candidates := map[sourceKey]*candidate{}
	warnings := []Warning{}
	if resolved.mode == ModeFTS {
		lexical, err := st.RecallFTS(ctx, resolved.query, resolved.sourceTypes)
		if err != nil {
			return Response{}, err
		}
		addLexicalCandidates(candidates, sourceByKey, lexical)
	} else {
		if inference == nil {
			return Response{}, errors.New("retrieval: hybrid recall requires inference")
		}
		embeddingSources, err := st.EmbeddingSources(ctx)
		if err != nil {
			return Response{}, err
		}
		eligibleEmbeddings := make([]store.EmbeddingSource, 0, len(sourceByKey))
		for _, source := range embeddingSources {
			if _, ok := sourceByKey[sourceKey{source.SourceType, source.SourceID}]; ok {
				eligibleEmbeddings = append(eligibleEmbeddings, source)
			}
		}
		vectors, status, err := embedding.CurrentVectors(ctx, embeddingsPath, eligibleEmbeddings)
		if err != nil {
			return Response{}, err
		}
		staleKeys := map[sourceKey]bool{}
		for _, entry := range status.Entries {
			switch entry.State {
			case embedding.StatusMissing, embedding.StatusContentHashMismatched, embedding.StatusWrongByteLength:
				staleKeys[sourceKey{entry.SourceType, entry.SourceID}] = true
			}
		}
		staleCount := status.Missing + status.ContentHashMismatched + status.WrongByteLength
		if staleCount > 0 {
			warnings = append(warnings, staleEmbeddingWarning(status, len(eligibleEmbeddings)))
		}

		queryVector, err := inference.Embed(resolved.query)
		if err != nil {
			return Response{}, fmt.Errorf("retrieval: embed query: %w", err)
		}
		if len(queryVector) != embedding.EmbeddingDim {
			return Response{}, fmt.Errorf("retrieval: query embedding dimension %d, want %d", len(queryVector), embedding.EmbeddingDim)
		}
		for _, vector := range vectors {
			key := sourceKey{vector.SourceType, vector.SourceID}
			source, ok := sourceByKey[key]
			if !ok {
				continue
			}
			cosine := cosineSimilarity(queryVector, vector.Values)
			if cosine >= 0 {
				candidates[key] = &candidate{source: source, vectorScore: clamp01(cosine)}
			}
		}

		// Stale-vector fallback is deliberately narrow: lexical hits add only
		// rows that could not enter through a current vector. Current-vector
		// candidates never receive a lexical score in hybrid mode.
		if len(staleKeys) > 0 {
			lexical, err := st.RecallFTS(ctx, resolved.query, resolved.sourceTypes)
			if err != nil {
				return Response{}, err
			}
			for _, hit := range lexical {
				key := sourceKey{hit.SourceType, hit.SourceID}
				if !staleKeys[key] {
					continue
				}
				addLexicalCandidate(candidates, sourceByKey, hit)
			}
		}
	}

	rows := make([]*candidate, 0, len(candidates))
	for key, candidate := range candidates {
		candidate.stale = staleByKey[key]
		candidate.ageDays = ageByKey[key]
		rows = append(rows, candidate)
	}
	scoreCandidates(rows, cfg.Scoring)
	sortCandidates(rows)
	candidateCount := len(rows)
	demoted := 0
	for _, row := range rows {
		if row.stale {
			demoted++
		}
	}
	if demoted > 0 {
		warnings = append(warnings, Warning{
			Kind: "stale_facts_demoted", StaleCount: demoted, TotalCount: candidateCount,
			Reason: fmt.Sprintf("%d stale fact(s) past the %d-day horizon were retained and demoted", demoted, cfg.FactStaleAfterDays),
			Fix:    "Re-verify or supersede stale facts; set retrieval.include_stale_by_default = false to exclude them.",
		})
	}

	reranked := resolved.mode == ModeHybrid && resolved.useReranker && len(rows) > 1
	if reranked {
		if len(rows) > resolved.rerankPool {
			rows = rows[:resolved.rerankPool]
		}
		type rerankedCandidate struct {
			row   *candidate
			score float32
			index int
		}
		rerankedRows := make([]rerankedCandidate, 0, len(rows))
		var highestDropped *float32
		for index, row := range rows {
			score, err := inference.Rerank(resolved.query, rerankText(row.source))
			if err != nil {
				return Response{}, fmt.Errorf("retrieval: rerank %s/%s: %w", row.source.SourceType, row.source.SourceID, err)
			}
			floor := resolved.minRerankScore
			if resolved.docsViaDefault && row.source.SourceType == "doc_chunk" {
				floor = resolved.docMinRerankScore
			}
			if score < floor {
				if highestDropped == nil || score > *highestDropped {
					copy := score
					highestDropped = &copy
				}
				continue
			}
			rerankedRows = append(rerankedRows, rerankedCandidate{row: row, score: score, index: index})
		}
		sort.SliceStable(rerankedRows, func(i, j int) bool {
			if rerankedRows[i].score != rerankedRows[j].score {
				return rerankedRows[i].score > rerankedRows[j].score
			}
			return rerankedRows[i].index < rerankedRows[j].index
		})
		rows = rows[:0]
		for _, reranked := range rerankedRows {
			score := reranked.score
			reranked.row.rerankScore = &score
			rows = append(rows, reranked.row)
		}
		if len(rows) == 0 && highestDropped != nil {
			warnings = append(warnings, Warning{
				Kind: "rerank_floor_dropped_all", TotalCount: candidateCount,
				Reason: fmt.Sprintf("candidates existed, but none cleared the configured rerank floor; highest dropped score was %.3f", *highestDropped),
				Fix:    "Refine the query or lower retrieval.scoring.min_rerank_score (and doc_min_rerank_score for default docs).",
			})
		}
	} else if resolved.docsViaDefault {
		filtered := rows[:0]
		for _, row := range rows {
			if row.source.SourceType != "doc_chunk" {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if len(rows) > resolved.maxResults {
		rows = rows[:resolved.maxResults]
	}

	results := make([]Hit, 0, len(rows))
	for index, row := range rows {
		hit := Hit{
			Rank: index + 1, SourceType: row.source.SourceType, SourceID: row.source.SourceID,
			Title: row.source.Title, Body: row.source.Body, Score: row.score,
			FTSScore: row.ftsScore, VectorScore: row.vectorScore, Stale: row.stale,
			SupersededBy: row.source.SupersededBy, Source: row.source.Source,
			CreatedAt: row.source.CreatedAt, RerankScore: row.rerankScore, Kind: row.source.Kind,
		}
		if resolved.provenance {
			hit.LastChanged, err = st.LastChanged(ctx, hit.SourceType, hit.SourceID)
			if err != nil {
				return Response{}, err
			}
		}
		results = append(results, hit)
	}
	availableDocs := docTotal
	if resolved.explicitlyDocumentScoped {
		availableDocs = 0
	} else if resolved.docsViaDefault {
		for _, hit := range results {
			if hit.SourceType == "doc_chunk" {
				availableDocs--
			}
		}
		if availableDocs < 0 {
			availableDocs = 0
		}
	}
	if err := embedding.RecordRecall(ctx, embeddingsPath, len(results) == 0); err != nil {
		return Response{}, err
	}
	matcher := resolved.matcher
	if reranked {
		matcher += "+rerank"
	}
	return Response{
		Query: resolved.query, Mode: resolved.mode, Results: results, Warnings: warnings,
		Matcher: matcher, CandidateCount: candidateCount, ReturnedCount: len(results),
		ElapsedMS: time.Since(started).Milliseconds(), AvailableDocs: availableDocs,
	}, nil
}

func resolve(cfg Config, options Options) (resolvedOptions, error) {
	if err := cfg.Validate(); err != nil {
		return resolvedOptions{}, fmt.Errorf("retrieval: %w", err)
	}
	query := strings.TrimSpace(options.Query)
	if query == "" {
		return resolvedOptions{}, errors.New("retrieval: recall query cannot be empty")
	}
	mode := options.Mode
	if mode == "" {
		mode = cfg.Mode
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return resolvedOptions{}, err
	}
	maxResults := options.MaxResults
	if maxResults == 0 {
		maxResults = cfg.DefaultMaxResults
	}
	if maxResults < 1 {
		return resolvedOptions{}, errors.New("retrieval: max results must be greater than zero")
	}
	docsViaDefault := len(options.SourceTypes) == 0 && cfg.IncludeDocsInDefault
	sourceTypes := append([]string(nil), options.SourceTypes...)
	if len(sourceTypes) == 0 {
		sourceTypes = append([]string(nil), defaultSourceTypes...)
		if docsViaDefault {
			sourceTypes = append(sourceTypes, "doc_chunk")
		}
	}
	deduped := sourceTypes[:0]
	for _, sourceType := range sourceTypes {
		if sourceType == "doc" {
			sourceType = "doc_chunk"
		}
		if !validSourceTypes[sourceType] {
			return resolvedOptions{}, fmt.Errorf("retrieval: source type %q must be fact, decision, task, or doc_chunk", sourceType)
		}
		found := false
		for _, existing := range deduped {
			found = found || existing == sourceType
		}
		if !found {
			deduped = append(deduped, sourceType)
		}
	}
	includeStale := cfg.IncludeStaleByDefault
	if options.IncludeStale != nil {
		includeStale = *options.IncludeStale
	}
	acceptedOnly := cfg.AcceptedOnlyByDefault
	if options.AcceptedOnly != nil {
		acceptedOnly = *options.AcceptedOnly
	}
	useReranker := cfg.UseReranker
	if options.UseReranker != nil {
		useReranker = *options.UseReranker
	}
	minScore := cfg.Scoring.MinRerankScore
	if options.MinRerankScore != nil {
		minScore = *options.MinRerankScore
	}
	if !finite32(minScore) {
		return resolvedOptions{}, errors.New("retrieval: minimum rerank score must be finite")
	}
	pool := cfg.RerankCandidatePool
	if pool < maxResults {
		pool = maxResults
	}
	explicitDoc := false
	for _, sourceType := range deduped {
		explicitDoc = explicitDoc || (len(options.SourceTypes) > 0 && sourceType == "doc_chunk")
	}
	return resolvedOptions{
		query: query, mode: mode, matcher: "recall:" + string(mode), maxResults: maxResults,
		rerankPool: pool, sourceTypes: deduped, includeStale: includeStale,
		acceptedOnly: acceptedOnly, useReranker: useReranker, minRerankScore: minScore,
		docMinRerankScore: cfg.Scoring.DocMinRerankScore, docsViaDefault: docsViaDefault,
		explicitlyDocumentScoped: explicitDoc, provenance: options.Provenance,
	}, nil
}

func addLexicalCandidates(candidates map[sourceKey]*candidate, sources map[sourceKey]store.RecallSource, hits []store.LexicalHit) {
	for _, hit := range hits {
		addLexicalCandidate(candidates, sources, hit)
	}
}

func addLexicalCandidate(candidates map[sourceKey]*candidate, sources map[sourceKey]store.RecallSource, hit store.LexicalHit) {
	key := sourceKey{hit.SourceType, hit.SourceID}
	source, ok := sources[key]
	if !ok {
		return
	}
	if existing, ok := candidates[key]; ok {
		if existing.ftsRaw == nil || hit.Score > *existing.ftsRaw {
			score := hit.Score
			existing.ftsRaw = &score
		}
		return
	}
	score := hit.Score
	candidates[key] = &candidate{source: source, ftsRaw: &score}
}

func scoreCandidates(rows []*candidate, scoring ScoringConfig) {
	minFTS, maxFTS := 0.0, 0.0
	first := true
	for _, row := range rows {
		if row.ftsRaw == nil {
			continue
		}
		if first {
			minFTS, maxFTS, first = *row.ftsRaw, *row.ftsRaw, false
		} else {
			minFTS = math.Min(minFTS, *row.ftsRaw)
			maxFTS = math.Max(maxFTS, *row.ftsRaw)
		}
	}
	for _, row := range rows {
		if row.ftsRaw != nil {
			row.ftsScore = normalizeFTS(*row.ftsRaw, minFTS, maxFTS)
		}
		relevance := scoring.FTSWeight*row.ftsScore + scoring.VectorWeight*row.vectorScore
		row.score = relevance * ageDecay(row.ageDays, scoring.AgeHalfLifeDays)
		if row.stale {
			row.score -= scoring.StalePenalty
		}
		if row.source.SupersededBy != "" {
			row.score -= scoring.SupersededPenalty
		}
	}
}

func sortCandidates(rows []*candidate) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		if rows[i].source.SourceType != rows[j].source.SourceType {
			return rows[i].source.SourceType < rows[j].source.SourceType
		}
		return rows[i].source.SourceID < rows[j].source.SourceID
	})
}

func sourceAge(source store.RecallSource, now time.Time, staleAfterDays int64) (bool, *float64) {
	var timestamp *time.Time
	switch {
	case source.SourceType == "fact":
		timestamp = source.VerifiedAt
		if timestamp == nil {
			return true, nil
		}
	case source.SourceType == "task" && source.Status == "done":
		timestamp = source.UpdatedAt
	default:
		return false, nil
	}
	days := now.Sub(timestamp.UTC()).Hours() / 24
	if days < 0 {
		days = 0
	}
	stale := source.SourceType == "fact" && days > float64(staleAfterDays)
	return stale, &days
}

func acceptedSource(source string) bool {
	return source == "user" || strings.HasPrefix(source, "user+agent:")
}

func staleEmbeddingWarning(status embedding.StatusReport, total int) Warning {
	parts := []string{}
	if status.Missing > 0 {
		parts = append(parts, fmt.Sprintf("%d missing", status.Missing))
	}
	if status.ContentHashMismatched > 0 {
		parts = append(parts, fmt.Sprintf("%d content-hash-mismatched", status.ContentHashMismatched))
	}
	if status.WrongByteLength > 0 {
		parts = append(parts, fmt.Sprintf("%d wrong-byte-length", status.WrongByteLength))
	}
	return Warning{
		Kind: "stale_embeddings", StaleCount: status.Missing + status.ContentHashMismatched + status.WrongByteLength,
		TotalCount: total,
		Reason:     strings.Join(parts, ", ") + "; current vectors remain vector-ordered and matching stale rows use lexical fallback",
		Fix:        embedding.RebuildRemedy,
	}
}

func rerankText(source store.RecallSource) string {
	if strings.TrimSpace(source.Summary) != "" {
		return source.Summary + "\n\n" + source.Title + "\n\n" + source.Body
	}
	return source.Title + "\n\n" + source.Body
}

func normalizeFTS(value, minValue, maxValue float64) float64 {
	if math.Abs(maxValue-minValue) < 1e-12 {
		return 1
	}
	return clamp01((value - minValue) / (maxValue - minValue))
}

func ageDecay(ageDays *float64, halfLifeDays int64) float64 {
	if halfLifeDays <= 0 || ageDays == nil || *ageDays <= 0 {
		return 1
	}
	return math.Exp(-*ageDays * math.Ln2 / float64(halfLifeDays))
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		normA += x * x
		normB += y * y
	}
	if normA <= 1e-12 || normB <= 1e-12 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func clamp01(value float64) float64 {
	return math.Max(0, math.Min(1, value))
}
