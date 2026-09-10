package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	reviewgate "github.com/kninetimmy/memdolt/internal/review"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestCommandVerifyAndListDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			before := storeCommitCount(t, base)
			if routed {
				serveStore(t, base)
			}
			marker := filepath.Join(base, "must-not-execute")
			for _, kind := range []string{"other", "lint", "run", "test", "build"} {
				line := "echo " + kind + " > " + marker
				result := decodeJSON[commandInfo](t, runMemdolt(t, "command", "verify", kind, line, "--exit-code", "0", "--actor", "Claude Code", "--dir", base, "--json"))
				if result.Commit == "" || result.Kind != kind || result.Cmdline == nil || *result.Cmdline != line || result.LastRunAt == nil || result.LastRunAt.IsZero() || result.LastExitCode == nil || *result.LastExitCode != 0 || result.SuccessCount == nil || *result.SuccessCount != 1 || result.FailCount == nil || *result.FailCount != 0 {
					t.Fatal(result)
				}
				humanInspect(t, base, func(st commandStore) {
					var author, email, message string
					humanRow(t, st, "SELECT committer, email, message FROM dolt_log WHERE commit_hash = ?", []any{result.Commit}, &author, &email, &message)
					if author != "agent:claude-code" || email != "agent-claude-code@memdolt.invalid" || message != "command record "+kind+" (exit 0)" {
						t.Fatalf("verify lost writer attribution: %s %s %s", author, email, message)
					}
				})
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("verify executed the supplied command line")
			}
			failed := decodeJSON[commandInfo](t, runMemdolt(t, "command", "verify", "test", "failing test", "--exit-code", "7", "--dir", base, "--json"))
			if failed.SuccessCount == nil || *failed.SuccessCount != 1 || failed.FailCount == nil || *failed.FailCount != 1 || failed.LastExitCode == nil || *failed.LastExitCode != 7 {
				t.Fatal(failed)
			}
			if human := runMemdolt(t, "command", "record", "test", "restored test", "--dir", base); !strings.Contains(human, "2 ok, 1 failed") {
				t.Fatal(human)
			}
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			humanInspect(t, base, func(st commandStore) {
				var commits int
				humanRow(t, st, "SELECT COUNT(*) FROM dolt_log", nil, &commits)
				if commits != before+7 {
					t.Fatalf("seven recordings added %d commits", commits-before)
				}
				if _, err := st.Commit(context.Background(), store.CommitRequest{Author: cliActor, Message: "tie command timestamps", NoText: true, Statements: []store.Statement{
					{SQL: "UPDATE commands SET last_run_at = ?", Args: []any{at}},
					{SQL: "UPDATE commands SET last_run_at = ? WHERE kind = ?", Args: []any{at.Add(time.Second), "other"}},
				}}); err != nil {
					t.Fatal(err)
				}
			})
			var snapshot [][]string
			humanInspect(t, base, func(st commandStore) { snapshot = repoStateSnapshot(t, st) })
			listed := decodeJSON[commandList](t, runMemdolt(t, "command", "list", "--dir", base, "--json")).Commands
			var kinds []string
			human := runMemdolt(t, "command", "list", "--dir", base)
			for _, command := range listed {
				kinds = append(kinds, command.Kind)
				got := decodeJSON[memory.Command](t, runMemdolt(t, "command", "get", command.Kind, "--dir", base, "--json"))
				if !reflect.DeepEqual(got, command) || !strings.Contains(human, commandLine(command)) {
					t.Fatalf("list/get differ or human output lost fields: %+v %+v %s", command, got, human)
				}
			}
			if !reflect.DeepEqual(kinds, []string{"other", "build", "test", "run", "lint"}) {
				t.Fatal(kinds)
			}
			writeTestFile(t, pathsFor(t, base).ConfigFile(), "[deny_list]\npatterns=['BLOCK_VERIFY']\n")
			if out, err := runMemdoltResult(t, "command", "verify", "build", "BLOCK_VERIFY", "--exit-code", "0", "--dir", base, "--json"); err == nil || out != "" || !strings.Contains(err.Error(), "deny-list") {
				t.Fatalf("verify bypassed deny-list: %q %v", out, err)
			}
			humanInspect(t, base, func(st commandStore) {
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(snapshot, after) {
					t.Fatal("list/get or refused verify changed memory")
				}
			})
		})
	}
}

