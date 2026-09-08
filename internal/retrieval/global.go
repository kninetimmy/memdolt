package retrieval

import (
	"context"
	"errors"
	"strings"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/store"
)

type recallCapture interface {
	CaptureRecall(context.Context, store.RecallSnapshotOptions) (store.RecallSnapshot, error)
}

// GlobalScope is an already owned global replica. The application chooses its
// enabled repository policy and owns its lifetime, including during inference.
type GlobalScope struct {
	Store                Store
	EmbeddingsPath       string
	IncludeDocsInDefault bool
}

func RunCombined(ctx context.Context, st Store, baseDir string, options Options, global GlobalScope) (Response, error) {
	return run(ctx, st, baseDir, options, &global)
}

// Internal scope prefixes are removed from output. They let the existing
// candidate map, vector fallback and deterministic sort distinguish equal IDs
// without changing persisted IDs, vectors, or the single reranking pipeline.
type recallScope struct {
	snapshot store.RecallSnapshot
	path     string
	name     string
	docs     bool
}

type combinedStore struct {
	scopes   []recallScope
	warnings []Warning
}

func recallCombined(ctx context.Context, st Store, path string, inference Inference, cfg Config, options Options, global GlobalScope) (result Response, err error) {
	local, ok := st.(recallCapture)
	if !ok {
		return result, errors.New("combined recall requires a store with committed snapshot capture")
	}
	gs, ok := global.Store.(recallCapture)
	if !ok {
		return result, errors.New("global recall requires committed snapshot capture")
	}
	localDocs := cfg.IncludeDocsInDefault
	cfg.IncludeDocsInDefault = localDocs || global.IncludeDocsInDefault
	resolved, err := resolve(cfg, options)
	if err != nil {
		return result, err
	}
	captureOptions := store.RecallSnapshotOptions{Query: resolved.query, SourceTypes: resolved.sourceTypes, Provenance: options.Provenance}
	localSnapshot, err := local.CaptureRecall(ctx, captureOptions)
	if err != nil {
		return result, err
	}
	globalSnapshot, err := gs.CaptureRecall(ctx, captureOptions)
	if err != nil {
		return result, err
	}
	combined := &combinedStore{scopes: []recallScope{
		{snapshot: localSnapshot, path: path, name: "repo", docs: localDocs || resolved.explicitlyDocumentScoped},
		{snapshot: globalSnapshot, path: global.EmbeddingsPath, name: "global", docs: global.IncludeDocsInDefault || resolved.explicitlyDocumentScoped},
	}}
	result, err = recall(ctx, combined, path, inference, cfg, options, combined)
	for i := range result.Results {
		hit := &result.Results[i]
		index, id := splitScopeID(hit.SourceID)
		hit.SourceID = id
		hit.Scope = combined.scopes[index].name
		hit.SnapshotCommit = combined.scopes[index].snapshot.Commit
	}
	return result, err
}

func scopeID(index int, id string) string {
	if index == 0 {
		return "0/" + id
	}
	return "1/" + id
}

func splitScopeID(id string) (int, string) {
	if raw, ok := strings.CutPrefix(id, "1/"); ok {
		return 1, raw
	}
	return 0, strings.TrimPrefix(id, "0/")
}

func globalSource(kind string) bool {
	return kind == "fact" || kind == "decision" || kind == "doc_chunk"
}

func (s *combinedStore) include(source store.RecallSource) bool {
	index, _ := splitScopeID(source.SourceID)
	return (index == 0 || globalSource(source.SourceType)) && (source.SourceType != "doc_chunk" || s.scopes[index].docs)
}

func (s *combinedStore) RecallSources(context.Context) ([]store.RecallSource, error) {
	var sources []store.RecallSource
	for i, scope := range s.scopes {
		for _, source := range scope.snapshot.Sources {
			if i != 0 && !globalSource(source.SourceType) {
				continue
			}
			source.SourceID = scopeID(i, source.SourceID)
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (s *combinedStore) EmbeddingSources(context.Context) ([]store.EmbeddingSource, error) {
	var sources []store.EmbeddingSource
	for i, scope := range s.scopes {
		for _, source := range scope.snapshot.Embeddings {
			if i != 0 && !globalSource(source.SourceType) {
				continue
			}
			source.SourceID = scopeID(i, source.SourceID)
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (s *combinedStore) RecallFTS(context.Context, string, []string) ([]store.LexicalHit, error) {
	var hits []store.LexicalHit
	for i, scope := range s.scopes {
		for _, hit := range scope.snapshot.Lexical {
			hit.SourceID = scopeID(i, hit.SourceID)
			hits = append(hits, hit)
		}
	}
	return hits, nil
}

func (s *combinedStore) LastChanged(_ context.Context, kind, id string) (*store.CommitProvenance, error) {
	index, raw := splitScopeID(id)
	return s.scopes[index].snapshot.Provenance[kind+"/"+raw], nil
}

func (s *combinedStore) currentVectors(ctx context.Context, _ string, sources []store.EmbeddingSource) ([]embedding.Vector, embedding.StatusReport, error) {
	var vectors []embedding.Vector
	var status embedding.StatusReport
	for index, scope := range s.scopes {
		var selected []store.EmbeddingSource
		for _, source := range sources {
			i, raw := splitScopeID(source.SourceID)
			if i == index {
				source.SourceID = raw
				selected = append(selected, source)
			}
		}
		current, report, err := embedding.CurrentVectors(ctx, scope.path, selected)
		if err != nil {
			return nil, status, err
		}
		for _, vector := range current {
			vector.SourceID = scopeID(index, vector.SourceID)
			vectors = append(vectors, vector)
		}
		for _, entry := range report.Entries {
			entry.SourceID = scopeID(index, entry.SourceID)
			status.Entries = append(status.Entries, entry)
		}
		stale := report.Missing + report.ContentHashMismatched + report.WrongByteLength
		if stale > 0 {
			warning := staleEmbeddingWarning(report, len(selected))
			warning.Scope = scope.name
			if index != 0 {
				warning.Fix = "run `memdolt index rebuild --global --dir <repository>`"
			}
			s.warnings = append(s.warnings, warning)
		}
		status.Missing += report.Missing
		status.ContentHashMismatched += report.ContentHashMismatched
		status.WrongByteLength += report.WrongByteLength
	}
	return vectors, status, nil
}
