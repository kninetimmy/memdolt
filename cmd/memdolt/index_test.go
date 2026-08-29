package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/layout"
)

func TestIndexStatusReportsMissingRowsAndRebuildRemedyWithoutWritingDolt(t *testing.T) {
	base := scratchDir(t)
	runMemdolt(t, "init", "--dir", base)
	task := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "Build the embedding index", "--notes", "Keep vectors outside Dolt", "--dir", base, "--json"))
	beforeHistory := queryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
	beforeTask := queryStrings(t, base, "SELECT CONCAT(title, '|', COALESCE(notes, '')) FROM tasks WHERE id = ?", task.ID)

	report := decodeJSON[embedding.StatusReport](t, runMemdolt(t, "index", "status", "--dir", base, "--json"))
	if report.Eligible != 1 || report.Missing != 1 || report.Current != 0 || !report.NeedsRebuild {
		t.Fatalf("index status = %+v, want one missing task embedding", report)
	}
	if report.Remedy != embedding.RebuildRemedy {
		t.Fatalf("index status remedy = %q, want %q", report.Remedy, embedding.RebuildRemedy)
	}
	if human := runMemdolt(t, "index", "status", "--dir", base); !strings.Contains(human, "remedy: "+embedding.RebuildRemedy) {
		t.Fatalf("human index status omitted the rebuild remedy: %q", human)
	}
	if got := queryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); !reflect.DeepEqual(got, beforeHistory) {
		t.Fatalf("index status changed Dolt history: before %v, after %v", beforeHistory, got)
	}
	if got := queryStrings(t, base, "SELECT CONCAT(title, '|', COALESCE(notes, '')) FROM tasks WHERE id = ?", task.ID); !reflect.DeepEqual(got, beforeTask) {
		t.Fatalf("index status changed its task source: before %v, after %v", beforeTask, got)
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if _, err := os.Stat(paths.EmbeddingsFile()); !os.IsNotExist(err) {
		t.Fatalf("read-only index status created %s or returned an unexpected stat error: %v", paths.EmbeddingsFile(), err)
	}
}

func TestIndexRebuildCreatesAnEmptyDerivedStoreWithoutWritingDolt(t *testing.T) {
	base := scratchDir(t)
	runMemdolt(t, "init", "--dir", base)
	beforeHistory := queryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")

	result := decodeJSON[embedding.RebuildResult](t, runMemdolt(t, "index", "rebuild", "--dir", base, "--json"))
	if result.Eligible != 0 || result.Created != 0 || result.Refreshed != 0 || result.Unchanged != 0 || result.Removed != 0 {
		t.Fatalf("empty index rebuild = %+v, want no source-row changes", result)
	}
	if got := queryStrings(t, base, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); !reflect.DeepEqual(got, beforeHistory) {
		t.Fatalf("index rebuild changed Dolt history: before %v, after %v", beforeHistory, got)
	}
	for table, query := range map[string]string{
		"facts":      "SELECT id FROM facts",
		"decisions":  "SELECT id FROM decisions",
		"tasks":      "SELECT id FROM tasks",
		"doc_chunks": "SELECT id FROM doc_chunks",
	} {
		if got := queryStrings(t, base, query); len(got) != 0 {
			t.Errorf("index rebuild added %s source rows: %v", table, got)
		}
	}
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if info, err := os.Stat(paths.EmbeddingsFile()); err != nil {
		t.Fatalf("embedding side-store was not created: %v", err)
	} else if info.IsDir() || !strings.HasSuffix(info.Name(), ".sqlite") {
		t.Fatalf("embedding side-store info = %+v, want a SQLite file", info)
	}
}
