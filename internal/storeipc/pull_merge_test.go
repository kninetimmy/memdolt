package storeipc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
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

func TestPullOwnerUnicodeBeforeMarshalAndRawDecode(t *testing.T) {
	ctx := context.Background()
	_, source, _ := startOwner(t)
	remote := transferRemoteFixture(t, source)
	if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	base, target := clonedTransferStore(t, remote)
	for i, st := range []*localdolt.Store{source, target} {
		if _, err := st.Commit(ctx, store.CommitRequest{Author: testActor, Message: "Unicode owner fixture", NoText: true, Statements: []store.Statement{{SQL: "UPDATE tasks SET notes = ? WHERE id='fixture'", Args: []any{[]string{"theirs", "ours"}[i]}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	replacements := []string{"bad\xfftext", `bad\ud800text`, `bad\udc00text`, `bad\ud800\u0041text`, `\ud83d\ude00\ufffd`}
	var mode, calls atomic.Int32
	routed := transferEndpoint(t, base, target, target, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == storeipc.OperationPath {
				calls.Add(1)
				if selected := mode.Load(); selected != 0 {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(bytes.Replace(data, []byte("OWNER_UNICODE_MARKER"), []byte(replacements[selected-1]), 1)))
				}
			}
			inner.ServeHTTP(w, r)
		})
	})
	shown, err := routed.Pull(ctx, localdolt.TransferOptions{})
	if err != nil || len(shown.Conflicts) != 1 {
		t.Fatalf("owner fixture = %+v, %v", shown, err)
	}
	before := queryString(t, target, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))")
	row := maps.Clone(shown.Conflicts[0].Rows[0].Ours)
	text := "bad\xfftext"
	row["notes"] = &text
	opts := localdolt.TransferOptions{Resolution: &localdolt.PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: []localdolt.PullChoice{{Conflict: shown.Conflicts[0].ID, Take: "manual", Row: row}}}}
	result, err := routed.Pull(ctx, opts)
	if err == nil || result.Status != "refused" || calls.Load() != 1 {
		t.Fatalf("typed malformed text was serialized: %+v, %v, calls=%d", result, err, calls.Load())
	}
	text = "OWNER_UNICODE_MARKER"
	for i := 0; i < len(replacements)-1; i++ {
		mode.Store(int32(i + 1))
		result, err := routed.Pull(ctx, opts)
		if err == nil || result.Changed || calls.Load() != int32(i+2) || queryString(t, target, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))") != before {
			t.Fatalf("raw malformed owner text was promoted: %+v, %v", result, err)
		}
	}
	mode.Store(int32(len(replacements)))
	result, err = routed.Pull(ctx, opts)
	if err != nil || !result.Changed || queryString(t, target, "SELECT notes FROM tasks WHERE id='fixture'") != "😀�" {
		t.Fatalf("valid owner surrogate pair changed: %+v, %v", result, err)
	}
}
