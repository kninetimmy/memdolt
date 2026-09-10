package main

import (
	"bytes"
	"context"
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
				if result.Commit == "" || result.Kind != kind || result.Cmdline != line || result.LastRunAt.IsZero() || result.LastExitCode != 0 || result.SuccessCount != 1 || result.FailCount != 0 {
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
			if failed.SuccessCount != 1 || failed.FailCount != 1 || failed.LastExitCode != 7 {
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
				if got != command || !strings.Contains(human, commandLine(command)) {
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
	if got.SuccessCount != 1 || got.FailCount != 0 || storeCommitCount(t, base) != before+1 {
		t.Fatal("unknown owner result was lost or replayed")
	}
}
