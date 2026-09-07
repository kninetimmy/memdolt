package storeipc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func transferRemoteFixture(t *testing.T, st *localdolt.Store) string {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(baseDir(t))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	remote := (&url.URL{Scheme: "file", Path: path}).String()
	_, err := st.Commit(ctx, store.CommitRequest{Author: testActor, Message: "configure synthetic remote and fixture", NoText: true, Statements: []store.Statement{
		{SQL: "CALL DOLT_REMOTE('add', 'origin', ?)", Args: []any{remote}},
		{SQL: "INSERT INTO tasks (id, title, status) VALUES ('fixture', 'fixture', 'open')"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func clonedTransferStore(t *testing.T, remote string) (string, *localdolt.Store) {
	t.Helper()
	base := baseDir(t)
	cfg := localdolt.Config{BaseDir: base, Actor: testActor, Logger: discardLogger()}
	if _, err := localdolt.Clone(context.Background(), cfg, remote, ""); err != nil {
		t.Fatal(err)
	}
	s, err := localdolt.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return base, s
}

func transferEndpoint(t *testing.T, base string, st *localdolt.Store, backend storeipc.Backend, wrap func(http.Handler) http.Handler) *storeipc.OwnerStore {
	t.Helper()
	handler, err := storeipc.NewHandler(storeipc.Config{Store: backend, ReviewAccept: reviewAcceptWithScorer(st, func() localdolt.ContradictionScorer { return fixedScorer(-100) }), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		handler = wrap(handler)
	}
	server, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: handler, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTransferOwnerLostResponsesAreNotResubmitted(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"push", "pull"} {
		t.Run(operation, func(t *testing.T) {
			base, source, endpoint := startOwner(t)
			remote := transferRemoteFixture(t, source)
			if err := endpoint.Close(); err != nil {
				t.Fatal(err)
			}
			st := source
			if operation == "pull" {
				if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
					t.Fatal(err)
				}
				base, st = clonedTransferStore(t, remote)
				if _, _, err := memory.New(source, memory.UserActor).AddTask(ctx, "incoming", "before lost response"); err != nil {
					t.Fatal(err)
				}
				if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
					t.Fatal(err)
				}
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
			var result localdolt.TransferResult
			var err error
			if operation == "push" {
				result, err = routed.Push(ctx, localdolt.TransferOptions{})
			} else {
				result, err = routed.Pull(ctx, localdolt.TransferOptions{})
			}
			if err == nil || result.Status != "unknown" || !strings.Contains(err.Error(), "inspect local and remote main") || calls.Load() != 1 {
				t.Fatalf("lost response = %+v, %v, calls=%d", result, err, calls.Load())
			}
			if operation == "push" {
				_, cloned := clonedTransferStore(t, remote)
				if queryInt(t, cloned, "SELECT COUNT(*) FROM tasks") != 1 {
					t.Fatal("lost push did not land")
				}
			} else if queryInt(t, st, "SELECT COUNT(*) FROM tasks") != 2 {
				t.Fatal("lost pull did not land")
			}
		})
	}
}

type transferFailureBackend struct{ storeipc.Backend }

func (s transferFailureBackend) Pull(ctx context.Context, opts localdolt.TransferOptions) (localdolt.TransferResult, error) {
	result, err := s.Backend.Pull(ctx, opts)
	if err == nil {
		err = errors.New("synthetic failure after confirmed promotion")
	}
	return result, err
}

func TestTransferOwnerPreservesPostSuccessResultAndUnboundedResponse(t *testing.T) {
	ctx := context.Background()
	_, source, _ := startOwner(t)
	remote := transferRemoteFixture(t, source)
	if _, err := source.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	base, destination := clonedTransferStore(t, remote)
	if _, _, err := memory.New(source, memory.UserActor).AddTask(ctx, "incoming", "post-success fixture"); err != nil {
		t.Fatal(err)
	}
	want, err := source.Push(ctx, localdolt.TransferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, destination, transferFailureBackend{destination}, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Exceed the three-second liveness-probe timeout after production
			// promotion, proving ordinary operations do not inherit that deadline.
			if r.URL.Path == storeipc.OperationPath {
				time.Sleep(3200 * time.Millisecond)
			}
			inner.ServeHTTP(w, r)
		})
	})
	got, err := routed.Pull(ctx, localdolt.TransferOptions{})
	if err == nil || !strings.Contains(err.Error(), "confirmed promotion") || !got.Changed || got.MainCommit != want.RemoteCommit || got.Status != "changed" {
		t.Fatalf("post-success wire result = %+v, %v", got, err)
	}
	if queryInt(t, destination, "SELECT COUNT(*) FROM tasks") != 2 {
		t.Fatal("promotion was lost")
	}
}

func TestTransferOwnerRefusalsPreserveLocalAndProposalWork(t *testing.T) {
	ctx := context.Background()
	sourceBase, source, _ := startOwner(t)
	remote := transferRemoteFixture(t, source)
	sourceOwner, err := storeipc.DialOwnerStore(sourceBase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceOwner.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	base, destination := clonedTransferStore(t, remote)
	routed := transferEndpoint(t, base, destination, destination, nil)
	proposal, err := routed.ProposeFact(ctx, localdolt.Proposal{Actor: testActor, Rationale: "keep pending", Target: localdolt.TargetRepo}, localdolt.Fact{Key: "pending.transfer", Value: "do not publish"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.New(routed, memory.UserActor).AddTask(ctx, "B local", "keep"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.New(sourceOwner, memory.UserActor).AddTask(ctx, "A remote", "keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceOwner.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"push", "pull"} {
		var got localdolt.TransferResult
		if operation == "push" {
			got, err = routed.Push(ctx, localdolt.TransferOptions{})
		} else {
			got, err = routed.Pull(ctx, localdolt.TransferOptions{})
		}
		if err == nil || got.Changed {
			t.Fatalf("divergent routed %s = %+v, %v", operation, got, err)
		}
	}
	if queryInt(t, destination, "SELECT COUNT(*) FROM tasks") != 2 || queryInt(t, destination, "SELECT COUNT(*) FROM facts") != 0 {
		t.Fatal("routed refusal changed durable work")
	}
	pending, err := routed.PendingProposals(ctx)
	if err != nil || len(pending) != 1 || pending[0].Commit != proposal.Commit {
		t.Fatalf("pending work changed: %+v, %v", pending, err)
	}
	if err := os.WriteFile(filepath.Join(base, ".memdolt", "config.toml"), []byte("[deny_list]\npatterns=['B local']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routed.Push(ctx, localdolt.TransferOptions{}); err == nil || !strings.Contains(err.Error(), "deny-list") {
		t.Fatalf("routed scan refusal = %v", err)
	}
}
