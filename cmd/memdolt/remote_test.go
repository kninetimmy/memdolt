package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func remoteFileURL(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func TestRemoteCLIConfigAndTransferDirectAndOwner(t *testing.T) {
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	if err := os.Unsetenv("DOLT_REMOTE_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			a := initStore(t)
			if routed {
				serveStore(t, a)
			}
			if got := runMemdolt(t, "repo", "remote", "list", "--dir", a, "--json"); got != "{\"remotes\":[]}\n" {
				t.Fatalf("empty list = %q", got)
			}
			if got := runMemdolt(t, "repo", "remote", "list", "--dir", a); got != "no remotes configured\n" {
				t.Fatalf("empty human list = %q", got)
			}
			target := filepath.Join(scratchDir(t), "remote with spaces")
			raw := remoteFileURL(target)
			added := decodeJSON[localdolt.Remote](t, runMemdolt(t, "repo", "remote", "add", "z-files", raw, "--dir", a, "--json"))
			if added != (localdolt.Remote{Name: "z-files", URL: raw}) {
				t.Fatalf("added = %+v", added)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("configuration contacted file target: %v", err)
			}
			// No password or listener is needed for storing an SQL username.
			httpRemote := localdolt.Remote{Name: "A-http", URL: "http://127.0.0.1:1/fixture", User: "fixture.user"}
			if got := runMemdolt(t, "repo", "remote", "add", httpRemote.Name, httpRemote.URL, "--user", httpRemote.User, "--dir", a); !strings.Contains(got, "user=fixture.user") {
				t.Fatalf("add human = %s", got)
			}
			report := decodeJSON[remoteListReport](t, runMemdolt(t, "repo", "remote", "list", "--dir", a, "--json"))
			if !reflect.DeepEqual(report.Remotes, []localdolt.Remote{httpRemote, added}) {
				t.Fatalf("sorted remotes = %+v", report.Remotes)
			}
			before := nativeRemoteConfig(t, a)
			if err := runMemdoltErr(t, "repo", "remote", "add", "z-files", "https://example.invalid/other", "--dir", a, "--json"); !strings.Contains(err, "already configured") {
				t.Fatal(err)
			}
			if got := nativeRemoteConfig(t, a); !bytes.Equal(got, before) {
				t.Fatal("duplicate add rewrote configuration")
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "task", "add", "first named remote task", "--dir", a)
			first := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "z-files", "--dir", a, "--json"))
			b := scratchDir(t)
			runMemdolt(t, "clone", raw, "--dir", b)
			if routed {
				serveStore(t, b)
			}
			runMemdolt(t, "repo", "remote", "add", "named", raw, "--dir", b)
			runMemdolt(t, "note", "add", "second named remote note", "--dir", a, "--actor", "cli")
			pushed := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "z-files", "--dir", a, "--json"))
			pulled := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "pull", "named", "--dir", b, "--json"))
			if !pulled.Changed || pulled.LocalCommit != first.MainCommit || pulled.MainCommit != pushed.MainCommit {
				t.Fatalf("round trip: first=%+v push=%+v pull=%+v", first, pushed, pulled)
			}
			if got := runMemdolt(t, "note", "list", "--dir", b); !strings.Contains(got, "second named remote note") {
				t.Fatalf("transferred note missing: %s", got)
			}
		})
	}
}

