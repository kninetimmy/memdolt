package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
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
		if err := runMemdoltErr(t, operation, "--dir", initialized); !strings.Contains(err, "no remote") || !strings.Contains(err, "memdolt repo remote add") {
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

func TestTransferCLIPullPreservesTagsAndStdout(t *testing.T) {
	for _, routed := range []bool{false, true} {
		for _, denied := range []bool{false, true} {
			t.Run(fmt.Sprintf("owner=%t/denied=%t", routed, denied), func(t *testing.T) {
				a := initStore(t)
				remote := configureCLITransferRemote(t, a)
				initial := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
				b := scratchDir(t)
				runMemdolt(t, "clone", remote, "--dir", b)
				localDB := openRepoFixtureDB(t, b)
				for _, name := range []string{"keep", "local-only"} {
					if _, err := localDB.Exec("CALL DOLT_TAG('--author', 'Local Fixture <local@example.invalid>', '-m', 'preserve local metadata', ?)", name); err != nil {
						t.Fatal(err)
					}
				}
				before := transferTags(t, localDB)
				if len(before) != 2 || before[0][1] != initial.MainCommit {
					t.Fatalf("local tag fixture = %v", before)
				}
				if err := localDB.Close(); err != nil {
					t.Fatal(err)
				}

				runMemdolt(t, "task", "add", "incoming tag fixture", "--dir", a)
				pushed := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
				// Native Dolt publishes tags only to construct the hostile remote;
				// memdolt's main-only push intentionally cannot publish these refs.
				sourceDB := openRepoFixtureDB(t, a)
				for _, name := range []string{"keep", "remote-only"} {
					if _, err := sourceDB.Exec("CALL DOLT_TAG('--author', 'Remote Fixture <remote@example.invalid>', '-m', 'remote replacement metadata', ?)", name); err != nil {
						t.Fatal(err)
					}
					if _, err := sourceDB.Exec("CALL DOLT_PUSH('origin', ?)", "refs/tags/"+name); err != nil {
						t.Fatal(err)
					}
				}
				if err := sourceDB.Close(); err != nil {
					t.Fatal(err)
				}
				if denied {
					if err := os.WriteFile(pathsFor(t, b).ConfigFile(), []byte("[deny_list]\npatterns = ['incoming tag fixture']\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				var stopOwner func()
				if routed {
					stopOwner = serveTransferProcess(t, b)
				}
				// A subprocess captures actual stdout, including Dolt's global
				// writer, which a Cobra-only buffer would miss.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTransferPullHelperProcess$")
				cmd.Env = append(os.Environ(), "MEMDOLT_PULL_HELPER_DIR="+b)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if stopOwner != nil {
					stopOwner()
				}
				wantMain := pushed.MainCommit
				if denied {
					wantMain = initial.MainCommit
					if err == nil || !strings.Contains(stderr.String(), "refuse promotion") || !strings.Contains(stderr.String(), "deny-list") {
						t.Errorf("pull refusal = %v, stderr %q", err, &stderr)
					}
					if stdout.Len() != 0 {
						t.Errorf("refused pull stdout = %q, want empty", &stdout)
					}
				} else {
					if err != nil {
						t.Fatalf("pull = %v, stderr %q", err, &stderr)
					}
					if !strings.HasPrefix(stdout.String(), "{") || strings.Count(stdout.String(), "\n") != 1 {
						t.Errorf("pull stdout = %q, want only one JSON line", &stdout)
					}
					got := decodeJSON[localdolt.TransferResult](t, stdout.String())
					if !got.Changed || got.Status != "changed" || got.MainCommit != wantMain || got.RemoteCommit != wantMain {
						t.Errorf("pull = %+v, want main %s", got, wantMain)
					}
				}
				status := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json"))
				if status.MainCommit != wantMain || !status.Clean {
					t.Errorf("reopened status = %+v, want clean main %s", status, wantMain)
				}
				afterDB := openRepoFixtureDB(t, b)
				if after := transferTags(t, afterDB); !reflect.DeepEqual(after, before) {
					t.Errorf("pull changed local tag refs/metadata: before %v, after %v", before, after)
				}
				if err := afterDB.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func transferTags(t *testing.T, db *sql.DB) [][]string {
	t.Helper()
	rows, err := db.Query("SELECT tag_name, tag_hash, tagger, email, date, message FROM dolt_tags ORDER BY tag_name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tags [][]string
	for rows.Next() {
		tag := make([]string, 6)
		if err := rows.Scan(&tag[0], &tag[1], &tag[2], &tag[3], &tag[4], &tag[5]); err != nil {
			t.Fatal(err)
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tags
}

func serveTransferProcess(t *testing.T, base string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeHelperProcess$")
	cmd.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+base)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := stdin.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve exit = %v, stderr %q", err, &stderr)
		}
		cancel()
		// No MCP input was sent; even a blank line here is unsolicited output.
		if stdout.Len() != 0 {
			t.Errorf("owner stdout = %q, want empty", &stdout)
		}
	}
	t.Cleanup(stop)
	for {
		status, info, err := ipc.Probe(ctx, base)
		if err == nil && status == ipc.StatusOwnerLive && info.PID == cmd.Process.Pid {
			return stop
		}
		select {
		case <-ctx.Done():
			stop()
			t.Fatalf("serve did not become a verified live owner: %v, stderr %q", err, &stderr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTransferPullHelperProcess(t *testing.T) {
	base := os.Getenv("MEMDOLT_PULL_HELPER_DIR")
	if base == "" {
		return
	}
	root := newRootCommand()
	root.SetArgs([]string{"pull", "--dir", base, "--json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
