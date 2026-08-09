package localdolt

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

// TestAFailedMigrationLeavesNoTableBehind covers the one thing Dolt does
// not give the runner for free: DDL is not rolled back with the
// transaction that ran it. A migration that creates a table and then fails
// would otherwise leave that table in the working set, where it permanently
// breaks the re-run the runner's idempotence promises ("table already
// exists") and where the next commit's DOLT_COMMIT('-A') would sweep it
// into an unrelated commit.
func TestAFailedMigrationLeavesNoTableBehind(t *testing.T) {
	ctx := context.Background()
	st := openInternalTestStore(t)

	// facts is migration 1's first table, so an orphan left here is exactly
	// what would break the real migration below.
	broken := store.Migration{
		Version: 1,
		Name:    "broken",
		Statements: []store.Statement{
			{SQL: "CREATE TABLE facts (id CHAR(26) PRIMARY KEY)"},
			{SQL: "INSERT INTO a_table_that_does_not_exist (id) VALUES ('x')"},
		},
	}
	if _, err := st.applyMigration(ctx, st.db, broken); err == nil {
		t.Fatal("a migration whose second statement is invalid reported success")
	}

	if got := countInternal(t, st, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?", DatabaseName); got != 0 {
		t.Fatalf("the failed migration left %d tables behind, want 0", got)
	}

	// The store is still migratable, which is the property that matters.
	result, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate after a failed migration: %v", err)
	}
	if result.Version != store.LatestSchemaVersion() {
		t.Fatalf("schema version = %d, want %d", result.Version, store.LatestSchemaVersion())
	}

	// And nothing from the failed attempt was swept into its commit: one
	// commit per migration on top of Dolt's own first commit.
	wantCommits := 1 + store.LatestSchemaVersion()
	if got := countInternal(t, st, "SELECT COUNT(*) FROM dolt_log"); got != wantCommits {
		t.Fatalf("history has %d commits, want %d", got, wantCommits)
	}
}

func openInternalTestStore(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-migrate")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	st, err := New(Config{
		BaseDir: dir,
		Actor:   store.Actor{Name: "user", Email: "user@memdolt.invalid"},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countInternal(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := st.db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}