func nativeRemoteConfig(t *testing.T, base string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pathsFor(t, base).DoltDataDir(), "memory", ".dolt", "repo_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRemoteCLIPreservesDirtyMemoryProposalsAndConfiguration(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			st := openInitializedStore(t, base)
			if _, err := st.ProposeFact(context.Background(), localdolt.Proposal{Rationale: "remote fixture", Actor: cliStagingActor, Target: localdolt.TargetRepo}, localdolt.Fact{Key: "remote.pending", Value: "preserve"}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			configureCLITransferRemote(t, base)
			db := openRepoFixtureDB(t, base)
			for _, query := range []string{
				"CALL DOLT_TAG('--author', 'Fixture <fixture@example.invalid>', '-m', 'preserve tag metadata', 'keep')",
				"INSERT INTO tasks (id, title) VALUES ('dirty', 'staged title')",
				"CALL DOLT_ADD('tasks')",
				"UPDATE tasks SET notes = 'unstaged notes' WHERE id = 'dirty'",
			} {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			tags := transferTags(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			artifacts := []string{"config.toml", "embeddings.sqlite", "code_index.sqlite", "rendered/PROJECT.md"}
			for _, name := range artifacts {
				writeTestFile(t, filepath.Join(base, ".memdolt", filepath.FromSlash(name)), "# preserve local artifact\n")
			}
			st = openInitializedStore(t, base)
			before := repoStateSnapshot(t, st)
			var oldState map[string]json.RawMessage
			if err := json.Unmarshal(nativeRemoteConfig(t, base), &oldState); err != nil {
				t.Fatal(err)
			}
			var backend commandStore = &localCommandStore{Store: st, baseDir: base}
			var endpoint *ipc.Server
			if routed {
				endpoint = serveRepoBackend(t, base, st)
				owner, err := storeipc.DialOwnerStore(base)
				if err != nil {
					t.Fatal(err)
				}
				backend = owner
			} else if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "repo", "remote", "add", "new", "https://example.invalid/memory", "--dir", base)
			runMemdolt(t, "repo", "remote", "list", "--dir", base)
			if !routed {
				st = openInitializedStore(t, base)
				backend = &localCommandStore{Store: st, baseDir: base}
			}
			if after := repoStateSnapshot(t, backend); !reflect.DeepEqual(before, after) {
				t.Fatalf("memory/proposals changed: before=%v after=%v", before, after)
			}
			var newState map[string]json.RawMessage
			if err := json.Unmarshal(nativeRemoteConfig(t, base), &newState); err != nil {
				t.Fatal(err)
			}
			var oldRemotes, newRemotes map[string]env.Remote
			if err := json.Unmarshal(oldState["remotes"], &oldRemotes); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(newState["remotes"], &newRemotes); err != nil {
				t.Fatal(err)
			}
			delete(newRemotes, "new")
			delete(oldState, "remotes")
			delete(newState, "remotes")
			if !reflect.DeepEqual(oldRemotes, newRemotes) || !reflect.DeepEqual(oldState, newState) {
				t.Fatal("remote add changed existing native state")
			}
			if endpoint != nil {
				if err := endpoint.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db = openRepoFixtureDB(t, base)
			if after := transferTags(t, db); !reflect.DeepEqual(tags, after) {
				t.Fatalf("tags changed: before=%v after=%v", tags, after)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for _, name := range artifacts {
				data, err := os.ReadFile(filepath.Join(base, ".memdolt", filepath.FromSlash(name)))
				if err != nil || string(data) != "# preserve local artifact\n" {
					t.Fatalf("changed %s: %v", name, err)
				}
			}
		})
	}
}

func TestRemoteCLIValidationMissingStoreAndHelp(t *testing.T) {
	base := initStore(t)
	file := remoteFileURL(filepath.Join(scratchDir(t), "absent"))
	before := nativeRemoteConfig(t, base)
	for _, args := range [][]string{
		{"add"}, {"add", "x"}, {"add", "x", "https://host/db", "unexpected"}, {"list", "http://user:rejected-secret@host/db"},
		{"add", "", "https://host/db"}, {"add", "origin/main", "https://host/db"},
		{"add", "x", "http://user:rejected-secret@host/db"}, {"add", "x", "https://host/db?password=rejected-secret"},
		{"add", "x", "https://host/db#rejected-secret"}, {"add", "x", "ssh://host/db"}, {"add", "x", "file:relative"},
		{"add", "x", "https://host/"}, {"add", "x", "https://host:65536/db"}, {"add", "x", file + "%25ambiguous"},
		{"add", "x", "https://host/db", "--user", "rejected-secret:bad"}, {"add", "x", "https://host/db", "--user", "--force"},
		{"add", "x", "https://host/db", "--user", ""}, {"add", "x", file, "--user", "fixture"},
		{"add", "x", "https://host/db", "--password", "rejected-secret"}, {"add", "x", "https://host/db", "--force"},
	} {
		got := runMemdoltErr(t, append(append([]string{"repo", "remote"}, args...), "--dir", base, "--json")...)
		if strings.Contains(got, "rejected-secret") {
			t.Fatalf("rejected input leaked: %s", got)
		}
		if !bytes.Equal(before, nativeRemoteConfig(t, base)) {
			t.Fatal("invalid input changed config")
		}
	}
	// -- ends option parsing: an option-like name still fails shared validation.
	if err := runMemdoltErr(t, "repo", "remote", "add", "--dir", base, "--", "--force", "https://host/db"); !strings.Contains(err, "invalid remote name") {
		t.Fatal(err)
	}
	for _, operation := range []string{"list", "add"} {
		missing := scratchDir(t)
		args := []string{"repo", "remote", operation, "--dir", missing, "--json"}
		if operation == "add" {
			args = append(args, "x", file)
		}
		files := repoFiles(t, missing)
		if err := runMemdoltErr(t, args...); !strings.Contains(err, "memdolt init") {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(files, repoFiles(t, missing)) {
			t.Fatal("missing store initialized")
		}
		help := runMemdolt(t, "repo", "remote", operation, "--help")
		for _, want := range []string{"owner", "--dir", "--json"} {
			if !strings.Contains(help, want) {
				t.Fatalf("help lacks %q: %s", want, help)
			}
		}
	}
}

func TestRemoteCLIStoredUserReachesTransferOnly(t *testing.T) {
	const password = "synthetic-remote-password"
	t.Setenv("DOLT_REMOTE_PASSWORD", password)
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			headers := make(chan string, 8)
			server := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
				md, _ := metadata.FromIncomingContext(stream.Context())
				headers <- strings.Join(md.Get("authorization"), " ")
				return status.Error(codes.Unauthenticated, "synthetic authentication refusal")
			}))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() { server.Stop(); <-done }()
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			runMemdolt(t, "repo", "remote", "add", "auth", "http://"+listener.Addr().String()+"/fixture", "--user", "stored-user", "--dir", base)
			runMemdolt(t, "repo", "remote", "list", "--dir", base)
			if len(headers) != 0 {
				t.Fatal("configuration contacted remote")
			}
			for _, operation := range []string{"push", "pull"} {
				if err := runMemdoltErr(t, operation, "auth", "--dir", base, "--json"); !strings.Contains(err, "authentication refusal") || strings.Contains(err, password) {
					t.Fatalf("transfer error = %s", err)
				}
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("stored-user:"+password))
				select {
				case got := <-headers:
					if got != want {
						t.Fatal("stored username/password environment not used by transfer")
					}
				default:
					t.Fatal("transfer did not reach synthetic server")
				}
			}
			if bytes.Contains(nativeRemoteConfig(t, base), []byte(password)) {
				t.Fatal("configuration persisted password")
			}
		})
	}
}

