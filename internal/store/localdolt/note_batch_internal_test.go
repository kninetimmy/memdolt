package localdolt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/store"
)

func TestCleanCommitRefusesToSweepAnUnrelatedWorkingSet(t *testing.T) {
	ctx := context.Background()
	actor := store.Actor{Name: "agent:test", Email: "agent-test@memdolt.invalid"}
	st, err := New(Config{BaseDir: t.TempDir(), Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	db, err := st.handle()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO tasks (id, title, status, created_at, updated_at) VALUES (?, ?, 'open', ?, ?)",
		"01UNRELATED00000000000000", "unrelated dirty task", now, now); err != nil {
		t.Fatal(err)
	}
	commitsBefore := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")

	_, err = st.Commit(ctx, store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO session_notes (id, actor, actor_raw, text, created_at) VALUES (?, ?, ?, ?, ?)",
			Args: []any{"01NOTE0000000000000000000", actor.Name, "test", "batched note", now},
		}},
		Text:         []string{"batched note", "test"},
		RequireClean: true,
		Message:      "note batch (1)",
		Author:       actor,
	})
	if err == nil || !strings.Contains(err.Error(), "uncommitted table change") {
		t.Fatalf("clean note commit error = %v, want dirty-working-set refusal", err)
	}
	if got := internalCount(t, st, "SELECT COUNT(*) FROM session_notes"); got != 0 {
		t.Fatalf("refused batch left %d note rows", got)
	}
	if got := internalCount(t, st, "SELECT COUNT(*) FROM tasks"); got != 1 {
		t.Fatalf("refused batch changed the unrelated working set: %d tasks", got)
	}
	if got := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log"); got != commitsBefore {
		t.Fatalf("refused batch changed history from %d to %d commits", commitsBefore, got)
	}
}

func internalCount(t *testing.T, st *Store, query string) int {
	t.Helper()
	rows, err := st.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query %q returned no row: %v", query, rows.Err())
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
