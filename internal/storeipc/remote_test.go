package storeipc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestRemoteOwnerSubmitsOnceWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	t.Setenv("DOLT_REMOTE_PASSWORD", "synthetic-secret-not-on-wire")
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == storeipc.OperationPath {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var request struct {
					Operation string                     `json:"operation"`
					Args      map[string]json.RawMessage `json:"args"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Error(err)
					return
				}
				if request.Operation == "add_remote" {
					calls.Add(1)
					if len(request.Args) != 3 || bytes.Contains(raw, []byte("synthetic-secret")) {
						t.Error("unexpected remote operands or credentials in IPC")
					}
					inner.ServeHTTP(httptest.NewRecorder(), r)
					panic(http.ErrAbortHandler)
				}
			}
			inner.ServeHTTP(w, r)
		})
	})
	remote := localdolt.Remote{Name: "new", URL: "https://example.invalid/db", User: "fixture"}
	got, err := routed.AddRemote(ctx, remote)
	if err == nil || got.Name != "" || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "memdolt repo remote list") || calls.Load() != 1 {
		t.Fatalf("lost response=%+v, %v, calls=%d", got, err, calls.Load())
	}
	listed, err := routed.ListRemotes(ctx)
	if err != nil || len(listed) != 1 || listed[0] != remote {
		t.Fatalf("persisted remote after lost response=%+v, %v", listed, err)
	}
	_, err = routed.AddRemote(ctx, localdolt.Remote{Name: "secret", URL: "https://user:synthetic-secret@host/db"})
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") || calls.Load() != 1 {
		t.Fatal("rejected URL credentials reached IPC or its error")
	}
}

type remoteFailureBackend struct{ storeipc.Backend }

func (s remoteFailureBackend) AddRemote(ctx context.Context, remote localdolt.Remote) (localdolt.Remote, error) {
	result, err := s.Backend.AddRemote(ctx, remote)
	if err == nil {
		err = errors.New("synthetic failure after confirmed configuration")
	}
	return result, err
}

func TestRemoteOwnerPreservesConfirmedResultAndError(t *testing.T) {
	ctx := context.Background()
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, remoteFailureBackend{st}, nil)
	want := localdolt.Remote{Name: "new", URL: "https://example.invalid/db"}
	got, err := routed.AddRemote(ctx, want)
	if err == nil || !strings.Contains(err.Error(), "confirmed configuration") || got != want {
		t.Fatalf("confirmation wire=%+v, %v", got, err)
	}
}
