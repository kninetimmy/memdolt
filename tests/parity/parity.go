//go:build parity

// Package parity is PRD §16's rig 2: the ONNX-in-Go parity harness. It runs
// internal/embedding's production tokenizer, embedding, and reranker against
// fixtures generated through memhub's fastembed 5 reference path.
//
// It is behind the `parity` build tag because it may fetch roughly 150MB of
// pinned artifacts and runs the full models; neither belongs in go test ./....
//
//	go test -tags parity,gms_pure_go ./tests/parity/... -timeout 30m
//
// The second tag is not decoration: see docs/spikes/m0-rig1.md §14 — a
// command-line -tags replaces GOFLAGS' tag list rather than adding to it.
//
// With no overrides, production provisioning uses ~/.memdolt/models and
// fetches missing artifacts from models/manifest.json. These optional
// overrides exercise the same verification path:
//
//	MEMDOLT_PARITY_MODEL_DIR         alternate model/cache root
//	ONNXRUNTIME_SHARED_LIBRARY_PATH alternate pre-positioned runtime library
//	MEMDOLT_INFERENCE_OFFLINE        nonempty: prohibit fetches
package parity
