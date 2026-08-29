package embedding

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	sgtokenizer "github.com/sugarme/tokenizer"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	embeddingModelName = "bge-small-en-v1.5"
	rerankerModelName  = "ms-marco-MiniLM-L-6-v2"
)

var runtimeState struct {
	sync.Mutex
	path  string
	users int
}

// Engine owns the two tokenizers and ONNX sessions used by production
// embedding and reranking. Close it before process exit.
type Engine struct {
	// ponytail: serialize tokenizer/session access until measured retrieval
	// throughput justifies batching or per-session workers.
	mu sync.Mutex

	embeddingTokenizer *sgtokenizer.Tokenizer
	rerankerTokenizer  *sgtokenizer.Tokenizer
	embedder           *embedRunner
	reranker           *rerankRunner
	closed             bool
}

// Open verifies or fetches every pinned model and the current platform's
// pinned runtime before ONNX Runtime initialization, then opens both sessions.
func Open(ctx context.Context, opts Options) (*Engine, error) {
	provisioner, err := newProvisioner(opts)
	if err != nil {
		return nil, err
	}
	artifacts, err := provisioner.provision(ctx)
	if err != nil {
		return nil, err
	}

	embedModel, err := requiredModelFile(artifacts, embeddingModelName, "model.onnx")
	if err != nil {
		return nil, err
	}
	embedTokenizerJSON, err := requiredModelFile(artifacts, embeddingModelName, "tokenizer.json")
	if err != nil {
		return nil, err
	}
	rerankModel, err := requiredModelFile(artifacts, rerankerModelName, "model.onnx")
	if err != nil {
		return nil, err
	}
	rerankTokenizerJSON, err := requiredModelFile(artifacts, rerankerModelName, "tokenizer.json")
	if err != nil {
		return nil, err
	}

	embedTokenizer, err := buildBertWordPieceTokenizer(embedTokenizerJSON)
	if err != nil {
		return nil, fmt.Errorf("building embedding tokenizer: %w", err)
	}
	rerankTokenizer, err := buildBertWordPieceTokenizer(rerankTokenizerJSON)
	if err != nil {
		return nil, fmt.Errorf("building reranker tokenizer: %w", err)
	}
	if err := acquireRuntime(artifacts.runtimePath); err != nil {
		return nil, err
	}
	embedder, err := newEmbedRunner(embedModel)
	if err != nil {
		return nil, errors.Join(err, releaseRuntime())
	}
	reranker, err := newRerankRunner(rerankModel)
	if err != nil {
		return nil, errors.Join(err, embedder.destroy(), releaseRuntime())
	}
	return &Engine{
		embeddingTokenizer: embedTokenizer,
		rerankerTokenizer:  rerankTokenizer,
		embedder:           embedder,
		reranker:           reranker,
	}, nil
}

func requiredModelFile(artifacts *provisionedArtifacts, bundle, name string) ([]byte, error) {
	files, ok := artifacts.models[bundle]
	if !ok {
		return nil, fmt.Errorf("committed artifact manifest has no model bundle %q", bundle)
	}
	data, ok := files[name]
	if !ok {
		return nil, fmt.Errorf("committed artifact manifest bundle %q has no %s", bundle, name)
	}
	return data, nil
}

func acquireRuntime(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving verified ONNX Runtime path %s: %w", path, err)
	}
	runtimeState.Lock()
	defer runtimeState.Unlock()
	if runtimeState.path != "" {
		if runtimeState.path != absPath {
			return fmt.Errorf("ONNX Runtime is already active from %s, refusing different path %s", runtimeState.path, absPath)
		}
		runtimeState.users++
		return nil
	}
	if ort.IsInitialized() {
		return fmt.Errorf("ONNX Runtime was initialized outside internal/embedding; its artifact verification cannot be established")
	}
	ort.SetSharedLibraryPath(absPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("initializing verified ONNX Runtime %s: %w", absPath, err)
	}
	runtimeState.path = absPath
	runtimeState.users = 1
	return nil
}

func releaseRuntime() error {
	runtimeState.Lock()
	defer runtimeState.Unlock()
	if runtimeState.users <= 0 {
		return fmt.Errorf("ONNX Runtime user count underflow")
	}
	runtimeState.users--
	if runtimeState.users != 0 {
		return nil
	}
	err := ort.DestroyEnvironment()
	runtimeState.path = ""
	return err
}

// EmbeddingTokenIDs returns the production BGE tokenizer's token IDs. It is
// exposed so the parity rig can bind production tokenization to its fixture.
func (e *Engine) EmbeddingTokenIDs(text string) ([]int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("embedding engine is closed")
	}
	enc, err := encodeSingle(e.embeddingTokenizer, text)
	if err != nil {
		return nil, err
	}
	return enc.IDs, nil
}

// RerankerTokenIDs returns the production reranker tokenizer's token IDs.
func (e *Engine) RerankerTokenIDs(text string) ([]int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("embedding engine is closed")
	}
	enc, err := encodeSingle(e.rerankerTokenizer, text)
	if err != nil {
		return nil, err
	}
	return enc.IDs, nil
}

// Embed returns a 384-dimensional, CLS-pooled, L2-normalized BGE embedding.
func (e *Engine) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("embedding engine is closed")
	}
	enc, err := encodeSingle(e.embeddingTokenizer, text)
	if err != nil {
		return nil, err
	}
	return e.embedder.embed(enc)
}

// Rerank returns the raw ms-marco cross-encoder relevance logit.
func (e *Engine) Rerank(query, passage string) (float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, fmt.Errorf("embedding engine is closed")
	}
	enc, err := encodePair(e.rerankerTokenizer, query, passage)
	if err != nil {
		return 0, err
	}
	return e.reranker.score(enc)
}

// Close destroys both sessions and releases the process-global ONNX Runtime
// environment after the final Engine closes.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return errors.Join(e.reranker.destroy(), e.embedder.destroy(), releaseRuntime())
}
