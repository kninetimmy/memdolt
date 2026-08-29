package embedding

import (
	"fmt"
	"math"

	ort "github.com/yalue/onnxruntime_go"
)

// EmbeddingDim is BGE-small-en-v1.5's output width (PRD §8.3).
const EmbeddingDim = 384

// l2NormalizeEpsilon matches fastembed 5.13.4's common::normalize exactly.
const l2NormalizeEpsilon = 1e-12

type embedRunner struct {
	session *ort.DynamicAdvancedSession
}

func newEmbedRunner(onnxData []byte) (*embedRunner, error) {
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(onnxData,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, nil)
	if err != nil {
		return nil, fmt.Errorf("loading embedding model: %w", err)
	}
	return &embedRunner{session: session}, nil
}

func (r *embedRunner) destroy() error {
	return r.session.Destroy()
}

func (r *embedRunner) embed(enc encodedInput) ([]float32, error) {
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

	outputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, seqLen, EmbeddingDim))
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

	data := outputTensor.GetData()
	if len(data) < EmbeddingDim {
		return nil, fmt.Errorf("output tensor too small: got %d floats, want at least %d", len(data), EmbeddingDim)
	}
	cls := make([]float32, EmbeddingDim)
	copy(cls, data[:EmbeddingDim])
	return l2Normalize(cls), nil
}

type rerankRunner struct {
	session *ort.DynamicAdvancedSession
}

func newRerankRunner(onnxData []byte) (*rerankRunner, error) {
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(onnxData,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"logits"}, nil)
	if err != nil {
		return nil, fmt.Errorf("loading reranker model: %w", err)
	}
	return &rerankRunner{session: session}, nil
}

func (r *rerankRunner) destroy() error {
	return r.session.Destroy()
}

func (r *rerankRunner) score(enc encodedInput) (float32, error) {
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

	outputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
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
	if len(data) == 0 {
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
