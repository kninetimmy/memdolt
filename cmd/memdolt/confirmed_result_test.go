package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	opencodehost "github.com/kninetimmy/memdolt/internal/opencode"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

var errConfirmedCLI = errors.New("synthetic late CLI result failure")

type confirmedCLIStore struct{ *localCommandStore }

func (s confirmedCLIStore) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	result, err := s.Store.Commit(ctx, req)
	if err == nil {
		err = errConfirmedCLI
	}
	return result, err
}

func (s confirmedCLIStore) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	if strings.Contains(query, "FROM commands") {
		return nil, errors.New("synthetic command read-back failure")
	}
	return s.Store.Query(ctx, query, args...)
}

func (s confirmedCLIStore) DocAdd(ctx context.Context, opts localdolt.DocAddOptions) (localdolt.DocResult, error) {
	result, err := s.Store.DocAdd(ctx, opts)
	if err == nil {
		err = errConfirmedCLI
	}
	return result, err
}

func (s confirmedCLIStore) DocRemove(ctx context.Context, ident string, actor memory.Actor) (localdolt.DocResult, error) {
	result, err := s.Store.DocRemove(ctx, ident, actor)
	if err == nil {
		err = errConfirmedCLI
	}
	return result, err
}

func TestConfirmedOwnerCLILanesRetainLateResults(t *testing.T) {
	base := initStore(t)
	before := storeCommitCount(t, base)
	st := openInitializedStore(t, base)
	backend := confirmedCLIStore{&localCommandStore{Store: st, baseDir: base}}
	handler, err := storeipc.NewHandler(storeipc.Config{Store: backend, ReviewAccept: backend.ReviewAcceptExpected})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	file := filepath.Join(base, "confirmed.md")
	writeTestFile(t, file, "# Confirmed owner document\n")
	var taskID, docID string
	for _, args := range [][]string{
		{"task", "add", "confirmed owner task"}, {"task", "done"}, {"task", "block"},
		{"note", "add", "confirmed owner note"}, {"command", "record", "test", "go test ./..."},
		{"command", "verify", "test", "go test ./...", "--exit-code", "0"},
		{"state", "set", "confirmed owner state"}, {"arch", "set", "confirmed owner architecture"},
		{"doc", "add", file}, {"doc", "rm"},
	} {
		if args[0] == "task" && args[1] != "add" {
			args = append(args, taskID)
		}
		if args[0] == "doc" && args[1] == "rm" {
			args = append(args, docID)
		}
		out, err := runMemdoltResult(t, append(args, "--dir", base, "--json")...)
		result := decodeJSON[struct {
			ID       string              `json:"id"`
			Kind     string              `json:"kind"`
			Commit   string              `json:"commit"`
			Document *localdolt.Document `json:"document"`
		}](t, out)
		if err == nil || result.Commit == "" || !strings.Contains(err.Error(), result.Commit) || !strings.Contains(err.Error(), "inspect") {
			t.Fatalf("%v lost confirmed output: %s %v", args, out, err)
		}
		if args[0] == "task" && args[1] == "add" {
			taskID = result.ID
		}
		if args[0] == "doc" && args[1] == "add" {
			docID = result.Document.ID
		}
		if result.ID == "" && result.Kind == "" && result.Document == nil {
			t.Fatalf("%v lost row identity: %s", args, out)
		}
	}
	note, err := logOpenCodeWrapUpNote(context.Background(), base, "synthetic-session", "verified late note",
		func(context.Context, string) (opencodehost.SessionInfo, error) {
			return opencodehost.SessionInfo{SessionID: "synthetic-session"}, nil
		})
	if err == nil || note.Commit == "" || note.ID == "" || note.Actor != "agent:opencode" || note.SessionID != "synthetic-session" || !strings.Contains(err.Error(), note.Commit) {
		t.Fatalf("OpenCode lost confirmed result/provenance: %+v %v", note, err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if storeCommitCount(t, base) != before+11 {
		t.Fatal("owner path duplicated/lost a committed write")
	}
	tasks := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--status", "all", "--dir", base, "--json"))
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].ID != taskID || tasks.Tasks[0].Status != memory.StatusBlocked {
		t.Fatal("reopened owner task lost final status")
	}
	command := decodeJSON[memory.Command](t, runMemdolt(t, "command", "get", "test", "--dir", base, "--json"))
	if command.SuccessCount != 2 {
		t.Fatal("owner read-back failure lost command counter")
	}
}

func TestConfirmedDirectCLIOutputFailureNamesCommitAfterClose(t *testing.T) {
	for _, args := range [][]string{
		{"task", "add", "confirmed direct task"}, {"note", "add", "confirmed direct note"},
		{"command", "record", "build", "go build ./..."}, {"state", "set", "confirmed direct state"}, {"arch", "set", "confirmed direct architecture"},
		{"command", "verify", "build", "go build ./...", "--exit-code", "0"},
	} {
		t.Run(args[0], func(t *testing.T) {
			base := initStore(t)
			before := storeCommitCount(t, base)
			root := newRootCommand()
			root.SetOut(repoFailWriter{errConfirmedCLI})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(append(args, "--dir", base, "--json"))
			err := root.Execute()
			if !errors.Is(err, errConfirmedCLI) || !strings.Contains(err.Error(), "confirmed; inspect") {
				t.Fatalf("output error hid durable effect: %v", err)
			}
			// Reopening also proves the direct command released its store before
			// reporting the output failure.
			if storeCommitCount(t, base) != before+1 {
				t.Fatal("direct output failure lost committed history")
			}
		})
	}
}
