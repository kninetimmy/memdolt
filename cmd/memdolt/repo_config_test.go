package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func identityCLIGit(t *testing.T, base, origin string) {
	t.Helper()
	for _, args := range [][]string{{"-c", "init.templateDir=", "init", "--quiet", base}, {"-C", base, "config", "--local", "remote.origin.url", origin}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated Git setup = %s, %v", out, err)
		}
	}
}

func TestRepositoryInitIdentityAndConfirmedOutput(t *testing.T) {
	base := scratchDir(t)
	identityCLIGit(t, base, "https://github.com/kninetimmy/memdolt.git")
	first := decodeJSON[initInfo](t, runMemdolt(t, "init", "--dir", base, "--json"))
	if first.ProjectID != "memdolt-414c4f88" || first.Commit == "" || first.Database != "proj_memdolt_414c4f88" {
		t.Fatalf("init identity = %+v", first)
	}
	before := storeCommitCount(t, base)
	again := decodeJSON[initInfo](t, runMemdolt(t, "init", "--dir", base, "--json"))
	if again.Commit != "" || again.ProjectIdentity != first.ProjectIdentity || storeCommitCount(t, base) != before {
		t.Fatalf("repeat init = %+v", again)
	}
	legacy := initStore(t)
	identityCLIGit(t, legacy, "git@github.com:kninetimmy/memdolt")
	root := newRootCommand()
	root.SetArgs([]string{"init", "--dir", legacy})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--adopt-identity") {
		t.Fatalf("implicit existing adoption = %v", err)
	}
	root = newRootCommand()
	root.SetOut(repoFailWriter{err: errors.New("fixture output failure")})
	root.SetArgs([]string{"init", "--adopt-identity", "--dir", legacy, "--json"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "confirmed commits") {
		t.Fatalf("confirmed identity output failure = %v", err)
	}
	adopted := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", legacy, "--json"))
	if adopted.ProjectID != first.ProjectID {
		t.Fatalf("reopened adoption = %+v", adopted)
	}
}

