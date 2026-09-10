package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

var errConfirmed = errors.New("synthetic failure after native Dolt commit")

// The injection is after the actual DOLT_COMMIT, before database/sql finishes.
// No fixture invents a hash or replaces the native statement execution.
type confirmedStore struct {
	*Store
	cancelFinalization bool
	readFailure        string
}

func (s confirmedStore) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	db, err := s.handle()
	if err != nil {
		return store.CommitResult{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return store.CommitResult{}, err
	}
	defer func() { _ = conn.Close() }()
	return s.commitConnFinalize(ctx, conn, req, func(tx *sql.Tx) error {
		if s.cancelFinalization {
			cancel()
			return tx.Commit()
		}
		return errors.Join(tx.Commit(), errConfirmed)
	})
}

func (s confirmedStore) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	if s.readFailure == "query" && strings.Contains(query, "FROM commands") {
		return nil, errConfirmed
	}
	rows, err := s.Store.Query(ctx, query, args...)
	if err == nil && s.readFailure == "close" && strings.Contains(query, "FROM commands") {
		return confirmedRows{rows}, nil
	}
	return rows, err
}

type confirmedRows struct{ store.Rows }

func (r confirmedRows) Close() error { return errors.Join(r.Rows.Close(), errConfirmed) }

func requireConfirmed(t *testing.T, hash string, err error) {
	t.Helper()
	if hash == "" || err == nil || !strings.Contains(err.Error(), hash) || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("confirmed result lost: hash=%q error=%v", hash, err)
	}
}

func reopenConfirmedStore(t *testing.T, st *Store) *Store {
	t.Helper()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(st.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	return fresh
}

func TestConfirmedDirectLanesSurviveNativeFinalization(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "late error", true: "native cancellation"}[canceled], func(t *testing.T) {
			ctx := context.Background()
			st, _ := documentFixture(t)
			before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
			lanes := memory.New(confirmedStore{Store: st, cancelFinalization: canceled}, memory.UserActor)
			task, hash, err := lanes.AddTask(ctx, "confirmed task", "details")
			requireConfirmed(t, hash, err)
			if task.ID == "" {
				t.Fatal("task identity lost")
			}
			done, hash, err := lanes.CompleteTask(ctx, task.ID)
			requireConfirmed(t, hash, err)
			blocked, hash, err := lanes.BlockTask(ctx, task.ID, "waiting")
			requireConfirmed(t, hash, err)
			if done.ID != task.ID || done.Status != memory.StatusDone || blocked.ID != task.ID || blocked.Status != memory.StatusBlocked {
				t.Fatal("confirmed task status lost")
			}
			note, hash, err := lanes.LogNoteWithProvenance(ctx, "confirmed note", memory.NoteProvenance{SessionID: "synthetic-session"})
			requireConfirmed(t, hash, err)
			prepared, err := lanes.PrepareNote("confirmed batch")
			if err != nil {
				t.Fatal(err)
			}
			hash, err = lanes.CommitNotes(ctx, []memory.Note{prepared})
			requireConfirmed(t, hash, err)
			if !strings.Contains(err.Error(), prepared.ID) {
				t.Fatal("batch identity missing from diagnostic")
			}
			command, hash, err := lanes.RecordCommand(ctx, "test", "go test ./...", 0)
			requireConfirmed(t, hash, err)
			if command.Kind != "test" || command.SuccessCount == nil || *command.SuccessCount != 1 {
				t.Fatal("confirmed command read-back lost")
			}
			state, hash, err := lanes.SetNarrative(ctx, memory.StateNarrative, "confirmed state")
			requireConfirmed(t, hash, err)
			arch, hash, err := lanes.SetNarrative(ctx, memory.ArchNarrative, "confirmed architecture")
			requireConfirmed(t, hash, err)
			if state.ID == "" || arch.ID == "" || note.ID == "" {
				t.Fatal("confirmed row identity lost")
			}
			st = reopenConfirmedStore(t, st)
			read := memory.New(st, memory.UserActor)
			tasks, err := read.Tasks(ctx, memory.StatusAny)
			if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID || tasks[0].Status != memory.StatusBlocked {
				t.Fatalf("reopened tasks=%+v %v", tasks, err)
			}
			notes, err := read.Notes(ctx, 10)
			if err != nil || len(notes) != 2 {
				t.Fatalf("reopened notes=%+v %v", notes, err)
			}
			if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before+8 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatal("late errors duplicated/lost commits or left dirty data")
			}
		})
	}
}

