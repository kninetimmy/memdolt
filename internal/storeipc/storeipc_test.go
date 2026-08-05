package storeipc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

var testActor = store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// baseDir returns a scratch repository root. It does not use t.TempDir
// because that fails the test if the directory cannot be removed, and the
// embedded engine memory-maps files inside the Dolt data directory that
// Windows may still hold briefly after the store is closed.
func baseDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-storeipc")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startOwner opens a store and serves it on this repository's IPC
// endpoint, exactly as the owning process does.
func startOwner(t *testing.T) (string, *localdolt.Store, *ipc.Server) {
	t.Helper()
	return startOwnerWrapping(t, nil)
}

// startOwnerWrapping is startOwner with the owner's routes wrapped, which is
// how a test puts a fault between the store and the answer that reports it.
func startOwnerWrapping(t *testing.T, wrap func(http.Handler) http.Handler) (string, *localdolt.Store, *ipc.Server) {
	t.Helper()
	base := baseDir(t)

	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: testActor, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	routes, err := storeipc.NewHandler(storeipc.Config{Store: st, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	if wrap != nil {
		routes = wrap(routes)
	}
	srv, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: routes, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return base, st, srv
}

// TestClientWritesAndReadsThroughTheOwner covers the acceptance criterion
// that a second party's store operations are routed over the internal/ipc
// endpoint: the write must land in the owner's store, and the read must
// come back through the same endpoint.
func TestClientWritesAndReadsThroughTheOwner(t *testing.T) {
	ctx := context.Background()
	base, owner, _ := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	commit, err := client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY, v TEXT, n BIGINT)"},
			{SQL: "INSERT INTO probe (k, v, n) VALUES (?, ?, ?)", Args: []any{"msrv", "1.26.2", 7}},
		},
		Message: "propose fact msrv=1.26.2",
		Author:  storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	})
	if err != nil {
		t.Fatalf("commit through the endpoint: %v", err)
	}
	if commit.Hash == "" {
		t.Fatal("commit returned an empty hash")
	}
	if commit.RowsAffected != 1 {
		t.Fatalf("rows affected = %d, want 1", commit.RowsAffected)
	}

	grid, err := client.Query(ctx, storeipc.QueryRequest{
		SQL:  "SELECT v, n, NULL FROM probe WHERE k = ?",
		Args: []any{"msrv"},
	})
	if err != nil {
		t.Fatalf("query through the endpoint: %v", err)
	}
	if len(grid.Rows) != 1 || len(grid.Rows[0]) != 3 {
		t.Fatalf("result grid = %+v, want one row of three cells", grid.Rows)
	}
	if got := grid.Rows[0][0]; got == nil || *got != "1.26.2" {
		t.Fatalf("text cell = %v, want %q", got, "1.26.2")
	}
	// The integer must survive JSON as the integer it was, not as a float.
	if got := grid.Rows[0][1]; got == nil || *got != "7" {
		t.Fatalf("integer cell = %v, want %q", got, "7")
	}
	if grid.Rows[0][2] != nil {
		t.Fatalf("null cell = %v, want nil", *grid.Rows[0][2])
	}

	// The write really went into the owner's store, not somewhere the
	// endpoint invented.
	rows, err := owner.Query(ctx, "SELECT v FROM probe WHERE k = ?", "msrv")
	if err != nil {
		t.Fatalf("query the owner's store directly: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("the write routed over the endpoint is not in the owner's store (err %v)", rows.Err())
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if value != "1.26.2" {
		t.Fatalf("owner's store holds %q, want %q", value, "1.26.2")
	}
}

// TestARefusedWriteIsDistinguishableFromALostAnswer covers the property the
// M0 rig's accounting rests on: a write the owner refused must be
// reportable as "did not happen", and must not look like a request whose
// outcome is unknown.
func TestARefusedWriteIsDistinguishableFromALostAnswer(t *testing.T) {
	base, _, _ := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, err = client.Commit(context.Background(), storeipc.CommitRequest{
		Statements: []storeipc.Statement{{SQL: "INSERT INTO no_such_table (k) VALUES (?)", Args: []any{"x"}}},
		Message:    "write to a table that does not exist",
		Author:     storeipc.Actor{Name: "user", Email: "user@memdolt.invalid"},
	})
	if err == nil {
		t.Fatal("a write against a missing table was reported as committed")
	}
	if !storeipc.IsOwnerRefusal(err) {
		t.Fatalf("error = %v, want one the caller can read as an owner refusal", err)
	}

	var status *ipc.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v does not carry the owner's status", err)
	}
	if status.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status.Code, http.StatusInternalServerError)
	}
	if !bytes.Contains([]byte(status.Body), []byte("no_such_table")) {
		t.Fatalf("owner's answer %q does not say why the write failed", status.Body)
	}
}

