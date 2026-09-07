package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestRepoStatusCLIProductionDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			if routed {
				serveStore(t, a)
			}
			noRemote := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--dir", a, "--diff", "--json"))
			if noRemote.Status != "no-remote" || noRemote.Diff != nil || noRemote.Remedy == "" {
				t.Fatalf("no remote = %+v", noRemote)
			}
			remote := remoteFileURL(scratchDir(t))
			runMemdolt(t, "repo", "remote", "add", "origin", remote, "--dir", a)
			runMemdolt(t, "note", "add", "initial", "--dir", a)
			first := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			if routed {
				serveStore(t, b)
			}
			runMemdolt(t, "repo", "remote", "add", "mirror", remote, "--dir", b)
			check := func(base, want string, extra ...string) localdolt.RepoStatusReport {
				t.Helper()
				args := append([]string{"repo", "status", "--dir", base, "--json"}, extra...)
				raw := runMemdolt(t, args...)
				got := decodeJSON[localdolt.RepoStatusReport](t, raw)
				if got.Status != want || strings.Count(strings.TrimSpace(raw), "\n") != 0 {
					t.Fatalf("status = %+v, stdout %q; want %s", got, raw, want)
				}
				return got
			}
			current := check(b, "current", "mirror", "--diff")
			if current.Remote != "mirror" || current.MainCommit != first.MainCommit || current.RemoteCommit != first.MainCommit || len(current.Diff.Tables) != 0 {
				t.Fatalf("named current = %+v", current)
			}
			check(b, "offline", "--local")
			runMemdolt(t, "task", "add", "remote task", "--dir", a)
			check(a, "ahead")
			remoteHead := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json")).MainCommit
			behind := check(b, "behind", "--diff")
			if behind.MainCommit != first.MainCommit || behind.RemoteCommit != remoteHead || len(behind.Diff.Tables) != 1 || behind.Diff.Tables[0].Table != "tasks" {
				t.Fatalf("behind diff = %+v", behind)
			}
			if human := runMemdolt(t, "repo", "status", "--dir", b); !strings.Contains(human, "behind") || !strings.Contains(human, remoteHead) || strings.Contains(human, "remote task") {
				t.Fatalf("ordinary human output = %q", human)
			}
			runMemdolt(t, "note", "add", "independent local note", "--dir", b)
			diverged := check(b, "diverged-mergeable")
			if got := check(b, "offline", "--local"); got.MainCommit != diverged.MainCommit || !got.Clean {
				t.Fatal("preview changed main or its working set")
			}
			for _, extra := range [][]string{{"missing"}, {"--local", "mirror"}, {"--local", "--diff"}, {"--local", "--user", "fixture"}, {"--user", ""}} {
				if err := runMemdoltErr(t, append([]string{"repo", "status", "--dir", b}, extra...)...); err == "" {
					t.Fatal("invalid flags/remote succeeded")
				}
			}
		})
	}
}

func TestRepoStatusFetchPreservesTagsAndProtocolStdout(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			remote := configureCLITransferRemote(t, a)
			runMemdolt(t, "push", "--dir", a)
			b := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", b)
			localDB := openRepoFixtureDB(t, b)
			for _, name := range []string{"keep", "local-only"} {
				if _, err := localDB.Exec("CALL DOLT_TAG('--author', 'Local <local@example.invalid>', '-m', 'preserve', ?)", name); err != nil {
					t.Fatal(err)
				}
			}
			tags := transferTags(t, localDB)
			if err := localDB.Close(); err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "note", "add", "remote note", "--dir", a)
			runMemdolt(t, "push", "--dir", a)
			sourceDB := openRepoFixtureDB(t, a)
			for _, name := range []string{"keep", "remote-only"} {
				if _, err := sourceDB.Exec("CALL DOLT_TAG('--author', 'Remote <remote@example.invalid>', '-m', 'replace', ?)", name); err != nil {
					t.Fatal(err)
				}
				if _, err := sourceDB.Exec("CALL DOLT_PUSH('origin', ?)", "refs/tags/"+name); err != nil {
					t.Fatal(err)
				}
			}
			if err := sourceDB.Close(); err != nil {
				t.Fatal(err)
			}
			var stop func()
			if routed {
				stop = serveTransferProcess(t, b)
			}
			for _, refused := range []bool{false, true} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepoStatusHelperProcess$")
				cmd.Env = append(os.Environ(), "MEMDOLT_STATUS_HELPER_DIR="+b)
				if refused {
					cmd.Env = append(cmd.Env, "MEMDOLT_STATUS_HELPER_REFUSE=1")
				}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				cancel()
				if refused {
					if err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "not configured") {
						t.Fatalf("refusal stdout=%q stderr=%q err=%v", &stdout, &stderr, err)
					}
				} else if err != nil || strings.Count(stdout.String(), "\n") != 1 || decodeJSON[localdolt.RepoStatusReport](t, stdout.String()).Status != "behind" {
					t.Fatalf("status stdout=%q stderr=%q err=%v", &stdout, &stderr, err)
				}
			}
			if stop != nil {
				stop()
			}
			after := openRepoFixtureDB(t, b)
			if !reflect.DeepEqual(tags, transferTags(t, after)) {
				t.Fatal("status fetch replaced/followed tags")
			}
		})
	}
}

func TestRepoStatusHelperProcess(t *testing.T) {
	base := os.Getenv("MEMDOLT_STATUS_HELPER_DIR")
	if base == "" {
		return
	}
	args := []string{"repo", "status", "--dir", base, "--json"}
	if os.Getenv("MEMDOLT_STATUS_HELPER_REFUSE") != "" {
		args = append(args, "missing")
	}
	root := newRootCommand()
	root.SetArgs(args)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
