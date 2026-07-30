//go:build parity

package parity

import (
	"fmt"
	"math"

	ort "github.com/yalue/onnxruntime_go"
)

// embeddingDim is BGE-small-en-v1.5's output width (PRD §8.3).
const embeddingDim = 384

// l2NormalizeEpsilon matches fastembed 5.13.4's common::normalize exactly
// (fastembed-5.13.4/src/common.rs), so the Go and Rust reference paths apply
// the identical epsilon rather than merely "close enough" ones.
const l2NormalizeEpsilon = 1e-12

// embedRunner wraps an onnxruntime session for BGE-small-en-v1.5: CLS
// pooling over `last_hidden_state`, then L2-normalized — matching
// fastembed's pooling::cls + common::normalize (fastembed-5.13.4/src/pooling.rs,
// src/common.rs), which memhub's embeddings.rs relies on unmodified.
type embedRunner struct {
	session *ort.DynamicAdvancedSession
}

// newEmbedRunner takes the model's already SHA-256-verified bytes (rather
// than re-opening it by path) so the bytes onnxruntime loads are exactly
// the ones stagedModelFiles hashed — no re-read in between to trust.
func newEmbedRunner(onnxData []byte) (*embedRunner, error) {
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(onnxData,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, nil)
	if err != nil {
		return nil, fmt.Errorf("loading embedding model: %w", err)
	}
	return &embedRunner{session: session}, nil
}

func (r *embedRunner) Destroy() error {
	return r.session.Destroy()
}

// Embed runs one sequence through the model and returns its 384-dim,
// L2-normalized, CLS-pooled embedding.
func (r *embedRunner) Embed(enc encodedInput) ([]float32, error) {
	seqLen := int64(len(enc.IDs))
	inputShape := ort.NewShape(1, seqLen)

	idsTensor, err := ort.NewTensor(inputShape, toInt64(enc.IDs))
	if err != nil {
		return nil, fmt.Errorf("input_ids tensor: %w", err)
	}
	defer func() { _ = idsTensor.Destroy() }()
	maskTensor, err := ort.NewTensor(inputShape, toInt64(enc.AttentionIDs))
	if err != nil {
		return nil, fmt.Errorf("attention_mask tensor: %w", err)
	}
	defer func() { _ = maskTensor.Destroy() }()
	typeTensor, err := ort.NewTensor(inputShape, toInt64(enc.TypeIDs))
	if err != nil {
		return nil, fmt.Errorf("token_type_ids tensor: %w", err)
	}
	defer func() { _ = typeTensor.Destroy() }()

	outputShape := ort.NewShape(1, seqLen, embeddingDim)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("output tensor: %w", err)
	}
	defer func() { _ = outputTensor.Destroy() }()

	if err := r.session.Run(
		[]ort.Value{idsTensor, maskTensor, typeTensor},
		[]ort.Value{outputTensor},
	); err != nil {
		return nil, fmt.Errorf("running embedding session: %w", err)
	}

	// CLS pooling: the [CLS] token is always position 0 (fastembed's
	// pooling::cls slices `[.., 0, ..]`), so the first embeddingDim floats
	// of the row-major [1, seqLen, 384] output are the pooled vector.
	data := outputTensor.GetData()
	if len(data) < embeddingDim {
		return nil, fmt.Errorf("output tensor too small: got %d floats, want at least %d", len(data), embeddingDim)
	}
	cls := make([]float32, embeddingDim)
	copy(cls, data[:embeddingDim])
	return l2Normalize(cls), nil
}

// rerankRunner wraps an onnxruntime session for ms-marco-MiniLM-L-6-v2: the
// raw `logits` column-0 value, matching fastembed's rerank.rs — no sigmoid,
// no normalization (fastembed-5.13.4/src/reranking/impl.rs's
// `outputs.slice(s![.., 0])`).
type rerankRunner struct {
	session *ort.DynamicAdvancedSession
}

// newRerankRunner takes the model's already SHA-256-verified bytes (rather
// than re-opening it by path); see newEmbedRunner.
func newRerankRunner(onnxData []byte) (*rerankRunner, error) {
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(onnxData,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"logits"}, nil)
	if err != nil {
		return nil, fmt.Errorf("loading reranker model: %w", err)
	}
	return &rerankRunner{session: session}, nil
}

func (r *rerankRunner) Destroy() error {
	return r.session.Destroy()
}

// Score runs one (query, passage) pair through the cross-encoder and
// returns its relevance logit.
func (r *rerankRunner) Score(enc encodedInput) (float32, error) {
	seqLen := int64(len(enc.IDs))
	inputShape := ort.NewShape(1, seqLen)

	idsTensor, err := ort.NewTensor(inputShape, toInt64(enc.IDs))
	if err != nil {
		return 0, fmt.Errorf("input_ids tensor: %w", err)
	}
	defer func() { _ = idsTensor.Destroy() }()
	maskTensor, err := ort.NewTensor(inputShape, toInt64(enc.AttentionIDs))
	if err != nil {
		return 0, fmt.Errorf("attention_mask tensor: %w", err)
	}
	defer func() { _ = maskTensor.Destroy() }()
	typeTensor, err := ort.NewTensor(inputShape, toInt64(enc.TypeIDs))
	if err != nil {
		return 0, fmt.Errorf("token_type_ids tensor: %w", err)
	}
	defer func() { _ = typeTensor.Destroy() }()

	outputShape := ort.NewShape(1, 1)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return 0, fmt.Errorf("output tensor: %w", err)
	}
	defer func() { _ = outputTensor.Destroy() }()

	if err := r.session.Run(
		[]ort.Value{idsTensor, maskTensor, typeTensor},
		[]ort.Value{outputTensor},
	); err != nil {
		return 0, fmt.Errorf("running rerank session: %w", err)
	}

	data := outputTensor.GetData()
	if len(data) < 1 {
		return 0, fmt.Errorf("empty logits output")
	}
	return data[0], nil
}

func toInt64(v []int) []int64 {
	out := make([]int64, len(v))
	for i, x := range v {
		out[i] = int64(x)
	}
	return out
}

func l2Normalize(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / (norm + l2NormalizeEpsilon))
	}
	return out
}
