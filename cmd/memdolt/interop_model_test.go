//go:build golden

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// Like the golden gate, this explicit model check uses real verified artifacts.
// CLI index/recall use their shipped default ~/.memdolt/models cache.
func TestInteropRebuildAndProductionHybridRecall(t *testing.T) {
	base := initStore(t)
	raw, err := os.ReadFile(repoFile("internal", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(interopTempDir(t), "legacy.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	imported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", "--from-memhub", file, "--dir", base, "--json"))
	before := interopQueryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
	rebuilt := decodeJSON[embedding.RebuildResult](t, runMemdolt(t, "index", "rebuild", "--dir", base, "--json"))
	if rebuilt.Eligible != 6 || rebuilt.Created != 6 {
		t.Fatalf("real synthetic-import rebuild=%+v", rebuilt)
	}
	response := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "Build memory importer", "--mode", "hybrid", "--source-type", "task", "--provenance", "--dir", base, "--json"))
	id := ""
	for _, identity := range imported.IdentityMap {
		if identity.Table == "tasks" && identity.SourceID == "31" {
			id = identity.Value
		}
	}
	if len(response.Results) == 0 || response.Results[0].SourceID != id || response.Results[0].LastChanged == nil || response.Results[0].LastChanged.Hash != imported.MainCommit {
		t.Fatalf("production recall after import=%+v", response)
	}
	if after := interopQueryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); !reflect.DeepEqual(before, after) {
		t.Fatal("derived rebuild/recall changed durable import history")
	}
}