func TestConfirmedNativeUnobservedCancellationIsUnknown(t *testing.T) {
	st, _ := documentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	id := newID()
	if _, err := tx.ExecContext(ctx, "INSERT INTO session_notes (id, actor, text) VALUES (?, 'user', 'native cancellation')", id); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)", "unobserved cancellation", memory.UserActor.CommitAuthor().String())
	if err != nil || !rows.Next() {
		t.Fatalf("native commit did not return a row: %v", err)
	}
	// Native execution reached the result row. Cancel and close before Scan,
	// exercising the same lost-result boundary database/sql Row.Scan combines.
	cancel()
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var hash string
	scanErr := rows.Scan(&hash)
	result, err := nativeCommitResult(hash, 1, scanErr)
	if scanErr == nil || result.Hash != "" || !errors.Is(err, store.ErrCommitUnknown) {
		t.Fatalf("unobserved native result claimed rollback/commit: %+v %v", result, err)
	}
	_ = tx.Rollback()
	_ = conn.Close()
	st = reopenConfirmedStore(t, st)
	if internalCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'") != 1 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'unobserved cancellation'") != 1 {
		t.Fatal("native cancellation fixture did not preserve its unobserved commit")
	}
}

func TestConfirmedRefusalsClaimNoEffectsAndPreserveRoots(t *testing.T) {
	st, file := documentFixture(t)
	writeDocumentFixture(t, st.paths.ConfigFile(), "[deny_list]\npatterns=['BLOCKED']\n")
	writeDocumentFixture(t, file, "# BLOCKED document\n")
	before := statusSnapshot(t, st)
	ctx := context.Background()
	lanes := memory.New(confirmedStore{Store: st}, memory.UserActor)
	task, hash, err := lanes.AddTask(ctx, "BLOCKED task", "")
	if err == nil || hash != "" || task.ID != "" {
		t.Fatalf("refused task claimed effects: %+v %q %v", task, hash, err)
	}
	note, err := lanes.PrepareNote("BLOCKED batch")
	if err != nil {
		t.Fatal(err)
	}
	if hash, err := lanes.CommitNotes(ctx, []memory.Note{note}); err == nil || hash != "" {
		t.Fatalf("refused batch claimed commit: %q %v", hash, err)
	}
	if result, err := st.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor}); err == nil || result.Commit != "" || result.Document != nil {
		t.Fatalf("refused document claimed effects: %+v %v", result, err)
	}
	if after := statusSnapshot(t, st); after != before {
		t.Fatal("refusal changed roots, refs or history")
	}
}

func TestConfirmedCommandRetainsIdentityAfterReadbackFailure(t *testing.T) {
	for _, failure := range []string{"query", "close"} {
		t.Run(failure, func(t *testing.T) {
			st, _ := documentFixture(t)
			command, hash, err := memory.New(confirmedStore{Store: st, readFailure: failure}, memory.UserActor).
				RecordCommand(context.Background(), "build", "go build ./...", 0)
			requireConfirmed(t, hash, err)
			if command.Kind != "build" || command.Cmdline != nil || command.LastExitCode != nil || command.LastRunAt != nil || command.SuccessCount != nil || command.FailCount != nil || !strings.Contains(err.Error(), "read back") {
				t.Fatalf("read-back failure claimed unknown totals: %+v %v", command, err)
			}
			st = reopenConfirmedStore(t, st)
			persisted, err := memory.New(st, memory.UserActor).Command(context.Background(), "build")
			if err != nil || persisted.SuccessCount == nil || *persisted.SuccessCount != 1 {
				t.Fatalf("committed command missing: %+v %v", persisted, err)
			}
		})
	}
}

func TestConfirmedDocumentsSurviveNativeFinalization(t *testing.T) {
	st, file := documentFixture(t)
	writeDocumentFixture(t, file, "# Confirmed document\n\nNative commit survives.\n")
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	result, err := st.docAddFinalize(context.Background(), DocAddOptions{File: file, Actor: memory.UserActor},
		enableDocumentRecall, func(tx *sql.Tx) error { return errors.Join(tx.Commit(), errConfirmed) })
	requireConfirmed(t, result.Commit, err)
	if result.Document == nil || result.Document.ID == "" || len(result.Chunks) != 1 || result.EnabledDefaultRecall || !strings.Contains(err.Error(), "do not replay ingestion") {
		t.Fatalf("document late result=%+v %v", result, err)
	}
	st = reopenConfirmedStore(t, st)
	shown, err := st.DocShow(context.Background(), result.Document.ID)
	if err != nil || shown.Document == nil || shown.Document.ContentHash != result.Document.ContentHash || len(shown.Chunks) != 1 {
		t.Fatalf("reopened document=%+v %v", shown, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed, err := st.docRemove(ctx, result.Document.ID, memory.UserActor, func(tx *sql.Tx) error {
		cancel()
		return tx.Commit()
	})
	requireConfirmed(t, removed.Commit, err)
	if removed.Status != "removed" || removed.Document.ID != result.Document.ID {
		t.Fatal("removed identity lost")
	}
	st = reopenConfirmedStore(t, st)
	if internalCount(t, st, "SELECT COUNT(*) FROM documents AS OF 'main'") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM doc_chunks AS OF 'main'") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before+2 {
		t.Fatal("late removal lost its atomic rows/history")
	}
}
