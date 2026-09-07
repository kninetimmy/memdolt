package storeipc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// Temporary roots can themselves be OS aliases (macOS /var). Fixtures use
// their canonical directory; production bundle paths still refuse links.
func interopTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInteropOwnerLostRepliesNeverReplay(t *testing.T) {
	for _, operation := range []string{"import", "export"} {
		t.Run(operation, func(t *testing.T) {
			base, st, endpoint := startOwner(t)
			ctx := context.Background()
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := endpoint.Close(); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == storeipc.OperationPath {
						calls.Add(1)
						inner.ServeHTTP(httptest.NewRecorder(), r)
						panic(http.ErrAbortHandler)
					}
					inner.ServeHTTP(w, r)
				})
			})
			file, err := filepath.Abs(filepath.Join("..", "store", "localdolt", "testdata", "memhub-v1.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Tests run from a worktree under protected .orchestrator metadata;
			// copy the synthetic bundle to the explicitly selected external file.
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			file = filepath.Join(interopTempDir(t), "memory.json")
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var result localdolt.InteropResult
			if operation == "import" {
				result, err = routed.ImportMemory(ctx, localdolt.ImportMemoryOptions{File: file, FromMemhub: true})
			} else {
				file = filepath.Join(interopTempDir(t), "export.json")
				result, err = routed.ExportMemory(ctx, localdolt.ExportMemoryOptions{File: file})
			}
			if err == nil || result.Status != "unknown" || !strings.Contains(err.Error(), "outcome unknown") || calls.Load() != 1 {
				t.Fatalf("lost reply=%+v, %v, calls=%d", result, err, calls.Load())
			}
			if operation == "import" {
				pending, err := st.PendingProposals(ctx)
				if err != nil || len(pending) != 2 {
					t.Fatalf("owner replayed/lost imported proposals: %+v, %v", pending, err)
				}
			} else if raw, err := os.ReadFile(file); err != nil || !strings.Contains(string(raw), "memdolt_export_version") {
				t.Fatalf("owner failed to publish: %v", err)
			}
		})
	}
}

type interopFailureBackend struct{ storeipc.Backend }

func (s interopFailureBackend) ImportMemory(ctx context.Context, opts localdolt.ImportMemoryOptions) (localdolt.InteropResult, error) {
	result, err := s.Backend.ImportMemory(ctx, opts)
	return result, errors.Join(err, errors.New("synthetic post-import failure"))
}

func TestInteropOwnerPreservesConfirmedProgressWithError(t *testing.T) {
	base, st, endpoint := startOwner(t)
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, interopFailureBackend{st}, nil)
	raw, err := os.ReadFile(filepath.Join("..", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(interopTempDir(t), "memory.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := routed.ImportMemory(ctx, localdolt.ImportMemoryOptions{File: file, FromMemhub: true})
	if err == nil || !strings.Contains(err.Error(), "post-import") || result.MainCommit == "" || result.Status != "imported" || len(result.IdentityMap) == 0 || len(result.CreatedProposals) != 2 || len(result.RemainingProposals) != 0 {
		t.Fatalf("owner erased confirmed progress: %+v, %v", result, err)
	}
}

func TestInteropTypedOwnerRejectsLossyPathBeforeSubmission(t *testing.T) {
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == storeipc.OperationPath {
				calls.Add(1)
			}
			inner.ServeHTTP(w, r)
		})
	})
	bad := filepath.Join(interopTempDir(t), "invalid-"+string([]byte{0xff})+".json")
	if _, err := routed.ImportMemory(context.Background(), localdolt.ImportMemoryOptions{File: bad}); err == nil {
		t.Fatal("serialized a lossy import path")
	}
	if _, err := routed.ExportMemory(context.Background(), localdolt.ExportMemoryOptions{File: bad}); err == nil {
		t.Fatal("serialized a lossy export path")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid typed paths reached the owner")
	}
}

func TestInteropRawOwnerUnicodeCannotSelectAnotherBundle(t *testing.T) {
	base, st, endpoint := startOwner(t)
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := interopTempDir(t)
	for _, name := range []string{"�.json", "😀�.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	replacements := []string{"\xff", `\ud800`, `\udc00`, `\ud83d\ude00\ufffd`}
	var selected, calls atomic.Int32
	routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == storeipc.OperationPath {
				calls.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(data, []byte("INTEROP_UNICODE_MARKER"), []byte(replacements[selected.Load()]), 1)))
			}
			inner.ServeHTTP(w, r)
		})
	})
	before := queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))")
	opts := localdolt.ImportMemoryOptions{File: filepath.Join(dir, "INTEROP_UNICODE_MARKER.json"), FromMemhub: true}
	for i := range len(replacements) - 1 {
		selected.Store(int32(i))
		result, err := routed.ImportMemory(ctx, opts)
		if err == nil || result.MainCommit != "" || calls.Load() != int32(i+1) || queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))") != before {
			t.Fatalf("raw Unicode selected a different source bundle: %+v, %v", result, err)
		}
	}
	selected.Store(int32(len(replacements) - 1))
	result, err := routed.ImportMemory(ctx, opts)
	if err != nil || result.MainCommit == "" || result.File != filepath.Join(dir, "😀�.json") || calls.Load() != int32(len(replacements)) {
		t.Fatalf("valid owner Unicode path changed: %+v, %v", result, err)
	}
}
