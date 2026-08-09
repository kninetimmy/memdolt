package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

// TestMigrationsAreContiguousAndNumbered guards the numbering the runner
// depends on: it applies every migration whose version exceeds the store's
// and then records the last one it applied, which is only the store's true
// version if versions ascend by one from 1.
func TestMigrationsAreContiguousAndNumbered(t *testing.T) {
	migrations := store.Migrations()
	if len(migrations) == 0 {
		t.Fatal("the binary ships no migrations, so no store can be initialized")
	}
	for i, migration := range migrations {
		if migration.Version != i+1 {
			t.Fatalf("migration %d has version %d, want %d (versions are 1-based and contiguous)",
				i, migration.Version, i+1)
		}
		if strings.TrimSpace(migration.Name) == "" {
			t.Errorf("migration %d has no name; it would produce a nameless commit message", migration.Version)
		}
		if len(migration.Statements) == 0 {
			t.Errorf("migration %d has no statements; it would fail with nothing to commit", migration.Version)
		}
	}
	if got, want := store.LatestSchemaVersion(), len(migrations); got != want {
		t.Fatalf("LatestSchemaVersion = %d, want %d", got, want)
	}
}

// TestMigrationsIsNotAliasedToTheBinarysOwnHistory checks that a caller
// cannot rewrite the shipped schema history through the returned slice.
func TestMigrationsIsNotAliasedToTheBinarysOwnHistory(t *testing.T) {
	migrations := store.Migrations()
	migrations[0].Version = 99
	if store.Migrations()[0].Version != 1 {
		t.Fatal("mutating the returned slice rewrote the shipped migration history")
	}
}

func TestMigrationTag(t *testing.T) {
	if got, want := store.MigrationTag(1), "migration/1"; got != want {
		t.Fatalf("MigrationTag(1) = %q, want %q", got, want)
	}
}

// TestCheckSchemaVersionRefusesAStoreNewerThanTheBinary covers PRD §6.4's
// version-skew rule: older and current stores are usable, a newer one is
// refused with an error that tells the operator to upgrade.
func TestCheckSchemaVersionRefusesAStoreNewerThanTheBinary(t *testing.T) {
	latest := store.LatestSchemaVersion()

	for _, version := range []int{0, latest} {
		if err := store.CheckSchemaVersion(version); err != nil {
			t.Fatalf("store at schema version %d was refused: %v", version, err)
		}
	}

	err := store.CheckSchemaVersion(latest + 1)
	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("store at schema version %d: error = %v, want one matching ErrSchemaTooNew", latest+1, err)
	}
	if !strings.Contains(err.Error(), "memdolt upgrade") {
		t.Fatalf("refusal %q does not tell the operator to upgrade", err)
	}
}
