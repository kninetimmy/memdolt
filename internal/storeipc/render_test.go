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

	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestRenderOwnerLostReplyNeverReplaysReplacement(t *testing.T) {
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
				inner.ServeHTTP(httptest.NewRecorder(), r)
				panic(http.ErrAbortHandler)
			}
			inner.ServeHTTP(w, r)
		})
	})
	result, err := routed.Render(context.Background())
	if err == nil || result.Status != "unknown" || !strings.Contains(err.Error(), "inspect configured outputs") || !strings.Contains(err.Error(), "before retrying") || calls.Load() != 1 {
		t.Fatalf("lost render reply=%+v, %v, calls=%d", result, err, calls.Load())
	}
	if !strings.Contains(err.Error(), "memdolt note list") || !strings.Contains(err.Error(), "Dolt history") || len(result.NoteCommits) != 0 {
		t.Fatal("lost reply omitted possible note effects or claimed unobserved hashes")
	}
	for _, name := range []string{"PROJECT.md", "PROJECT_LEDGER.md"} {
		raw, err := os.ReadFile(filepath.Join(base, ".memdolt", "rendered", name))
		if err != nil || !strings.HasPrefix(string(raw), render.Marker) {
			t.Fatalf("owner did not complete %s: %v", name, err)
		}
	}
	backups, err := os.ReadDir(filepath.Join(base, ".memdolt", "backups", "rendered"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("lost reply replayed a replacement: %v, %v", backups, err)
	}
}

type renderFailureBackend struct{ storeipc.Backend }

func (s renderFailureBackend) Render(ctx context.Context) (render.Result, error) {
	result, err := s.Backend.Render(ctx)
	return result, errors.Join(err, errors.New("synthetic post-replacement error"))
}

func TestRenderOwnerRetainsConfirmedOutputsAndBackupsOnError(t *testing.T) {
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Render(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, renderFailureBackend{st}, nil)
	result, err := routed.Render(context.Background())
	if err == nil || !strings.Contains(err.Error(), "post-replacement") || result.Status != "written" || len(result.WrittenFiles) != 2 || len(result.BackupFiles) != 2 || result.SourceCommit == "" {
		t.Fatalf("owner lost confirmed render result=%+v, %v", result, err)
	}
	for _, path := range append(result.WrittenFiles, result.BackupFiles...) {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}
