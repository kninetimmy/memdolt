package localdolt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"

	"github.com/kninetimmy/memdolt/internal/store"
)

func TestRemoteSaveFailurePreservesNativeStateAndCache(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before save", true: "after save"}[persisted], func(t *testing.T) {
			s, _ := transferFixture(t)
			ctx := context.Background()
			path := filepath.Join(s.DataDir(), DatabaseName, ".dolt", "repo_state.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			head := transferMain(t, s)
			remote := Remote{Name: "new", URL: "https://example.invalid/fixture", User: "fixture"}
			call, err := s.remotes(ctx, &remote, func(state *env.RepoState, fs filesys.ReadWriteFS) error {
				if persisted {
					if err := state.Save(fs); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("synthetic save failure with rejected-secret")
			})
			if err == nil || strings.Contains(err.Error(), "rejected-secret") || !strings.Contains(err.Error(), "inspect `memdolt repo remote list`") {
				t.Fatalf("save failure = %+v, %v", call.added, err)
			}
			if (call.added.Name != "") != persisted {
				t.Fatalf("confirmation = %+v, persisted=%t", call.added, persisted)
			}
			var cached int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM dolt_remotes WHERE name = 'new'").Scan(&cached); err != nil || (cached == 1) != persisted {
				t.Fatalf("cache=%d, persisted=%t, error=%v", cached, persisted, err)
			}
			if got := transferMain(t, s); got != head {
				t.Fatal("save failure changed main")
			}
			after, err := os.ReadFile(path)
			if err != nil || (!persisted && !bytes.Equal(before, after)) {
				t.Fatalf("failed save changed native state: %v", err)
			}
			remotes, err := s.ListRemotes(ctx)
			if err != nil || len(remotes) != 1+cached {
				t.Fatalf("list after failure=%+v, %v", remotes, err)
			}
		})
	}
}

func TestRemoteSaveSerializesMemoryAndRemoteOperations(t *testing.T) {
	s, _ := transferFixture(t)
	ctx := context.Background()
	reached := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := s.remotes(ctx, &Remote{Name: "new", URL: "https://example.invalid/fixture"}, func(state *env.RepoState, fs filesys.ReadWriteFS) error {
			close(reached)
			<-resume
			return state.Save(fs)
		})
		finished <- err
	}()
	<-reached
	// Both the native read and a direct lane must wait until save/read-back
	// completes; otherwise one observes partial configuration or loses work.
	read := make(chan error, 1)
	write := make(chan error, 1)
	go func() { _, err := s.ListRemotes(ctx); read <- err }()
	go func() {
		_, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "concurrent note", NoText: true,
			Statements: []store.Statement{{SQL: "INSERT INTO tasks (id, title) VALUES ('concurrent', 'preserve')"}}})
		write <- err
	}()
	select {
	case err := <-read:
		close(resume)
		t.Fatalf("list passed incomplete save: %v", err)
	case err := <-write:
		close(resume)
		t.Fatalf("commit passed incomplete save: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(resume)
	for _, done := range []chan error{finished, read, write} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var title string
	if err := s.db.QueryRow("SELECT title FROM tasks WHERE id = 'concurrent'").Scan(&title); err != nil || title != "preserve" {
		t.Fatalf("concurrent write lost: %q, %v", title, err)
	}
	if _, err := s.db.Exec("CALL memdolt_remotes()"); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("ordinary SQL reached configuration capability: %v", err)
	}
}

func TestRemoteLegacyValidationIsReadOnlyAndCredentialFree(t *testing.T) {
	s, _ := transferFixture(t)
	ctx := context.Background()
	for _, fixture := range []struct{ raw, params string }{
		{"https://user:rejected-secret@host/db", "{}"},
		{"https://host/db?password=rejected-secret", "{}"},
		{"https://host/db#rejected-secret", "{}"},
		{"https://host/db", `{"password":"rejected-secret"}`},
		{"https://host/db", `{"__DOLT__grpc_username":"rejected-secret:invalid"}`},
	} {
		s = transferConfiguredRemote(t, s, fixture.raw, fixture.params)
		path := filepath.Join(s.DataDir(), DatabaseName, ".dolt", "repo_state.json")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := s.ListRemotes(ctx); err == nil || len(rows) != 0 || strings.Contains(err.Error(), "rejected-secret") {
			t.Fatalf("unsafe list = %+v, %v", rows, err)
		}
		if got, err := s.AddRemote(ctx, Remote{Name: "new", URL: "https://example.invalid/db"}); err == nil || got.Name != "" || strings.Contains(err.Error(), "rejected-secret") {
			t.Fatalf("unsafe add = %+v, %v", got, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("unsafe legacy configuration changed")
		}
	}
	// An optional stored username is safe to list without a password. Listing
	// the native Windows file spelling/spaces is covered by the initial fixture.
	s = transferConfiguredRemote(t, s, "https://example.invalid/db", `{"__DOLT__grpc_username":"fixture"}`)
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	if err := os.Unsetenv("DOLT_REMOTE_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListRemotes(ctx)
	want := []Remote{{Name: "origin", URL: "https://example.invalid/db", User: "fixture"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("password-free list=%+v, %v", got, err)
	}
}
