package storeipc_test

import (
	"context"
	"errors"
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
			file = filepath.Join(t.TempDir(), "memory.json")
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			var result localdolt.InteropResult
			if operation == "import" {
				result, err = routed.ImportMemory(ctx, localdolt.ImportMemoryOptions{File: file, FromMemhub: true})
			} else {
				file = filepath.Join(t.TempDir(), "export.json")
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
	file := filepath.Join(t.TempDir(), "memory.json")
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
	bad := filepath.Join(t.TempDir(), "invalid-"+string([]byte{0xff})+".json")
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
