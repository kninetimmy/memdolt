//go:build golden

package golden

import (
	"context"
	"slices"
	"testing"
)

func TestFactKeyPrefixAndFulltextContract(t *testing.T) {
	ctx := context.Background()
	db, err := openDisposableStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("opening disposable store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := createSchema(ctx, db); err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO facts
		(id, `+"`key`"+`, value, source, verified_at, created_at, superseded_by)
		VALUES
		('fact-build-old', 'build.command', 'legacy cargo command', 'user', NOW(), NOW(), 'fact-build-new'),
		('fact-build-new', 'build.command', 'current cargo command', 'user', NOW(), NOW(), NULL),
		('fact-build-cache', 'build.cache', 'reuse compiled artifacts', 'user', NOW(), NOW(), NULL),
		('fact-env-shell', 'env.shell', 'PowerShell', 'user', NOW(), NOW(), NULL)`)
	if err != nil {
		t.Fatalf("seeding facts: %v", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT id FROM facts WHERE `key` LIKE ? ORDER BY id", "build.%")
	if err != nil {
		t.Fatalf("filtering facts by dotted key prefix: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var prefixIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning dotted-prefix fact: %v", err)
		}
		prefixIDs = append(prefixIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading dotted-prefix facts: %v", err)
	}
	wantPrefix := []string{"fact-build-cache", "fact-build-new", "fact-build-old"}
	if !slices.Equal(prefixIDs, wantPrefix) {
		t.Fatalf("dotted-prefix ids = %v, want %v", prefixIDs, wantPrefix)
	}

	hits, err := newFulltextGatherer(db).Gather(ctx, "fact", "cargo", 10)
	if err != nil {
		t.Fatalf("gathering facts through FULLTEXT: %v", err)
	}
	fulltextIDs := make([]string, len(hits))
	for i, hit := range hits {
		fulltextIDs[i] = hit.id
	}
	slices.Sort(fulltextIDs)
	wantFulltext := []string{"fact-build-new", "fact-build-old"}
	if !slices.Equal(fulltextIDs, wantFulltext) {
		t.Fatalf("FULLTEXT fact ids = %v, want %v", fulltextIDs, wantFulltext)
	}
}