func TestCommandVerifyLostOwnerReplyNeverReplays(t *testing.T) {
	base := initStore(t)
	before := storeCommitCount(t, base)
	st := openInitializedStore(t, base)
	t.Cleanup(func() { _ = st.Close() })
	handler, err := storeipc.NewHandler(storeipc.Config{Store: st,
		ReviewAccept: func(ctx context.Context, id, expected string, actor store.Actor, force bool) (localdolt.AcceptResult, error) {
			return reviewgate.AcceptExpected(ctx, st, pathsFor(t, base).ConfigFile(), id, expected, actor, force)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture could not read request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"operation":"record_command"`)) {
			calls.Add(1)
			handler.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		handler.ServeHTTP(w, r)
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	out, err := runMemdoltResult(t, "command", "verify", "build", "one observed run", "--exit-code", "0", "--dir", base, "--json")
	if err == nil || out != "" || calls.Load() != 1 || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "command get build") {
		t.Fatalf("lost verify reply = %q, %v; submissions=%d", out, err, calls.Load())
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	got := decodeJSON[memory.Command](t, runMemdolt(t, "command", "get", "build", "--dir", base, "--json"))
	if got.SuccessCount == nil || *got.SuccessCount != 1 || got.FailCount == nil || *got.FailCount != 0 || storeCommitCount(t, base) != before+1 {
		t.Fatal("unknown owner result was lost or replayed")
	}
}

func TestCommandReadsImportedAbsentObservationsDirectAndOwner(t *testing.T) {
	raw, err := os.ReadFile(repoFile("internal", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(interopTempDir(t), "legacy.json")
	writeTestFile(t, fixture, string(raw))
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			runMemdolt(t, "import", "--from-memhub", fixture, "--dir", base, "--json")
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			for _, args := range [][]string{{"command", "list"}, {"command", "get", "build"}} {
				out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...)
				if err != nil {
					t.Errorf("imported NULL command %v failed: %v", args, err)
					continue
				}
				var row map[string]any
				if args[1] == "list" {
					rows := decodeJSON[struct{ Commands []map[string]any }](t, out).Commands
					if len(rows) != 1 {
						t.Fatal(rows)
					}
					row = rows[0]
				} else {
					row = decodeJSON[map[string]any](t, out)
				}
				want := `{"kind":"build","cmdline":"go build ./...","lastExitCode":null,"lastRunAt":null,"successCount":9,"failCount":2}`
				if !reflect.DeepEqual(row, decodeJSON[map[string]any](t, want)) {
					encoded, _ := json.Marshal(row)
					t.Fatalf("invented/lost imported observations: %s", encoded)
				}
				human := runMemdolt(t, append(args, "--dir", base)...)
				if !strings.Contains(human, "last run unknown, exit unknown; 9 ok, 2 failed") {
					t.Fatal(human)
				}
			}
			humanInspect(t, base, func(st commandStore) {
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
					t.Fatal("command reads changed imported main/proposals/working data")
				}
			})
			verified := decodeJSON[commandInfo](t, runMemdolt(t, "command", "verify", "build", "go build ./...", "--exit-code", "0", "--dir", base, "--json"))
			if verified.LastExitCode == nil || *verified.LastExitCode != 0 || verified.LastRunAt == nil || verified.LastRunAt.IsZero() || verified.SuccessCount == nil || *verified.SuccessCount != 10 || verified.FailCount == nil || *verified.FailCount != 2 {
				t.Fatalf("verify changed known imported counter semantics: %s", runMemdolt(t, "command", "get", "build", "--dir", base, "--json"))
			}
		})
	}
}

func TestCommandReadsNativeNullableFieldsDirectAndOwner(t *testing.T) {
	source := initStore(t)
	humanInspect(t, source, func(st commandStore) {
		if _, err := st.Commit(context.Background(), store.CommitRequest{Author: cliActor, Message: "nullable native commands", NoText: true, Statements: []store.Statement{
			{SQL: "INSERT INTO commands (kind) VALUES ('build')"},
			{SQL: "INSERT INTO commands (kind, cmdline, last_exit_code, success_count) VALUES ('test', '', 0, 0)"},
			{SQL: "INSERT INTO commands (kind, last_run_at, fail_count) VALUES ('run', ?, 0)", Args: []any{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}},
		}}); err != nil {
			t.Fatal(err)
		}
	})
	fixture := filepath.Join(interopTempDir(t), "nullable-native.json")
	runMemdolt(t, "export", fixture, "--dir", source)
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			runMemdolt(t, "import", fixture, "--dir", base, "--json")
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			listed := decodeJSON[struct{ Commands []map[string]any }](t, runMemdolt(t, "command", "list", "--dir", base, "--json")).Commands
			want := `[
				{"kind":"run","cmdline":null,"lastExitCode":null,"lastRunAt":"2026-01-02T03:04:05Z","successCount":null,"failCount":0},
				{"kind":"build","cmdline":null,"lastExitCode":null,"lastRunAt":null,"successCount":null,"failCount":null},
				{"kind":"test","cmdline":"","lastExitCode":0,"lastRunAt":null,"successCount":0,"failCount":null}
			]`
			if !reflect.DeepEqual(listed, decodeJSON[[]map[string]any](t, want)) {
				t.Fatalf("native nullable command rows changed: %+v", listed)
			}
			for _, row := range listed {
				got := decodeJSON[map[string]any](t, runMemdolt(t, "command", "get", row["kind"].(string), "--dir", base, "--json"))
				if !reflect.DeepEqual(got, row) {
					t.Fatal("native nullable list/get differ")
				}
			}
			human := runMemdolt(t, "command", "list", "--dir", base)
			for _, line := range []string{
				"run: unknown (last run 2026-01-02T03:04:05Z, exit unknown; unknown ok, 0 failed)",
				"build: unknown (last run unknown, exit unknown; unknown ok, unknown failed)",
				"test:  (last run unknown, exit 0; 0 ok, unknown failed)",
			} {
				if !strings.Contains(human, line) {
					t.Fatalf("human output lost NULL/empty/zero distinctions: %s", human)
				}
			}
			humanInspect(t, base, func(st commandStore) {
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
					t.Fatal("native nullable reads changed main/working data")
				}
			})
			verified := decodeJSON[commandInfo](t, runMemdolt(t, "command", "verify", "build", "new observed build", "--exit-code", "1", "--dir", base, "--json"))
			if verified.Commit == "" || verified.Cmdline == nil || *verified.Cmdline != "new observed build" || verified.LastRunAt == nil || verified.LastExitCode == nil || *verified.LastExitCode != 1 || verified.SuccessCount != nil || verified.FailCount != nil {
				t.Fatal("verify fabricated previously unknown counters or lost the new observation")
			}
		})
	}
}
