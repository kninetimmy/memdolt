package main

import (
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/retrieval"
)

func TestRecallCLIExposesResponseShapeInJSONAndHumanOutput(t *testing.T) {
	base := initStore(t)
	task := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "Ship recall surface", "--notes", "exercise production lexical retrieval", "--dir", base, "--json"))

	response := decodeJSON[retrieval.Response](t, runMemdolt(t,
		"recall", "production lexical retrieval", "--mode", "fts", "--source-type", "task",
		"--provenance", "--dir", base, "--json"))
	if response.Query != "production lexical retrieval" || response.Mode != retrieval.ModeFTS ||
		response.CandidateCount != 1 || response.ReturnedCount != 1 || response.AvailableDocs != 0 ||
		len(response.Results) != 1 || response.Results[0].SourceID != task.ID ||
		response.Results[0].LastChanged == nil || response.Results[0].LastChanged.Hash == "" {
		t.Fatalf("recall JSON response = %+v", response)
	}
	if response.Results == nil || response.Warnings == nil {
		t.Fatalf("recall JSON arrays must not be null: %+v", response)
	}

	human := runMemdolt(t, "recall", "production lexical retrieval", "--mode", "fts",
		"--source-type", "task", "--provenance", "--dir", base)
	for _, want := range []string{"Query:", "Candidates: 1", "Returned: 1", "Available docs: 0", "[task:" + task.ID + "]", "last_changed="} {
		if !strings.Contains(human, want) {
			t.Errorf("human recall output %q does not contain %q", human, want)
		}
	}

	empty := decodeJSON[retrieval.Response](t, runMemdolt(t,
		"recall", "zzzzunmatchedtoken", "--mode", "fts", "--dir", base, "--json"))
	if empty.ReturnedCount != 0 || len(empty.Results) != 0 {
		t.Fatalf("empty recall returned %+v", empty.Results)
	}
	doctor := runDoctorJSON(t, base)
	check := findCheck(t, doctor, "empty-recall-rate")
	if check.Status != statusOK || !strings.Contains(check.Detail, "1 empty of 3 recall calls") || !strings.Contains(check.Detail, "33.3%") {
		t.Fatalf("empty recall doctor check = %+v", check)
	}
}