// dropFirstAnswer runs the owner's routes and then aborts the connection
// before the first answer is written, which is the failure rig 1 caught:
// the owner applies the write and the client never learns that it did
// (docs/spikes/m0-rig1.md, F3).
func dropFirstAnswer(inner http.Handler) http.Handler {
	var dropped atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dropped.CompareAndSwap(false, true) {
			inner.ServeHTTP(w, r)
			return
		}
		// The answer is written to a recorder that goes nowhere, and then
		// the connection is closed without one; net/http closes it quietly
		// on ErrAbortHandler.
		inner.ServeHTTP(httptest.NewRecorder(), r)
		panic(http.ErrAbortHandler)
	})
}

// TestALostAnswerIsNotResubmitted covers the acceptance criterion that the
// client machinery never resubmits the operation that hit a transport
// failure. A write over IPC is at-least-once, so a resubmission is a
// duplicate write, and M0 has no way to make one harmless.
func TestALostAnswerIsNotResubmitted(t *testing.T) {
	ctx := context.Background()
	base, owner, _ := startOwnerWrapping(t, dropFirstAnswer)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, err = client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY)"},
			{SQL: "INSERT INTO probe (k) VALUES (?)", Args: []any{"lost-answer"}},
		},
		Message: "a write whose answer never arrives",
		Author:  storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	})
	if err == nil {
		t.Fatal("a write whose answer was dropped was reported as committed")
	}
	// The owner never answered, so this is not a refusal: the outcome is
	// unknown to the caller, and that is the whole point.
	if storeipc.IsOwnerRefusal(err) {
		t.Fatalf("a lost answer was reported as an owner refusal: %v", err)
	}
	// The owner is still live, so the caller must not be told the store is
	// free to open.
	if errors.Is(err, ipc.ErrNoLiveOwner) {
		t.Fatalf("a live owner was reported as no live owner: %v", err)
	}

	// The same client, without being rebuilt, reaches the owner again — and
	// carries a second, distinct write rather than the one that failed.
	if _, err := client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{{SQL: "INSERT INTO probe (k) VALUES (?)", Args: []any{"answered"}}},
		Message:    "a write whose answer arrives",
		Author:     storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("commit after the lost answer: %v", err)
	}

	rows, err := owner.Query(ctx, "SELECT k, COUNT(*) FROM probe GROUP BY k ORDER BY k")
	if err != nil {
		t.Fatalf("query the owner's store directly: %v", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[string]int64{}
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			t.Fatalf("scan: %v", err)
		}
		applied[key] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	// The dropped answer's write did land — the table it created is being
	// read here — and it landed exactly once.
	if applied["lost-answer"] != 1 {
		t.Fatalf("the write whose answer was lost was applied %d times, want exactly 1 (rows %v)",
			applied["lost-answer"], applied)
	}
	if applied["answered"] != 1 {
		t.Fatalf("the write made after the failure was applied %d times, want exactly 1 (rows %v)",
			applied["answered"], applied)
	}
}

// TestAnOperationAfterTheOwnerStopsSaysTheStoreIsFree covers, at the layer a
// CLI uses, the acceptance criterion that a transport failure against a
// repository no live owner holds is reported as such: PRD §5.2's design
// response then has the caller open the store directly.
func TestAnOperationAfterTheOwnerStopsSaysTheStoreIsFree(t *testing.T) {
	base, _, srv := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close the owner's endpoint: %v", err)
	}

	_, err = client.Query(context.Background(), storeipc.QueryRequest{SQL: "SELECT 1"})
	if !errors.Is(err, ipc.ErrNoLiveOwner) {
		t.Fatalf("error = %v, want one matching ipc.ErrNoLiveOwner", err)
	}
	if storeipc.IsOwnerRefusal(err) {
		t.Fatalf("a transport failure was reported as an owner refusal: %v", err)
	}
}

// TestStoreRoutesAreBehindTheTokenCheck covers the acceptance criterion
// that routing store operations over the endpoint does not widen its
// security posture: the routes carry writes, so an untokened request must
// be refused before it reaches the store.
func TestStoreRoutesAreBehindTheTokenCheck(t *testing.T) {
	ctx := context.Background()
	_, owner, srv := startOwner(t)

	for _, path := range []string{storeipc.CommitPath, storeipc.QueryPath} {
		body := `{"statements":[{"sql":"CREATE TABLE untokened (k VARCHAR(8) PRIMARY KEY)"}],` +
			`"message":"untokened","author":{"name":"user","email":"user@memdolt.invalid"},` +
			`"sql":"SELECT 1"}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+srv.Addr()+path,
			bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	// And the refused write never reached the store.
	rows, err := owner.Query(ctx, "SHOW TABLES LIKE 'untokened'")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("an untokened request created a table")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("show tables rows: %v", err)
	}
}
