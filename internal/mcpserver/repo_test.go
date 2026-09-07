package mcpserver

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

type statusAttributionBackend struct {
	Backend
	actor memory.Actor
}

func (s *statusAttributionBackend) RepoStatus(ctx context.Context, opts localdolt.RepoStatusOptions) (localdolt.RepoStatusReport, error) {
	s.actor = ActorFromContext(ctx)
	return s.Backend.RepoStatus(ctx, opts)
}

func TestRepoStatusMCPModernAndLegacy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			ctx := context.Background()
			base, st := initializedToolStore(t)
			backend := &statusAttributionBackend{Backend: testElicitationBackend(base, st)}
			server := New("test")
			tools := RegisterTools(server, base, backend)
			client, session := connect(t, server, &mcp.Implementation{Name: "user", Version: "1"}, legacy)
			defer closeSessions(t, client, session)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			initial := callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{})
			if initial.Status != "no-remote" || !initial.LocalOnly || backend.actor.Name != "agent:user" || backend.actor.Raw != "user" {
				t.Fatalf("no-remote or attribution = %+v, %+v", initial, backend.actor)
			}
			callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{"local": true})
			callError(t, client, "repo_status", map[string]any{"local": true, "diff": true}, "cannot be combined")
			callError(t, client, "repo_status", map[string]any{"remote": "missing"}, "not configured")
			callError(t, client, "repo_status", map[string]any{"remote": "' OR 1=1"}, "invalid remote")
			path := filepath.ToSlash(t.TempDir())
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			remote := (&url.URL{Scheme: "file", Path: path}).String()
			if _, err := st.AddRemote(ctx, localdolt.Remote{Name: "origin", URL: remote}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Push(ctx, localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			cloneBase := t.TempDir()
			cfg := localdolt.Config{BaseDir: cloneBase, Actor: memory.UserActor.CommitAuthor()}
			if _, err := localdolt.Clone(ctx, cfg, remote, ""); err != nil {
				t.Fatal(err)
			}
			remoteWriter, err := localdolt.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := remoteWriter.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := remoteWriter.Close(); err != nil {
					t.Error(err)
				}
			}()
			current := callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{"diff": true})
			if current.Status != "current" || current.Diff == nil || len(current.Diff.Tables) != 0 {
				t.Fatalf("current = %+v", current)
			}
			if _, _, err := memory.New(remoteWriter, memory.UserActor).AddTask(ctx, "remote task", ""); err != nil {
				t.Fatal(err)
			}
			pushed, err := remoteWriter.Push(ctx, localdolt.TransferOptions{})
			if err != nil {
				t.Fatal(err)
			}
			behind := callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{"diff": true})
			if behind.Status != "behind" || behind.MainCommit != current.MainCommit || behind.RemoteCommit != pushed.MainCommit || len(behind.Diff.Tables) != 1 || *behind.Diff.Tables[0].Rows[0].To["title"] != "remote task" {
				t.Fatalf("behind = %+v", behind)
			}
			callOK(t, client, "task_add", map[string]any{"title": "local independent task"})
			before := testText(t, st, "SELECT DOLT_HASHOF('main')")
			diverged := callAs[localdolt.RepoStatusReport](t, client, "repo_status", map[string]any{})
			if diverged.Status != "diverged-mergeable" || diverged.MainCommit != before || diverged.Diff != nil || testText(t, st, "SELECT DOLT_HASHOF('main')") != before || testCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatalf("preview = %+v", diverged)
			}
			if _, err := st.Push(ctx, localdolt.TransferOptions{}); err == nil {
				t.Fatal("preview unexpectedly reconciled divergence")
			}
			listed, err := client.ListTools(ctx, nil)
			if err != nil || listed.TTLMs <= 0 {
				t.Fatalf("cached tools = %+v, %v", listed, err)
			}
		})
	}
}
