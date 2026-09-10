package storeipc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestHistoryOwnerParityAndLostRepliesNeverReplay(t *testing.T) {
	ctx := context.Background()
	base, st, endpoint := startOwner(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	commit, err := st.Commit(ctx, store.CommitRequest{Author: testActor, Message: "history owner fixture", NoText: true, Statements: []store.Statement{
		{SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES ('f', 'history.owner', '', NULL, NULL)"},
		{SQL: "INSERT INTO decisions (id, title, rationale, status) VALUES ('d', NULL, '', NULL)"},
		{SQL: "INSERT INTO project_state (id, body, actor_raw) VALUES ('s', NULL, '')"},
		{SQL: "INSERT INTO project_arch (id, body, created_at) VALUES ('a', 'stored old text', '2000-01-01 00:00:00')"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.New(st, memory.UserActor).SetNarrative(ctx, memory.StateNarrative, "latest owner state"); err != nil {
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
	before := queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))")
	for _, subject := range []string{"fact", "decision", "state", "arch"} {
		opts := localdolt.HistoryOptions{Subject: subject, Limit: 25}
		if subject == "fact" || subject == "decision" {
			id := subject[:1]
			opts.ID = &id
		}
		for _, revision := range []*string{nil, &commit.Hash} {
			opts.AsOf = revision
			want, err := st.History(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := owner.History(ctx, opts)
			// Wire timestamps can have different time.Location identities;
			// compare the complete canonical JSON, including every NULL field.
			wantJSON, marshalErr := json.Marshal(want)
			gotJSON, gotErr := json.Marshal(got)
			if err != nil || marshalErr != nil || gotErr != nil || string(wantJSON) != string(gotJSON) {
				t.Fatalf("%s owner parity: %s != %s; %v %v %v", subject, gotJSON, wantJSON, err, marshalErr, gotErr)
			}
		}
	}
	opts := localdolt.HistoryOptions{Subject: "state", Limit: 25}
	lose.Store(true)
	calls.Store(0)
	if result, err := owner.History(ctx, opts); err == nil || !reflect.DeepEqual(result, localdolt.HistoryResult{}) || calls.Load() != 1 {
		t.Fatalf("lost history was reused/replayed: %+v, %v, calls=%d", result, err, calls.Load())
	}
	lose.Store(false)
	calls.Store(0)
	invalid := string([]byte{0xff})
	if _, err := owner.History(ctx, localdolt.HistoryOptions{Subject: "fact", ID: &invalid, Limit: 25}); err == nil || calls.Load() != 0 {
		t.Fatal("invalid UTF-8 crossed the owner boundary")
	}
	if after := queryString(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'))"); after != before {
		t.Fatal("owner history changed native roots")
	}
	if _, _, err := memory.New(owner, memory.UserActor).LogNote(ctx, "ordinary owner write after history"); err != nil {
		t.Fatal(err)
	}
}
