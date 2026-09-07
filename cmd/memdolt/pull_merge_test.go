package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestPullCLIConflictResolutionDirectAndLiveOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			remote := configureCLITransferRemote(t, a)
			task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", "shared task", "--dir", a, "--json"))
			runMemdolt(t, "push", "--dir", a)
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			for i, base := range []string{a, b} {
				db := openRepoFixtureDB(t, base)
				if _, err := db.Exec("UPDATE tasks SET notes=?, updated_at=? WHERE id=?", []string{"theirs", "ours"}[i], []string{"2026-01-03 00:00:00", "2026-01-02 00:00:00"}[i], task.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("CALL DOLT_COMMIT('-A', '-m', 'synthetic CLI conflict')"); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			runMemdolt(t, "push", "--dir", a)
			stop := func() {}
			if routed {
				stop = serveTransferProcess(t, b)
			}
			before := runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json")
			stdout, err := runMemdoltResult(t, "pull", "--dir", b, "--json")
			if err == nil || !strings.Contains(err.Error(), "--resolve") {
				t.Fatalf("noninteractive conflict = %q, %v", stdout, err)
			}
			shown := decodeJSON[localdolt.TransferResult](t, stdout)
			if shown.Status != "conflicted" || len(shown.Conflicts) != 1 || shown.Conflicts[0].Rows[0].TheirsBlame.Message != "synthetic CLI conflict" {
				t.Fatalf("CLI conflict = %+v", shown)
			}
			if runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json") != before {
				t.Fatal("CLI conflict changed local state")
			}
			resolution := localdolt.PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: []localdolt.PullChoice{{Conflict: shown.Conflicts[0].ID, Take: "manual"}}}
			resolution.Choices[0].Row = shown.Conflicts[0].Rows[0].Ours
			manual := "reviewed manual CLI value"
			resolution.Choices[0].Row["notes"] = &manual
			data, err := json.Marshal(resolution)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(b, "resolution.json")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			malformed := filepath.Join(b, "malformed.json")
			if err := os.WriteFile(malformed, []byte(`{"localCommit":"first","localCommit":"second"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runMemdoltErr(t, "pull", "--dir", b, "--resolve", malformed); !strings.Contains(err, "duplicate") {
				t.Fatal(err)
			}
			resolved := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--resolve", file, "--json"))
			if !resolved.Changed || resolved.LocalCommit != shown.LocalCommit || resolved.RemoteCommit != shown.RemoteCommit {
				t.Fatalf("CLI merge = %+v", resolved)
			}
			stop()
			listed := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", b, "--json"))
			if len(listed.Tasks) != 1 || listed.Tasks[0].ID != task.ID || listed.Tasks[0].Notes != manual {
				t.Fatalf("ordinary reopened CLI read = %+v", listed)
			}
			if string(mustPullFile(t, file)) != string(data) {
				t.Fatal("pull modified resolution source file")
			}
			if err := runMemdoltErr(t, "pull", "--dir", b, "--resolve", file); !strings.Contains(err, "changed since") {
				t.Fatal(err)
			}
		})
	}
}

func mustPullFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPullCLIIndependentMergeAndStdinResolutionParsing(t *testing.T) {
	a := initStore(t)
	remote := configureCLITransferRemote(t, a)
	runMemdolt(t, "push", "--dir", a)
	b := scratchDir(t)
	runMemdolt(t, "clone", remote, "--dir", b)
	runMemdolt(t, "task", "add", "local task", "--dir", b)
	runMemdolt(t, "task", "add", "remote task", "--dir", a)
	runMemdolt(t, "push", "--dir", a)
	result := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--json"))
	if !result.Changed || result.MainCommit == result.RemoteCommit || len(decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", b, "--json")).Tasks) != 2 {
		t.Fatalf("independent CLI merge = %+v", result)
	}
	runMemdolt(t, "push", "--dir", b)
	c := scratchDir(t)
	runMemdolt(t, "clone", remote, "--dir", c)
	if len(decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", c, "--json")).Tasks) != 2 {
		t.Fatal("ordinary fresh clone lost merged rows")
	}
	root := newRootCommand()
	root.SetIn(strings.NewReader(`{"localCommit":"not a hash","remoteCommit":"not a hash","choices":[]}`))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"pull", "--dir", b, "--resolve", "-"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "exact displayed") {
		t.Fatalf("stdin malformed hashes = %v", err)
	}
}
