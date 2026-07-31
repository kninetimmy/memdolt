//go:build parity

// Package parity is PRD §16's rig 2: the ONNX-in-Go parity harness. It
// tokenizes and runs BGE-small-en-v1.5 (embeddings) and
// ms-marco-MiniLM-L-6-v2 (rerank) through a pure-Go stack — a WordPiece
// tokenizer built from github.com/sugarme/tokenizer's primitives and
// github.com/yalue/onnxruntime_go for inference, both wired up in
// tests/inference (shared with rig 3, tests/golden, behind the `parity ||
// golden` build tag) — and diffs the results against reference fixtures
// generated from the same SHA-256-pinned model files run through memhub's
// own stack (fastembed 5, tests/parity/fixturegen).
//
// It is behind the `parity` build tag because it needs the ~150MB of model
// files staged on disk and an onnxruntime shared library, neither of which
// belong in the default `go test ./...` run on three platforms and CI.
//
//	go test -tags parity,gms_pure_go ./tests/parity/... -timeout 30m
//
// The second tag is not decoration: see docs/spikes/m0-rig1.md §14 — a
// command-line -tags replaces GOFLAGS' tag list rather than adding to it.
//
// Two environment variables are required, and the harness fails loudly
// (never silently skips) if either is unset or if any model file's SHA-256
// does not match tests/parity/testdata/model_pins.json:
//
//	MEMDOLT_PARITY_MODEL_DIR            directory containing bge-small-en-v1.5/
//	                                     and ms-marco-MiniLM-L-6-v2/ subdirectories,
//	                                     each with model.onnx, tokenizer.json,
//	                                     config.json, special_tokens_map.json and
//	                                     tokenizer_config.json (memhub build.rs's
//	                                     OUT_DIR staging layout). If these files are
//	                                     not staged locally, fetch them from the
//	                                     pinned Hugging Face URLs in
//	                                     tests/parity/testdata/model_pins.json and
//	                                     verify each SHA-256 before use.
//	ONNXRUNTIME_SHARED_LIBRARY_PATH     path to an onnxruntime 1.26.0 shared
//	                                     library for this platform (onnxruntime.dll
//	                                     / libonnxruntime.so.1.26.0 /
//	                                     libonnxruntime.1.26.0.dylib). See
//	                                     docs/spikes/m0-rig2.md for where to get one
//	                                     per platform, including Windows AMD64's
//	                                     test-only copy bundled inside the
//	                                     yalue/onnxruntime_go module cache at
//	                                     <module cache>/github.com/yalue/onnxruntime_go@<version>/test_data/onnxruntime.dll.
package parity
