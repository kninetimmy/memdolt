package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/mcpserver"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func renderNoteSession(t *testing.T) (string, *mcp.ClientSession, func()) {
	t.Helper()
	base := initStore(t)
	server := mcpserver.New("test")
	clientPipe, serverPipe := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, base, server, &mcp.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "render-notes", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.IOTransport{Reader: clientPipe, Writer: clientPipe}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		_ = session.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not finish")
		}
	}
	t.Cleanup(stop)
	return base, session, stop
}

func queueRenderNote(t *testing.T, session *mcp.ClientSession) memory.Note {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "log_session_note", Arguments: map[string]any{"text": "retain this note after a later render failure"},
	})
	if err != nil || result.IsError {
		t.Fatalf("queue note: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSON[struct{ Note memory.Note }](t, string(raw)).Note
}

func TestRenderRetainsFlushedNotesOnProductionOwnerRefusal(t *testing.T) {
	for _, route := range []string{"MCP", "CLI"} {
		t.Run(route, func(t *testing.T) {
			base, session, stop := renderNoteSession(t)
			writeTestFile(t, pathsFor(t, base).ConfigFile(), "[render]\noutput_dir='../outside'\n")
			note := queueRenderNote(t, session)
			call := func() (string, error) {
				if route == "CLI" {
					return runMemdoltResult(t, "render", "--dir", base, "--json")
				}
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "render", Arguments: map[string]any{}})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				if !result.IsError {
					return string(raw), nil
				}
				content, err := json.Marshal(result.Content)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw), errors.New(string(content))
			}
			out, err := call()
			result := decodeJSON[struct {
				NoteCommits  map[string]string `json:"noteCommits"`
				SourceCommit string            `json:"sourceCommit"`
				WrittenFiles []string          `json:"writtenFiles"`
				Error        string            `json:"error"`
			}](t, out)
			head := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", base, "--json")).MainCommit
			if err == nil || len(result.NoteCommits) != 1 || result.NoteCommits[note.ID] != head || result.SourceCommit != "" || len(result.WrittenFiles) != 0 {
				t.Fatalf("%s discarded notes committed before render refusal: %s; main=%s note=%s error=%v", route, out, head, note.ID, err)
			}
			for _, text := range []string{note.ID, head, "memdolt note list", "Dolt history"} {
				if !strings.Contains(result.Error, text) || !strings.Contains(err.Error(), text) {
					t.Errorf("%s omitted note inspection evidence %q: %s / %v", route, text, result.Error, err)
				}
			}
			if _, err := os.Stat(filepath.Join(base, ".memdolt", "rendered", "PROJECT.md")); !os.IsNotExist(err) {
				t.Fatal("refused render published a file")
			}
			again, err := call()
			if err == nil || len(decodeJSON[struct{ NoteCommits map[string]string }](t, again).NoteCommits) != 0 {
				t.Fatalf("repeat render claimed new note commits: %s %v", again, err)
			}
			stop()
			notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json"))
			if len(notes.Notes) != 1 || notes.Notes[0].ID != note.ID || commitLog(t, base)[head].message != "note batch (1)" {
				t.Fatal("reopened note/history did not match confirmed effect")
			}
		})
	}
}

func TestRenderRetainsFlushedNotesOnCLICloseAndOutputFailure(t *testing.T) {
	for _, failure := range []string{"close", "output"} {
		t.Run(failure, func(t *testing.T) {
			base, session, stop := renderNoteSession(t)
			note := queueRenderNote(t, session)
			st, err := openCommandStore(context.Background(), base, cliActor)
			if err != nil {
				t.Fatal(err)
			}
			cmd := newRenderCommand()
			cmd.SetContext(context.Background())
			var output bytes.Buffer
			cmd.SetOut(&output)
			jsonOutput = true
			if failure == "close" {
				st = renderCloseFailure{st}
			} else {
				cmd.SetOut(failingRenderOutput{})
			}
			err = runRender(cmd, st)
			head := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", base, "--json")).MainCommit
			if err == nil {
				t.Fatal("expected reporting failure")
			}
			for _, text := range []string{note.ID, head, "memdolt note list", "Dolt history"} {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("%s omitted note inspection evidence %q: %v", failure, text, err)
				}
			}
			if failure == "close" {
				result := decodeJSON[struct{ NoteCommits map[string]string }](t, output.String())
				if result.NoteCommits[note.ID] != head {
					t.Fatalf("close error discarded structured note effects: %s", &output)
				}
			}
			stop()
			notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json"))
			if len(notes.Notes) != 1 || notes.Notes[0].ID != note.ID || commitLog(t, base)[head].message != "note batch (1)" {
				t.Fatal("reporting failure lost committed note/history")
			}
		})
	}
}
