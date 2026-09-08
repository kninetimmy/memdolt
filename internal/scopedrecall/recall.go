// Package scopedrecall is the shared CLI/MCP application boundary for enabled
// global recall. The scoring engine remains independent of store ownership.
package scopedrecall

import (
	"context"
	"errors"

	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func Run(ctx context.Context, st retrieval.Store, repo string, options retrieval.Options) (result retrieval.Response, err error) {
	cfg, err := localdolt.ReadGlobalConfig(repo)
	if err != nil {
		return result, err
	}
	if !cfg.Enabled {
		return retrieval.Run(ctx, st, repo, options)
	}
	global, err := localdolt.OpenGlobal(ctx, repo)
	if err != nil {
		return result, err
	}
	// Hold the established exclusive lock through global vector reads/reranking.
	// A competing repository owner refuses visibly before opening an engine.
	defer func() { err = errors.Join(err, global.Close()) }()
	paths, err := localdolt.GlobalPaths()
	if err != nil {
		return result, err
	}
	return retrieval.RunCombined(ctx, st, repo, options, retrieval.GlobalScope{
		Store: global, EmbeddingsPath: paths.EmbeddingsFile(), IncludeDocsInDefault: cfg.IncludeDocsInDefault,
	})
}
