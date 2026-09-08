package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

type confirmedBackend struct {
	Backend
	attempts map[string]int
}

type canceledOwnerBackend struct {
	Backend
	owner         *storeipc.OwnerStore
	cancelRequest chan context.CancelFunc
}

func (s canceledOwnerBackend) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cancelRequest <- cancel
	return s.owner.Commit(ctx, req)
}

func TestConfirmedCanceledOwnerNoteIsInspectableWithoutReplay(t *testing.T) {
	base, st := initializedToolStore(t)
	backend := testElicitationBackend(base, st)
	routes, err := storeipc.NewHandler(storeipc.Config{Store: backend, ReviewAccept: backend.ReviewAcceptExpected})
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest := make(chan context.CancelFunc, 1)
	var attempts atomic.Int32
	endpoint, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != storeipc.CommitPath {
			routes.ServeHTTP(w, r)
			return
		}
		// The real native store commits, but its authenticated reply cannot
		// reach the caller because the request is canceled at this boundary.
		attempts.Add(1)
		routes.ServeHTTP(httptest.NewRecorder(), r)
		(<-cancelRequest)()
		<-r.Context().Done()
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	owner, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatal(err)
	}
	tools := RegisterTools(New("test"), base, canceledOwnerBackend{backend, owner, cancelRequest})
	note, err := tools.queueNote(context.Background(), memory.UserActor, "canceled reply after native note commit")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := tools.Render(context.Background())
		if !errors.Is(err, store.ErrCommitUnknown) || result.Status != "notes-failed" || !strings.Contains(err.Error(), note.ID) {
			t.Fatalf("unknown note outcome lost or replayed: %+v %v", result, err)
		}
	}
	if len(tools.groups) != 1 || tools.groups[0].unknown == nil {
		t.Fatal("unknown group was not retained for inspection")
	}
	if err := tools.Close(); !errors.Is(err, store.ErrCommitUnknown) || len(tools.groups) != 0 {
		t.Fatalf("close did not report/discard inspect-only group: %v", err)
	}
	if attempts.Load() != 1 || testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'") != 1 || testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (1)'") != 1 {
		t.Fatal("unknown reply caused automatic replay")
	}
}

func (s *confirmedBackend) Commit(ctx context.Context, req store.CommitRequest) (store.CommitResult, error) {
	s.attempts[req.Author.Name]++
	if req.Author.Name == "agent:retry" && s.attempts[req.Author.Name] == 1 {
		return store.CommitResult{}, errors.New("synthetic uncommitted failure")
	}
	result, err := s.Backend.Commit(ctx, req)
	if err == nil && req.Author.Name == "agent:late" {
		err = errors.New("synthetic late finalization failure")
	}
	return result, err
}

func TestConfirmedSessionGroupsNeverReplay(t *testing.T) {
	for _, trigger := range []string{"render", "timer-render", "timer-shutdown"} {
		t.Run(trigger, func(t *testing.T) {
			base, st := initializedToolStore(t)
			backend := &confirmedBackend{Backend: testElicitationBackend(base, st), attempts: map[string]int{}}
			tools := registerTools(New("test"), base, backend, time.Hour)
			for _, name := range []string{"retry", "late", "ok"} {
				actor, err := memory.NormalizeActor(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tools.queueNote(context.Background(), actor, "queued "+name); err != nil {
					t.Fatal(err)
				}
			}
			if trigger == "render" {
				result, err := tools.Render(context.Background())
				if err == nil || result.Status != "notes-failed" || !strings.Contains(err.Error(), "confirmed") {
					t.Fatalf("late flush published or lost its outcome: %+v %v", result, err)
				}
			} else {
				tools.mu.Lock()
				tools.timer.Stop()
				tools.interval = time.Millisecond
				tools.startTimer()
				tools.mu.Unlock()
				deadline := time.Now().Add(10 * time.Second)
				for {
					tools.mu.Lock()
					finished := tools.flushErr != nil
					tools.mu.Unlock()
					if finished {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("timer flush did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			}
			tools.mu.Lock()
			pending := len(tools.groups) == 1 && tools.groups[0].actor.Name == "agent:retry"
			tools.mu.Unlock()
			if !pending || testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'") != 2 {
				t.Fatal("first flush failed to attempt every group or retained confirmed rows")
			}
			output := filepath.Join(base, ".memdolt", "rendered", "PROJECT.md")
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("failed flush published render output")
			}
			if trigger == "timer-render" {
				// The timer's failure must be visible to the explicit renderer even
				// if its remaining uncommitted group now succeeds.
				if result, err := tools.Render(context.Background()); err == nil || len(result.WrittenFiles) != 0 {
					t.Fatalf("timer failure hidden by render: %+v %v", result, err)
				}
			}
			if trigger != "timer-shutdown" {
				result, err := tools.Render(context.Background())
				if err != nil || result.Status != "written" {
					t.Fatalf("explicit retry could not render committed notes: %+v %v", result, err)
				}
				data, err := os.ReadFile(output)
				if err != nil || !strings.Contains(string(data), "queued late") || !strings.Contains(string(data), "queued retry") {
					t.Fatalf("render missed committed notes: %s %v", data, err)
				}
			}
			for range 2 {
				if err := tools.Close(); err == nil || !strings.Contains(err.Error(), "late finalization") || !strings.Contains(err.Error(), "uncommitted failure") {
					t.Fatalf("shutdown hid earlier failures: %v", err)
				}
			}
			if backend.attempts["agent:late"] != 1 || backend.attempts["agent:ok"] != 1 || backend.attempts["agent:retry"] != 2 {
				t.Fatalf("confirmed group replayed or uncommitted group lost retry: %v", backend.attempts)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: memory.UserActor.CommitAuthor()})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Open(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if testCount(t, st, "SELECT COUNT(*) FROM session_notes AS OF 'main'") != 3 || testCount(t, st, "SELECT COUNT(*) FROM dolt_log WHERE message = 'note batch (1)'") != 3 {
				t.Fatal("reopened notes or history are not exactly once")
			}
		})
	}
}

func TestConfirmedMCPWritesRetainStructuredEffects(t *testing.T) {
	base, st := initializedToolStore(t)
	backend := &confirmedBackend{Backend: testElicitationBackend(base, st), attempts: map[string]int{}}
	server := New("test")
	tools := RegisterTools(server, base, backend)
	client, session := connect(t, server, &mcp.Implementation{Name: "late", Version: "1"}, false)
	var id string
	for _, name := range []string{"task_add", "task_done", "record_command"} {
		args := map[string]any{"title": "confirmed MCP task"}
		switch name {
		case "task_done":
			args = map[string]any{"id": id}
		case "record_command":
			args = map[string]any{"kind": "test", "cmdline": "go test ./...", "exit_code": 0}
		}
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || !result.IsError || result.StructuredContent == nil {
			t.Fatalf("%s discarded confirmed effects: %+v %v", name, result, err)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var wire taskWriteOutput
		if err := json.Unmarshal(raw, &wire); err != nil || wire.Commit == "" || wire.Actor.Name != "agent:late" {
			t.Fatalf("%s result lost commit/actor: %s %v", name, raw, err)
		}
		if name == "task_add" {
			id = wire.Task.ID
		}
	}
	closeSessions(t, client, session)
	if err := tools.Close(); err != nil {
		t.Fatal(err)
	}
}