func TestRemoteCLIOwnerFailuresAndFinalization(t *testing.T) {
	for _, authentication := range []bool{false, true} {
		base := initStore(t)
		before := nativeRemoteConfig(t, base)
		// A direct-open fallback would recreate this released fixture lock.
		if err := os.Remove(pathsFor(t, base).LockFile()); err != nil {
			t.Fatal(err)
		}
		var closeOwner func()
		if authentication {
			server, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "synthetic authentication refusal", http.StatusUnauthorized)
			})})
			if err != nil {
				t.Fatal(err)
			}
			closeOwner = func() { _ = server.Close() }
		} else {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "synthetic unhealthy owner", http.StatusServiceUnavailable)
			}))
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(endpoint.Port())
			if err != nil {
				t.Fatal(err)
			}
			lock, err := singleowner.Acquire(pathsFor(t, base).PidFile(), singleowner.Options{Detail: map[string]any{"port": port, "token": "fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			closeOwner = func() { _ = lock.Release(); server.Close() }
		}
		for _, args := range [][]string{{"list"}, {"add", "x", "https://host/db"}} {
			err := runMemdoltErr(t, append(append([]string{"repo", "remote"}, args...), "--dir", base, "--json")...)
			if !strings.Contains(err, "live store owner") && !strings.Contains(err, "401") {
				t.Fatalf("owner failure = %s", err)
			}
			if !bytes.Equal(nativeRemoteConfig(t, base), before) {
				t.Fatal("failed owner check changed config")
			}
			if _, err := os.Stat(pathsFor(t, base).LockFile()); !os.IsNotExist(err) {
				t.Fatalf("owner failure attempted a direct open: %v", err)
			}
		}
		closeOwner()
	}
	for _, add := range []bool{false, true} {
		for _, failure := range []string{"close", "human", "json"} {
			base := initStore(t)
			fixtureError := errors.New("synthetic remote finalization failure")
			st := &repoFaultStore{commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base}}
			cmd := &cobra.Command{Use: "remote"}
			cmd.SetContext(context.Background())
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			jsonOutput = failure == "json"
			if failure == "close" {
				st.closeErr = fixtureError
			} else {
				cmd.SetOut(repoFailWriter{fixtureError})
			}
			var remote *localdolt.Remote
			if add {
				remote = &localdolt.Remote{Name: "x", URL: "https://host/db"}
			}
			err := runRepoRemote(cmd, st, remote)
			if !errors.Is(err, fixtureError) || !st.closed || out.Len() != 0 || (add && !strings.Contains(err.Error(), "confirmed persisted")) {
				t.Fatalf("finalization add=%t/%s: %v, closed=%t, output=%s", add, failure, err, st.closed, out)
			}
		}
	}
}

