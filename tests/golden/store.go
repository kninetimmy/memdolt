//go:build golden

package golden

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	// Registers the "dolt" database/sql driver.
	_ "github.com/dolthub/driver"
)

// databaseName is the Dolt database this rig seeds. Deliberately not
// internal/store/localdolt.DatabaseName: this rig store is self-contained
// (PRD §6.1-shaped tables in a throwaway data directory), not a real
// memdolt repository, so it has no reason to couple to that package.
const databaseName = "rig3"

// openDisposableStore opens a fresh embedded Dolt engine rooted at dataDir
// (a caller-owned, throwaway t.TempDir()) and ensures databaseName exists.
// There is no single-owner lock here — unlike internal/store/localdolt,
// this store is never shared across processes, so PRD §5.2's concern does
// not apply.
func openDisposableStore(ctx context.Context, dataDir string) (*sql.DB, error) {
	dsn, err := buildDSN(dataDir, databaseName)
	if err != nil {
		return nil, fmt.Errorf("golden: %w", err)
	}
	db, err := sql.Open("dolt", dsn)
	if err != nil {
		return nil, fmt.Errorf("golden: open embedded dolt engine at %s: %w", dataDir, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("golden: ping embedded dolt engine at %s: %w", dataDir, err)
	}
	create := "CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(databaseName)
	if _, err := db.ExecContext(ctx, create); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("golden: create database %q: %w", databaseName, err)
	}
	return db, nil
}

// buildDSN renders the embedded driver's data source name for a Dolt data
// directory (mirrors internal/store/localdolt/dsn.go's buildDSN — the
// driver's DSN parser is literal, so the path goes in verbatim with forward
// slashes and only the query string is encoded).
func buildDSN(dataDir, database string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve dolt data directory %q: %w", dataDir, err)
	}
	slashed := filepath.ToSlash(abs)
	if strings.Contains(slashed, "?") {
		return "", fmt.Errorf("dolt data directory %q contains '?', which the embedded driver's data source parser treats as the start of the query string", abs)
	}
	params := url.Values{}
	params.Set("commitname", "rig3")
	params.Set("commitemail", "rig3@memdolt.invalid")
	params.Set("database", database)
	return "file://" + slashed + "?" + params.Encode(), nil
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
