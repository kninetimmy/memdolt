package storeipc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestDocumentOwnerLostMutationRepliesSubmitOnce(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"doc_add", "doc_remove"} {
		t.Run(operation, func(t *testing.T) {
			base, st, endpoint := startOwner(t)
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := endpoint.Close(); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(base, "doc.md")
			if err := os.WriteFile(file, []byte("# Durable through a lost reply\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := localdolt.DocAddOptions{File: file, Actor: memory.UserActor}
			if operation == "doc_remove" {
				if _, err := st.DocAdd(ctx, opts); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
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
							Operation string `json:"operation"`
						}
						if err := json.Unmarshal(raw, &request); err != nil {
							t.Error(err)
							return
						}
						if request.Operation == operation {
							calls.Add(1)
							inner.ServeHTTP(httptest.NewRecorder(), r)
							panic(http.ErrAbortHandler)
						}
					}
					inner.ServeHTTP(w, r)
				})
			})
			var result localdolt.DocResult
			var err error
			if operation == "doc_add" {
				result, err = routed.DocAdd(ctx, opts)
			} else {
				result, err = routed.DocRemove(ctx, file, memory.UserActor)
			}
			if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "memdolt doc ls") || result.Commit != "" || calls.Load() != 1 {
				t.Fatalf("lost result=%+v, %v, calls=%d", result, err, calls.Load())
			}
			shown, err := routed.DocShow(ctx, file)
			if err != nil || (operation == "doc_add" && shown.Status != "found") || (operation == "doc_remove" && shown.Status != "not-found") {
				t.Fatalf("inspection after lost reply=%+v, %v", shown, err)
			}
			listed, err := routed.DocList(ctx)
			if err != nil || (operation == "doc_add" && len(listed) != 1) || (operation == "doc_remove" && len(listed) != 0) {
				t.Fatalf("list after lost reply=%+v, %v", listed, err)
			}
		})
	}
}

type documentFailureBackend struct{ storeipc.Backend }

func (s documentFailureBackend) DocAdd(ctx context.Context, opts localdolt.DocAddOptions) (localdolt.DocResult, error) {
	result, err := s.Backend.DocAdd(ctx, opts)
	if err == nil {
		err = errors.New("synthetic post-commit document configuration failure")
	}
	return result, err
}

func TestDocumentOwnerCarriesConfirmedResultWithError(t *testing.T) {
	ctx := context.Background()
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	routed := transferEndpoint(t, base, st, documentFailureBackend{st}, nil)
	file := filepath.Join(base, "confirmed.md")
	if err := os.WriteFile(file, []byte("# Confirmed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := routed.DocAdd(ctx, localdolt.DocAddOptions{File: file, Actor: memory.UserActor})
	if err == nil || !strings.Contains(err.Error(), "post-commit document") || result.Status != "created" || result.Commit == "" || result.Document == nil || len(result.Chunks) != 1 {
		t.Fatalf("confirmed wire result=%+v, %v", result, err)
	}
	shown, err := routed.DocShow(ctx, result.Document.ID)
	if err != nil || shown.Status != "found" || shown.Document.ContentHash != result.Document.ContentHash {
		t.Fatalf("confirmed document disappeared=%+v, %v", shown, err)
	}
}
