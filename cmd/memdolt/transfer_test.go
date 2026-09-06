package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func configureCLITransferRemote(t *testing.T, base string) string {
	t.Helper()
	path := filepath.ToSlash(scratchDir(t))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	remote := (&url.URL{Scheme: "file", Path: path}).String()
	dsn := "file://" + filepath.ToSlash(filepath.Join(base, ".memdolt", "dolt")) + "?commitname=user&commitemail=user%40memdolt.invalid&database=memory"
	db, err := sql.Open("dolt", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CALL DOLT_REMOTE('add', 'origin', ?)", remote); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestTransferCLIProductionDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owner"}[routed], func(t *testing.T) {
			a := initStore(t)
			remote := configureCLITransferRemote(t, a)
			if routed {
				serveStore(t, a)
			}
			runMemdolt(t, "note", "add", "first transfer note", "--dir", a, "--actor", "cli")
			first := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
			if !first.Changed || first.Remote != "origin" || first.Operation != "push" || first.RemoteCommit != first.LocalCommit {
				t.Fatalf("push = %+v", first)
			}
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			if routed {
				serveStore(t, b)
			}
			for _, lane := range []string{"embeddings.sqlite", "code_index.sqlite", "rendered/PROJECT.md"} {
				file := filepath.Join(b, ".memdolt", filepath.FromSlash(lane))
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("local artifact"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runMemdolt(t, "task", "add", "published task", "--dir", a)
			pushed := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "origin", "--dir", a, "--json"))
			pulled := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "origin", "--dir", b, "--json"))
			if !pulled.Changed || pulled.LocalCommit != first.LocalCommit || pulled.MainCommit != pushed.RemoteCommit {
				t.Fatalf("pull = %+v, push = %+v", pulled, pushed)
			}
			current := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--json"))
			if current.Changed || current.Status != "current" {
				t.Fatalf("repeat = %+v", current)
			}
			if text := runMemdolt(t, "push", "--dir", b); !strings.Contains(text, "push origin: current") || !strings.Contains(text, pushed.RemoteCommit) {
				t.Fatalf("human push = %s", text)
			}
			if got := runMemdolt(t, "task", "list", "--dir", b); !strings.Contains(got, "published task") {
				t.Fatal("reopened CLI task missing")
			}
			if got := runMemdolt(t, "note", "list", "--dir", b); !strings.Contains(got, "first transfer note") {
				t.Fatal("reopened CLI note missing")
			}
			for _, lane := range []string{"embeddings.sqlite", "code_index.sqlite", "rendered/PROJECT.md"} {
				data, err := os.ReadFile(filepath.Join(b, ".memdolt", filepath.FromSlash(lane)))
				if err != nil || string(data) != "local artifact" {
					t.Fatalf("changed derived %s: %v", lane, err)
				}
			}
		})
	}
}

func TestTransferCLIPreflightHelpAndOutputFailures(t *testing.T) {
	for _, operation := range []string{"push", "pull"} {
		base := scratchDir(t)
		before := repoFiles(t, base)
		if err := runMemdoltErr(t, operation, "--dir", base, "--json"); !strings.Contains(err, "memdolt init") {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(repoFiles(t, base), before) {
			t.Fatal("missing-store transfer created files")
		}
		for _, flag := range []string{"--force", "--all", "--prune"} {
			if err := runMemdoltErr(t, operation, flag, "--dir", base); !strings.Contains(err, "unknown flag") {
				t.Fatal(err)
			}
		}
		initialized := initStore(t)
		if err := runMemdoltErr(t, operation, "--dir", initialized); !strings.Contains(err, "no remote") || !strings.Contains(err, "dolt remote add") {
			t.Fatal(err)
		}
		if err := runMemdoltErr(t, operation, "--dir", initialized, "--user", ""); !strings.Contains(err, "must not be empty") {
			t.Fatal(err)
		}
		help := runMemdolt(t, operation, "--help")
		for _, want := range []string{"DOLT_REMOTE_PASSWORD", "owner", "--dir", "--json", "--user", "inspect"} {
			if !strings.Contains(help, want) {
				t.Fatalf("help lacks %q", want)
			}
		}
	}
	base := initStore(t)
	configureCLITransferRemote(t, base)
	for _, failure := range []string{"close", "human", "json"} {
		t.Run(failure, func(t *testing.T) {
			fixtureError := errors.New("synthetic transfer finalization failure")
			st := &repoFaultStore{commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base}}
			out := &bytes.Buffer{}
			cmd := &cobra.Command{Use: "push"}
			cmd.SetContext(context.Background())
			cmd.SetOut(out)
			jsonOutput = failure == "json"
			if failure == "close" {
				st.closeErr = fixtureError
			} else {
				cmd.SetOut(repoFailWriter{fixtureError})
			}
			err := runTransfer(cmd, st, "push", localdolt.TransferOptions{})
			if !errors.Is(err, fixtureError) || !strings.Contains(err.Error(), "confirmed") || !st.closed || out.Len() != 0 {
				t.Fatalf("finalization = %v, closed=%t, stdout=%s", err, st.closed, out)
			}
		})
	}
}
