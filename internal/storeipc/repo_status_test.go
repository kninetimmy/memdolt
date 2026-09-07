package storeipc_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestRepoStatusOwnerParityAndLostResponse(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "parity", true: "lost"}[lost], func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("DOLT_REMOTE_PASSWORD", "synthetic-status-never-on-wire")
			_, source, _ := startOwner(t)
			remote := transferRemoteFixture(t, source)
			if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			base, st := clonedTransferStore(t, remote)
			if _, _, err := memory.New(st, memory.UserActor).AddTask(ctx, "local task", ""); err != nil {
				t.Fatal(err)
			}
			if _, _, err := memory.New(source, memory.UserActor).AddTask(ctx, "remote task", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			before := queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))")
			opts := localdolt.RepoStatusOptions{Diff: true}
			direct, err := st.RepoStatus(ctx, opts)
			if err != nil || direct.Status != "diverged-mergeable" {
				t.Fatalf("direct = %+v, %v", direct, err)
			}
			var calls atomic.Int32
			routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == storeipc.OperationPath {
						calls.Add(1)
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if !bytes.Contains(raw, []byte(`"operation":"repo_status"`)) || bytes.Contains(raw, []byte("synthetic-status")) {
							t.Error("not one complete password-free status request")
						}
						r.Body = io.NopCloser(bytes.NewReader(raw))
						if lost {
							inner.ServeHTTP(httptest.NewRecorder(), r)
							panic(http.ErrAbortHandler)
						}
					}
					inner.ServeHTTP(w, r)
				})
			})
			got, err := routed.RepoStatus(ctx, opts)
			if calls.Load() != 1 {
				t.Fatalf("status submitted %d times", calls.Load())
			}
			if lost {
				if err == nil || !strings.Contains(err.Error(), "inspect local main") || got.MainCommit != "" {
					t.Fatalf("lost reply = %+v, %v", got, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertJSONParity(t, "repo status", direct, got)
			}
			if after := queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))"); after != before || queryInt(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatal("owner preview changed main/working/staged state")
			}
			if _, err := routed.RepoStatus(ctx, localdolt.RepoStatusOptions{Local: true, Remote: "origin"}); err == nil || calls.Load() != 1 {
				t.Fatal("invalid status options reached owner")
			}
		})
	}
}
