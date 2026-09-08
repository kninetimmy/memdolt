package storeipc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestGlobalCapturesOwnerResultsAndLostRepliesNeverReplay(t *testing.T) {
	ctx := context.Background()
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	fact, err := st.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "capture.owner", Value: "scopebeacon"}, Source: "user", Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	var lose atomic.Bool
	var calls atomic.Int32
	owner := transferEndpoint(t, base, st, st, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == storeipc.OperationPath {
				calls.Add(1)
				if lose.Load() {
					inner.ServeHTTP(httptest.NewRecorder(), r)
					panic(http.ErrAbortHandler)
				}
			}
			inner.ServeHTTP(w, r)
		})
	})
	record, err := owner.CapturePromotion(ctx, "fact", fact.ID)
	if err != nil || record.Commit != fact.Commit || record.Row["evidence"] != nil || record.Row["id"] == nil || *record.Row["id"] != fact.ID {
		t.Fatalf("owner capture lost nullable row: %+v %v", record, err)
	}
	options := store.RecallSnapshotOptions{Query: "scopebeacon", SourceTypes: []string{"fact"}, Provenance: true}
	snapshot, err := owner.CaptureRecall(ctx, options)
	if err != nil || snapshot.Commit != fact.Commit || len(snapshot.Sources) != 1 || len(snapshot.Embeddings) != 1 || len(snapshot.Lexical) != 1 || snapshot.Provenance["fact/"+fact.ID] == nil {
		t.Fatalf("owner snapshot=%+v %v", snapshot, err)
	}
	lose.Store(true)
	calls.Store(0)
	if record, err := owner.CapturePromotion(ctx, "fact", fact.ID); err == nil || record.Row != nil || calls.Load() != 1 {
		t.Fatalf("lost promotion capture reused or retried: %+v %v calls=%d", record, err, calls.Load())
	}
	calls.Store(0)
	if snapshot, err := owner.CaptureRecall(ctx, options); err == nil || snapshot.Commit != "" || calls.Load() != 1 {
		t.Fatalf("lost recall capture reused or retried: %+v %v calls=%d", snapshot, err, calls.Load())
	}
	calls.Store(0)
	if _, err := owner.CapturePromotion(ctx, "fact", string([]byte{0xff})); err == nil || calls.Load() != 0 {
		t.Fatal("lossy promotion identifier reached IPC")
	}
}
