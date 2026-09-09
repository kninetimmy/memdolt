//go:build golden

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestGlobalProductionHybridRecallAndRebuild(t *testing.T) {
	cache, err := embedding.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	home, base := globalFixture(t)
	// Copy only existing model artifacts into the isolated home. The production
	// loader still verifies every hash before use; no live global store is read.
	if info, err := os.Stat(cache); err == nil && info.IsDir() {
		if err := os.CopyFS(filepath.Join(home, ".memdolt", "models"), os.DirFS(cache)); err != nil {
			t.Fatal(err)
		}
	}
	for _, global := range []bool{false, true} {
		args := []string{"fact", "add", "build.command", "Run go build ./... to compile the application", "--dir", base}
		if global {
			args = append(args, "--global")
		}
		runMemdolt(t, args...)
		index := []string{"index", "rebuild", "--dir", base, "--json"}
		if global {
			index = append(index, "--global")
		}
		result := decodeJSON[embedding.RebuildResult](t, runMemdolt(t, index...))
		if result.Created != 1 || result.Eligible != 1 {
			t.Fatal(result)
		}
	}
	result := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "How do I compile the application?", "--mode", "hybrid", "--source-type", "fact", "--provenance", "--dir", base, "--json"))
	if len(result.Results) != 2 || result.Results[0].Scope == result.Results[1].Scope {
		t.Fatalf("real-model scopes not merged: %+v", result)
	}
	for _, hit := range result.Results {
		if hit.RerankScore == nil || hit.LastChanged == nil || hit.SnapshotCommit == "" || hit.VectorScore <= 0 {
			t.Fatal(hit)
		}
	}
}

func TestGlobalReviewProductionModelAcceptanceAndProvenance(t *testing.T) {
	cache, err := embedding.DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	home, base := globalFixture(t)
	if info, err := os.Stat(cache); err == nil && info.IsDir() {
		if err := os.CopyFS(filepath.Join(home, ".memdolt", "models"), os.DirFS(cache)); err != nil {
			t.Fatal(err)
		}
	}
	// A same-kind destination row forces the shipped checksum-verified scorer
	// to run during acceptance. The unrelated subject should remain below 2.0.
	runMemdolt(t, "fact", "add", "fixture.botany", "A cactus needs infrequent watering and bright sunlight", "--global", "--dir", base)
	staged := stageGlobalCLI(t, base, false)
	accepted := decodeJSON[localdolt.AcceptResult](t, runMemdolt(t, "review", "accept", staged.ID, "--dir", base, "--json"))
	if accepted.Commit == "" || accepted.GlobalStageCommit == "" || accepted.Proposal.Commit != staged.Commit {
		t.Fatal(accepted)
	}
	runMemdolt(t, "index", "rebuild", "--global", "--dir", base)
	runMemdolt(t, "index", "rebuild", "--dir", base)
	recalled := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "How do I compile the application?", "--mode", "hybrid", "--source-type", "fact", "--provenance", "--dir", base, "--json"))
	for _, hit := range recalled.Results {
		if hit.SourceID == staged.RowID {
			if hit.Scope != "global" || hit.SnapshotCommit != accepted.Commit || hit.LastChanged == nil || hit.RerankScore == nil || hit.VectorScore <= 0 {
				t.Fatal(hit)
			}
			if hit.LastChanged.Hash != accepted.GlobalStageCommit || hit.LastChanged.Author != cliStagingActor.Name {
				t.Fatalf("global recall lost native staging provenance: %+v", hit.LastChanged)
			}
			return
		}
	}
	t.Fatalf("accepted global fact missing from actual-model recall: %+v", recalled)
}
