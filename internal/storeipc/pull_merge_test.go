package storeipc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

type resolutionFailureBackend struct{ storeipc.Backend }

func (s resolutionFailureBackend) Pull(ctx context.Context, opts localdolt.TransferOptions) (localdolt.TransferResult, error) {
	result, err := s.Backend.Pull(ctx, opts)
	if result.Changed {
		err = errors.Join(err, errors.New("synthetic late resolution failure"))
	}
	return result, err
}

func TestPullOwnerCompleteResolutionAndLostOrLateReplies(t *testing.T) {
	for _, failure := range []string{"none", "lost", "late"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			_, source, _ := startOwner(t)
			remote := transferRemoteFixture(t, source)
			if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			base, target := clonedTransferStore(t, remote)
			for i, st := range []*localdolt.Store{source, target} {
				if _, err := st.Commit(ctx, store.CommitRequest{Author: testActor, Message: "synthetic owner conflict", NoText: true, Statements: []store.Statement{{SQL: "UPDATE tasks SET notes = ? WHERE id = 'fixture'", Args: []any{[]string{"theirs", "ours"}[i]}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var backend storeipc.Backend = target
			if failure == "late" {
				backend = resolutionFailureBackend{target}
			}
			routed := transferEndpoint(t, base, target, backend, func(inner http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == storeipc.OperationPath && calls.Add(1) == 3 && failure == "lost" {
						inner.ServeHTTP(httptest.NewRecorder(), r)
						panic(http.ErrAbortHandler)
					}
					inner.ServeHTTP(w, r)
				})
			})
			before := queryString(t, target, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))")
			shown, err := routed.Pull(ctx, localdolt.TransferOptions{})
			if err != nil || shown.Status != "conflicted" || len(shown.Conflicts) != 1 || shown.Conflicts[0].Rows[0].OursBlame == nil {
				t.Fatalf("routed preview = %+v %v", shown, err)
			}
			resolution := &localdolt.PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit}
			if _, err := routed.Pull(ctx, localdolt.TransferOptions{Resolution: resolution}); err == nil {
				t.Fatal("routed incomplete choices merged")
			}
			if queryString(t, target, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))") != before {
				t.Fatal("preview/refusal changed roots")
			}
			resolution.Choices = []localdolt.PullChoice{{Conflict: shown.Conflicts[0].ID, Take: "theirs"}}
			result, err := routed.Pull(ctx, localdolt.TransferOptions{Resolution: resolution})
			if failure == "lost" {
				if err == nil || result.Status != "unknown" || !strings.Contains(err.Error(), "outcome unknown") {
					t.Fatalf("lost resolution = %+v %v", result, err)
				}
			} else if (err != nil) != (failure == "late") || !result.Changed || result.Status != "changed" || result.LocalCommit != shown.LocalCommit || result.RemoteCommit != shown.RemoteCommit {
				t.Fatalf("resolution = %+v %v", result, err)
			}
			if calls.Load() != 3 || queryString(t, target, "SELECT notes FROM tasks WHERE id='fixture'") != "theirs" || queryInt(t, target, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatalf("operation replay or incomplete commit, calls=%d", calls.Load())
			}
		})
	}
}