func TestRepositoryConfigurePreservesLateChangesAndRefusesLive(t *testing.T) {
	base := initStore(t)
	late := errors.New("fixture late config close")
	cmd := newRepoConfigureCommand(func(base string, cfg localdolt.RepoConfig) (bool, error) {
		changed, err := localdolt.SetRepoConfig(base, cfg)
		return changed, errors.Join(err, late)
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dir", base, "--topology", "local"})
	if err := cmd.Execute(); !errors.Is(err, late) || !strings.Contains(err.Error(), "change confirmed") || !strings.Contains(out.String(), "true") {
		t.Fatalf("late config = %q, %v", &out, err)
	}
	for _, raw := range []string{"[repo]\ntopology='live'\n", "[repo]\nremote_url='https://token:secret@host/db'\n", "[repo]\nunknown=true\n", "[repo]\ntopology='live'\nTopology='local'\n", "[Repo]\ntopology='local'\n", "[repo]\nremote_url='http://example.invalid/intended'\nRemote_URL='http://example.invalid/other'\n"} {
		writeTestFile(t, pathsFor(t, base).ConfigFile(), raw)
		for _, args := range [][]string{{"task", "list"}, {"repo", "status", "--local"}, {"init"}, {"serve"}} {
			root := newRootCommand()
			root.SetOut(&out)
			root.SetArgs(append(args, "--dir", base))
			if err := root.Execute(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid topology/config reached %v: %v", args, err)
			}
		}
	}
}

func TestRepositoryOwnerStartupPullOnceAndMCP(t *testing.T) {
	a := scratchDir(t)
	identityCLIGit(t, a, "https://github.com/kninetimmy/memdolt")
	runMemdolt(t, "init", "--dir", a)
	remote := remoteFileURL(scratchDir(t))
	runMemdolt(t, "repo", "configure", "--topology", "clone", "--remote-url", remote, "--dir", a)
	runMemdolt(t, "push", "--dir", a)
	b := scratchDir(t)
	identityCLIGit(t, b, "ssh://git@github.com/kninetimmy/memdolt.git")
	writeTestFile(t, pathsFor(t, b).ConfigFile(), "[repo]\ntopology='clone'\nremote_url='"+remote+"'\n")
	cloned := decodeJSON[localdolt.CloneResult](t, runMemdolt(t, "clone", "--dir", b, "--json"))
	runMemdolt(t, "repo", "configure", "--auto-pull-on-session-start", "--dir", b)
	runMemdolt(t, "task", "add", "first remote task", "--dir", a)
	first := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
	// An ordinary CLI open does not pull, even with the opt-in enabled.
	before := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json"))
	if before.MainCommit != cloned.MainCommit {
		t.Fatal("ordinary CLI open pulled implicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeHelperProcess$")
	process.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+b)
	process.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "repository-startup-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: process}, nil)
	if err != nil {
		t.Fatalf("start configured MCP owner: %v, %s", err, &stderr)
	}
	defer func() { _ = session.Close() }()
	status := func() localdolt.RepoStatusReport {
		t.Helper()
		response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "repo_status", Arguments: map[string]any{"local": true}})
		if err != nil || response.IsError {
			t.Fatalf("MCP status = %+v, %v", response, err)
		}
		encoded, err := json.Marshal(response.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return decodeJSON[localdolt.RepoStatusReport](t, string(encoded))
	}
	if got := status(); got.MainCommit != first.MainCommit || got.ProjectID != cloned.ProjectID {
		t.Fatalf("startup identity/pull = %+v", got)
	}
	runMemdolt(t, "task", "add", "second remote task", "--dir", a)
	second := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", a, "--json"))
	if got := status(); got.MainCommit != first.MainCommit {
		t.Fatal("MCP request repeated startup pull")
	}
	if got := runMemdolt(t, "task", "list", "--dir", b); !strings.Contains(got, "first remote task") || strings.Contains(got, "second remote task") {
		t.Fatalf("owner CLI open repeated startup pull: %s", got)
	}
	pulled := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "--dir", b, "--json"))
	if pulled.MainCommit != second.MainCommit {
		t.Fatal("explicit authenticated-owner pull ignored topology")
	}
	owner, err := storeipc.DialOwnerStore(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Open(ctx); err != nil {
		t.Fatal(err)
	}
	identityCLIGit(t, b, "https://github.com/changed/repository")
	request := store.CommitRequest{Author: cliActor, Message: "refused owner fixture", NoText: true, Statements: []store.Statement{{SQL: "INSERT INTO tasks (id,title) VALUES ('refused','must not persist')"}}}
	if got, err := owner.Commit(ctx, request); err == nil || got.Hash != "" || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("raw owner bypassed identity drift = %+v, %v", got, err)
	}
	if response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "task_add", Arguments: map[string]any{"title": "must not persist"}}); err == nil && !response.IsError {
		t.Fatal("MCP ignored changed Git origin")
	}
	identityCLIGit(t, b, "https://github.com/kninetimmy/memdolt")
	configPath := pathsFor(t, b).ConfigFile()
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"[repo]\ntopology='live'\n", "[repo]\ntopology='live'\nTopology='local'\n", "[Repo]\ntopology='local'\n"} {
		writeTestFile(t, configPath, raw)
		if got, err := owner.Commit(ctx, request); err == nil || got.Hash != "" {
			t.Fatalf("raw owner bypassed invalid topology/config = %+v, %v", got, err)
		}
		if response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "task_add", Arguments: map[string]any{"title": "must not persist"}}); err == nil && !response.IsError {
			t.Fatal("MCP ignored invalid topology/config")
		}
	}
	writeTestFile(t, configPath, string(saved))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	assertServeReleased(t, b)
	if got := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", b, "--json")); got.MainCommit != second.MainCommit {
		t.Fatal("reopen changed native history")
	}
}

func TestRepositoryStartupFailureDoesNotPublishOwner(t *testing.T) {
	base := initStore(t)
	runMemdolt(t, "repo", "configure", "--topology", "clone", "--auto-pull-on-session-start", "--dir", base)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeHelperProcess$")
	process.Env = append(os.Environ(), "MEMDOLT_SERVE_HELPER=1", "MEMDOLT_SERVE_DIR="+base)
	var out, stderr bytes.Buffer
	process.Stdout, process.Stderr = &out, &stderr
	if err := process.Run(); err == nil || out.Len() != 0 || !strings.Contains(stderr.String(), "session-start pull did not finish") || !strings.Contains(stderr.String(), "no remote") {
		t.Fatalf("startup refusal stdout=%s stderr=%s err=%v", &out, &stderr, err)
	}
	assertServeReleased(t, base)
}
