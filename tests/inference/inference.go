//go:build parity || golden

// Package inference is the ONNX-in-Go embedding/rerank/tokenizer stack that
// PRD §16's rig 2 (tests/parity) built and validated against memhub's
// fastembed-driven reference (docs/spikes/m0-rig2.md). It is factored out
// as its own package, rather than living inside tests/parity, so that rig 3
// (tests/golden, build tag `golden`) can reuse the exact same code instead
// of reimplementing it — Go's per-directory package boundary means two
// different test packages (tests/parity, tests/golden) can only share
// unexported logic by importing a common package, and that common package
// needs a build tag covering both of its callers' tags.
//
// Two environment variables are required by callers wiring this package
// up, and the harness fails loudly (never silently skips) if either is
// unset or if any model file's SHA-256 does not match
// tests/parity/testdata/model_pins.json:
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
//	                                     library for this platform. See
//	                                     docs/spikes/m0-rig2.md for where to get one
//	                                     per platform.
package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ModelFilePin is one file entry of tests/parity/testdata/model_pins.json.
type ModelFilePin struct {
	RemotePath string `json:"remote_path"`
	LocalName  string `json:"local_name"`
	SHA256     string `json:"sha256"`
}

// ModelBundlePin is one model bundle entry of model_pins.json.
type ModelBundlePin struct {
	Name    string         `json:"name"`
	BaseURL string         `json:"base_url"`
	Files   []ModelFilePin `json:"files"`
}

// ModelPins is the parsed shape of model_pins.json.
type ModelPins struct {
	Bundles []ModelBundlePin `json:"bundles"`
}

// LoadModelPins parses a model_pins.json file at path.
func LoadModelPins(path string) (*ModelPins, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var pins ModelPins
	if err := json.Unmarshal(raw, &pins); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &pins, nil
}

// StagedModelFiles verifies every file in bundle against modelDir's
// SHA-256-pinned copy and returns the bundle directory's file bytes keyed by
// local_name. It fails loudly rather than skipping: a missing or
// hash-mismatched file is an error naming exactly what to fetch and from
// where.
func StagedModelFiles(modelDir string, bundle ModelBundlePin) (map[string][]byte, error) {
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

// RequireEnv fails loudly with instructions when name is unset, rather than
// silently skipping the check.
func RequireEnv(t interface{ Fatalf(string, ...any) }, name, instructions string) string {
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set.\n%s", name, instructions)
	}
	return v
}