func TestRemoteCLIUnsafeNativeConfigAndUnsupportedSchema(t *testing.T) {
	for _, fixture := range []string{"unsafe URL", "unsafe parameters", "older schema", "newer schema"} {
		t.Run(fixture, func(t *testing.T) {
			base := initStore(t)
			if strings.HasPrefix(fixture, "unsafe") {
				var state map[string]json.RawMessage
				if err := json.Unmarshal(nativeRemoteConfig(t, base), &state); err != nil {
					t.Fatal(err)
				}
				remote := env.NewRemote("legacy", "https://example.invalid/db", map[string]string{})
				if fixture == "unsafe URL" {
					remote.Url = "https://user:rejected-secret@host/db"
				} else {
					remote.Params["password"] = "rejected-secret"
				}
				var err error
				state["remotes"], err = json.Marshal(map[string]env.Remote{"legacy": remote})
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(pathsFor(t, base).DoltDataDir(), "memory", ".dolt", "repo_state.json"), string(data))
			}
			st := openInitializedStore(t, base)
			if strings.HasSuffix(fixture, "schema") {
				version := store.LatestSchemaVersion() - 1
				if fixture == "newer schema" {
					version = store.LatestSchemaVersion() + 1
				}
				if _, err := st.Commit(context.Background(), store.CommitRequest{
					Author: cliActor, Message: "unsupported schema fixture", NoText: true,
					Statements: []store.Statement{{SQL: "UPDATE meta SET v = ? WHERE k = ?", Args: []any{strconv.Itoa(version), store.SchemaVersionKey}}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := repoStateSnapshot(t, st)
			config := nativeRemoteConfig(t, base)
			endpoint := serveRepoBackend(t, base, st)
			for _, routed := range []bool{true, false} {
				if !routed {
					if err := endpoint.Close(); err != nil {
						t.Fatal(err)
					}
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
				}
				for _, args := range [][]string{{"list"}, {"add", "new", "https://example.invalid/db"}} {
					root := newRootCommand()
					out := &bytes.Buffer{}
					root.SetOut(out)
					root.SetErr(out)
					root.SetArgs(append(append([]string{"repo", "remote"}, args...), "--dir", base, "--json"))
					err := root.Execute()
					if err == nil || strings.Contains(err.Error(), "rejected-secret") || out.Len() != 0 {
						t.Fatalf("%s owner=%t refusal=%v, stdout=%q", fixture, routed, err, out)
					}
					if !bytes.Equal(config, nativeRemoteConfig(t, base)) {
						t.Fatal("refused command rewrote native configuration")
					}
				}
				if routed && !reflect.DeepEqual(repoStateSnapshot(t, st), before) {
					t.Fatal("refused commands changed memory")
				}
			}
			// Raw fixture inspection remains available for a newer schema that the
			// production opener correctly refuses to expose to this binary.
			db := openRepoFixtureDB(t, base)
			var main, working, staged string
			if err := db.QueryRow("SELECT DOLT_HASHOF('main'), DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED')").Scan(&main, &working, &staged); err != nil {
				t.Fatal(err)
			}
			if main != before[0][1] || working != before[1][0] || staged != before[1][1] {
				t.Fatal("refused direct commands changed main or memory roots")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
