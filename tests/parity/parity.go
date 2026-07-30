//go:build parity

// Package parity is PRD §16's rig 2: the ONNX-in-Go parity harness. It
// tokenizes and runs BGE-small-en-v1.5 (embeddings) and
// ms-marco-MiniLM-L-6-v2 (rerank) through a pure-Go stack — a WordPiece
// tokenizer built from github.com/sugarme/tokenizer's primitives and
// github.com/yalue/onnxruntime_go for inference — and diffs the results
// against reference fixtures generated from the same SHA-256-pinned model
// files run through memhub's own stack (fastembed 5, tests/parity/fixturegen).
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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// modelFilePin is one entry of testdata/model_pins.json.
type modelFilePin struct {
	RemotePath string `json:"remote_path"`
	LocalName  string `json:"local_name"`
	SHA256     string `json:"sha256"`
}

type modelBundlePin struct {
	Name    string         `json:"name"`
	BaseURL string         `json:"base_url"`
	Files   []modelFilePin `json:"files"`
}

type modelPins struct {
	Bundles []modelBundlePin `json:"bundles"`
}

func loadModelPins(path string) (*modelPins, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var pins modelPins
	if err := json.Unmarshal(raw, &pins); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &pins, nil
}

// stagedModelFiles verifies every file in bundle against modelDir's
// SHA-256-pinned copy and returns the bundle directory's file bytes keyed by
// local_name. It fails loudly rather than skipping: a missing or
// hash-mismatched file is an error naming exactly what to fetch and from
// where.
func stagedModelFiles(modelDir string, bundle modelBundlePin) (map[string][]byte, error) {
	bundleDir := filepath.Join(modelDir, bundle.Name)
	out := make(map[string][]byte, len(bundle.Files))
	for _, f := range bundle.Files {
		path := filepath.Join(bundleDir, f.LocalName)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf(
				"reading %s: %w\nFetch it from %s/%s and verify sha256=%s before use "+
					"(or point MEMDOLT_PARITY_MODEL_DIR at memhub's staged build output, "+
					"e.g. <memhub checkout>/target/debug/build/memhub-*/out/)",
				path, err, bundle.BaseURL, f.RemotePath, f.SHA256)
		}
		sum := sha256.Sum256(data)
		actual := hex.EncodeToString(sum[:])
		if actual != f.SHA256 {
			return nil, fmt.Errorf(
				"%s: sha256 mismatch: expected %s, got %s\nRe-fetch from %s/%s and verify before use",
				path, f.SHA256, actual, bundle.BaseURL, f.RemotePath)
		}
		out[f.LocalName] = data
	}
	return out, nil
}

// requireEnv fails loudly with instructions when name is unset, rather than
// silently skipping the parity check.
func requireEnv(t interface{ Fatalf(string, ...any) }, name, instructions string) string {
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set.\n%s", name, instructions)
	}
	return v
}
