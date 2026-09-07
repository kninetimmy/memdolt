//go:build golden

package golden

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kninetimmy/memdolt/internal/codeindex"
	"github.com/kninetimmy/memdolt/internal/embedding"
)

// The Rust corpus is the complete corresponding benchmark snapshot; the
// polyglot fixture and both goldens are unchanged v0.2.0 bytes. NOTICE.md
// records the later tagged corpus's obsolete cosine target and 17/18 result.
func TestLocateGolden(t *testing.T) {
	ctx := context.Background()
	engine := locateEngine(t)
	for _, fixture := range []struct {
		name, golden   string
		files, matches int
	}{
		{"rust-benchmark", "code_locate_golden.json", 100, 18},
		{"polyglot", "code_locate_golden_polyglot.json", 6, 17},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := seedLocateCorpus(t, fixture.name, fixture.files)
			_ = readLocateFixture(t, fixture.golden)
			golden, err := codeindex.LoadGolden(fixture.golden)
			if err != nil {
				t.Fatal(err)
			}
			fusion, evalErr := codeindex.Evaluate(ctx, root, engine, golden, codeindex.EvalOptions{GoldenPath: fixture.golden, K: 3})
			logLocateSummary(t, "fusion", fusion)
			if evalErr != nil {
				t.Error(evalErr)
			}
			if fusion.MatchQueries != fixture.matches || fusion.MatchPassesAtK != fixture.matches || fusion.RecallAtK != 1 {
				t.Errorf("fusion must retain full Recall@3: got %d/%d; want %d/%d", fusion.MatchPassesAtK, fusion.MatchQueries, fixture.matches, fixture.matches)
			}
			if fusion.EmptyQueries != 2 || fusion.SafetyFailures != 2 {
				t.Errorf("no-floor fusion must honestly report both nonsense leaks: %+v", fusion)
			}
			floor := float32(0)
			rerank, err := codeindex.Evaluate(ctx, root, engine, golden, codeindex.EvalOptions{GoldenPath: fixture.golden, K: 3, UseReranker: true, MinRerankScore: &floor})
			logLocateSummary(t, "rerank-floor-0", rerank)
			if err != nil && !errors.Is(err, codeindex.ErrBelowBaseline) {
				t.Fatal(err)
			}
			if !rerank.Reranked || rerank.EmptyQueries != 2 || rerank.EmptyPasses != 2 || rerank.SafetyFailures != 0 {
				t.Errorf("rerank floor 0 must reject both nonsense probes: %+v", rerank)
			}
			// Rerank can hurt true-match recall. Its actual losses above remain
			// visible; fusion's full match bar remains the governing assertion.
			for _, name := range []string{"dolt", "embeddings.sqlite"} {
				if _, err := os.Stat(filepath.Join(root, ".memdolt", name)); !os.IsNotExist(err) {
					t.Fatalf("locator touched %s: %v", name, err)
				}
			}
		})
	}
}

// This diagnostic deliberately does not replace the full18/18 benchmark gate.
// It reproduces the stale matcher against every v0.2.0 source file.
func TestLocateTaggedCorpusDiagnostic(t *testing.T) {
	root := seedLocateCorpus(t, "rust", 117)
	_ = readLocateFixture(t, "code_locate_golden.json")
	golden, err := codeindex.LoadGolden("code_locate_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := codeindex.Evaluate(context.Background(), root, locateEngine(t), golden, codeindex.EvalOptions{GoldenPath: "code_locate_golden.json", K: 3})
	if err != nil && !errors.Is(err, codeindex.ErrBelowBaseline) {
		t.Fatal(err)
	}
	logLocateSummary(t, "tagged-corpus-diagnostic", summary)
}

func locateEngine(t *testing.T) *embedding.Engine {
	t.Helper()
	ctx := context.Background()
	engine, err := embedding.Open(ctx, embedding.Options{
		CacheDir:    os.Getenv("MEMDOLT_PARITY_MODEL_DIR"),
		RuntimePath: os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH"),
		Offline:     os.Getenv("MEMDOLT_INFERENCE_OFFLINE") != "",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	return engine
}

func readLocateFixture(t *testing.T, file string) []byte {
	t.Helper()
	hashes := map[string]string{
		"code_locate_golden.json":          "843f48f743e967c17332d476751341fec53e566dd7a61f6c9921bc3e78ebd866",
		"code_locate_golden_polyglot.json": "6aac2e5dafe63bd75ffa233257796200a4ed8fb8734919b95d11a57deb6b2a40",
		"rust-benchmark.json":              "b36e9a82f25aaacd8813e66bed002cc4800ef22f5802d58c15d608df66d074d6",
		"rust.json":                        "d8ac31a506b529f30bd8b3a0236b2c9a53f8bc1f2e1a3a7cfcde04e52263f54d",
		"polyglot.json":                    "e62829f9eed206f4fc572704bdcc819162fdd4026ebe16508b856facecd91fcf",
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// Only outer JSON line endings may follow a host's checkout convention;
	// source strings retain their exact original bytes and per-file hashes.
	if fmt.Sprintf("%x", sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))) != hashes[filepath.Base(file)] {
		t.Fatalf("frozen locator fixture changed: %s", file)
	}
	return raw
}

func seedLocateCorpus(t *testing.T, name string, expected int) string {
	t.Helper()
	var files []struct{ Path, SHA256, Content string }
	raw := readLocateFixture(t, filepath.Join("testdata", "locate", name+".json"))
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != expected {
		t.Fatalf("%s corpus files=%d, want %d", name, len(files), expected)
	}
	root := t.TempDir()
	for _, file := range files {
		if !fs.ValidPath(file.Path) || !filepath.IsLocal(filepath.FromSlash(file.Path)) {
			t.Fatalf("unsafe frozen path: %q", file.Path)
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(file.Content))) != file.SHA256 {
			t.Fatalf("changed frozen bytes: %s", file.Path)
		}
		filePath := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filePath, []byte(file.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"-c", "core.autocrlf=false", "add", "--", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v: %s", err, out)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".memdolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".memdolt", "config.toml"), []byte("[retrieval]\nmode = 'hybrid'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func logLocateSummary(t *testing.T, mode string, s codeindex.EvalSummary) {
	t.Helper()
	t.Logf("%s: match@1=%d/%d match@3=%d/%d empty=%d/%d safety_failures=%d elapsed_ms=%d", mode,
		s.MatchPassesAt1, s.MatchQueries, s.MatchPassesAtK, s.MatchQueries, s.EmptyPasses, s.EmptyQueries, s.SafetyFailures, s.ElapsedMS)
	for _, out := range s.Outcomes {
		if !out.Passed {
			t.Logf("%s %s: %s", mode, out.ID, *out.FailureReason)
		}
	}
}
