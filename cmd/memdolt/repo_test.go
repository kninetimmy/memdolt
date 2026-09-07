package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	embedded "github.com/dolthub/driver"
	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/singleowner"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

func TestRepoStatusPreservesLocalStateDirectAndOwner(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirty), func(t *testing.T) {
			ctx := context.Background()
			base := initStore(t)
			task := decodeJSON[taskInfo](t, runMemdolt(t, "task", "add", "committed task", "--dir", base, "--json"))
			st := openInitializedStore(t, base)
			var staged []localdolt.StagedProposal
			for i, target := range []localdolt.ProposalTarget{localdolt.TargetRepo, localdolt.TargetRepo, localdolt.TargetGlobal} {
				proposal, err := st.ProposeFact(ctx, localdolt.Proposal{
					Rationale: "status fixture", Actor: cliStagingActor, Target: target,
				}, localdolt.Fact{Key: fmt.Sprintf("status.fact%d", i), Value: "proposal value"})
				if err != nil {
					t.Fatal(err)
				}
				staged = append(staged, proposal)
			}
			// Expected-commit acceptance leaves the merged branch as physical
			// residue. Status must use PendingProposals' reachability rule.
			accepted, err := st.AcceptProposal(ctx, staged[0].ID, cliActor, localdolt.AcceptOptions{
				ExpectedCommit: staged[0].Commit, Force: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if dirty {
				db := openRepoFixtureDB(t, base)
				for _, statement := range []store.Statement{
					{SQL: "UPDATE tasks SET title = ? WHERE id = ?", Args: []any{"staged title", task.ID}},
					{SQL: "CALL DOLT_ADD('tasks')"},
					{SQL: "UPDATE tasks SET notes = ? WHERE id = ?", Args: []any{"unstaged notes", task.ID}},
				} {
					if _, err := db.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			st = openInitializedStore(t, base)
			before := repoStateSnapshot(t, st)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			// A configured hub does not change what this local-only command
			// inspects. There is no server at this deliberately unusable URL.
			writeTestFile(t, pathsFor(t, base).ConfigFile(), "[repo]\nremote_url = 'http://127.0.0.1:0/memory'\ntopology = 'live'\n")
			directJSON := runMemdolt(t, "repo", "status", "--local", "--dir", filepath.Join(base, "."), "--json")
			direct := decodeJSON[localdolt.RepoStatusReport](t, directJSON)
			if strings.Count(strings.TrimSpace(directJSON), "\n") != 0 {
				t.Fatalf("status emitted more than one JSON line: %q", directJSON)
			}
			if !direct.LocalOnly || direct.Store != pathsFor(t, base).DoltDataDir() ||
				direct.SchemaVersion != store.LatestSchemaVersion() || direct.MainCommit != accepted.Commit {
				t.Fatalf("unexpected repository identity: %+v (accepted %+v)", direct, accepted)
			}
			if direct.PendingProposals.Repo != 1 || direct.PendingProposals.Global != 1 {
				t.Fatalf("pending counts = %+v, want one repo and one global", direct.PendingProposals)
			}
			wantChanges := []localdolt.RepoTableChange{}
			if dirty {
				wantChanges = []localdolt.RepoTableChange{{Table: "tasks", Status: "modified"}, {Table: "tasks", Staged: true, Status: "modified"}}
			}
			if direct.Clean == dirty || !reflect.DeepEqual(direct.Changes, wantChanges) {
				t.Fatalf("working set = clean %t, %+v; want clean %t, %+v", direct.Clean, direct.Changes, !dirty, wantChanges)
			}
			human := runMemdolt(t, "repo", "status", "--local", "--dir", base)
			for _, want := range []string{"local-only", "remote state not checked", direct.Store, direct.MainCommit,
				fmt.Sprintf("schema: v%d", direct.SchemaVersion), "pending proposals: 1 repo, 1 global"} {
				if !strings.Contains(human, want) {
					t.Errorf("human output %q is missing %q", human, want)
				}
			}
			if dirty && (!strings.Contains(human, "staged=true  modified") || !strings.Contains(human, "staged=false  modified")) {
				t.Fatalf("human output omits staged/unstaged changes: %q", human)
			}
			st = openInitializedStore(t, base)
			if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("direct status changed state: before %v, after %v", before, after)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			serveStore(t, base)
			routed := decodeJSON[localdolt.RepoStatusReport](t, runMemdolt(t, "repo", "status", "--local", "--dir", base, "--json"))
			if !reflect.DeepEqual(routed, direct) {
				t.Fatalf("owner status %+v differs from direct %+v", routed, direct)
			}
			if got := runMemdolt(t, "repo", "status", "--local", "--dir", base); got != human {
				t.Fatalf("owner human output %q differs from direct %q", got, human)
			}
			owner, err := storeipc.DialOwnerStore(base)
			if err != nil {
				t.Fatal(err)
			}
			if after := repoStateSnapshot(t, owner); !reflect.DeepEqual(before, after) {
				t.Fatalf("owner status changed state: before %v, after %v", before, after)
			}
		})
	}
}

func TestRepoStatusDoesNotInitializeMissingStores(t *testing.T) {
	for _, existing := range []string{"", ".memdolt", ".memdolt/dolt", ".memdolt/dolt/memory", ".memdolt/dolt/memory/.dolt", ".memdolt/dolt/memory/.dolt/noms"} {
		t.Run(existing, func(t *testing.T) {
			base := scratchDir(t)
			if existing != "" {
				if err := os.MkdirAll(filepath.Join(base, existing), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before := repoFiles(t, base)
			got := runMemdoltErr(t, "repo", "status", "--dir", base, "--json")
			if !strings.Contains(got, "memdolt init") {
				t.Fatalf("missing-store error = %q, want an init remedy", got)
			}
			if after := repoFiles(t, base); !reflect.DeepEqual(before, after) {
				t.Fatalf("missing-store status created files: before %v, after %v (error %q)", before, after, got)
			}
		})
	}
}

func TestRepoStatusRefusesInvalidManifest(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			base := scratchDir(t)
			manifest := filepath.Join(pathsFor(t, base).DoltDataDir(), "memory", ".dolt", "noms", "manifest")
			if directory {
				if err := os.MkdirAll(manifest, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestFile(t, manifest, "")
			}
			before := repoFiles(t, base)
			if got := runMemdoltErr(t, "repo", "status", "--dir", base); !strings.Contains(got, "invalid memdolt manifest") {
				t.Fatalf("invalid manifest error = %q", got)
			}
			if after := repoFiles(t, base); !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid-store status created files: before %v, after %v", before, after)
			}
		})
	}
}

func TestRepoStatusRefusesUnsupportedSchemasDirectAndOwner(t *testing.T) {
	for _, version := range []int{store.LatestSchemaVersion() - 1, store.LatestSchemaVersion() + 1} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := context.Background()
			base := initStore(t)
			st := openInitializedStore(t, base)
			versionText := strconv.Itoa(version)
			commit, err := st.Commit(ctx, store.CommitRequest{
				Statements: []store.Statement{{SQL: "UPDATE meta SET v = ? WHERE k = ?", Args: []any{versionText, store.SchemaVersionKey}}},
				NoText:     true, Message: "simulate unsupported schema", Author: cliActor,
			})
			if err != nil {
				t.Fatal(err)
			}
			backend := &repoFaultStore{commandStore: &localCommandStore{Store: st, baseDir: base}}
			owner := serveRepoBackend(t, base, backend)
			remedy := "missing migrations"
			if version > store.LatestSchemaVersion() {
				remedy = "memdolt upgrade"
			}
			if got := runMemdoltErr(t, "repo", "status", "--dir", base); !strings.Contains(got, remedy) {
				t.Fatalf("owner schema refusal = %q, want %q", got, remedy)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if got := runMemdoltErr(t, "repo", "status", "--dir", base); !strings.Contains(got, remedy) {
				t.Fatalf("direct schema refusal = %q, want %q", got, remedy)
			}
			// Inspect with the raw fixture driver: the production opener rightly
			// refuses to give this binary a newer-schema store handle.
			db := openRepoFixtureDB(t, base)
			var head, actualVersion, working, staged, committed string
			if err := db.QueryRowContext(ctx,
				"SELECT hash, (SELECT v FROM meta WHERE k = ?), DOLT_HASHOF_DB('WORKING'), "+
					"DOLT_HASHOF_DB('STAGED'), DOLT_HASHOF_DB('HEAD') FROM dolt_branches WHERE name = 'main'",
				store.SchemaVersionKey).Scan(&head, &actualVersion, &working, &staged, &committed); err != nil {
				t.Fatal(err)
			}
			if head != commit.Hash || actualVersion != versionText || working != committed || staged != committed {
				t.Fatalf("schema refusal changed state: head %s, version %s, roots %s/%s/%s", head, actualVersion, working, staged, committed)
			}
		})
	}
}

func TestRepoStatusOwnerFailuresNeverOpenEmbedded(t *testing.T) {
	for _, failure := range []string{"unreadable pidfile", "unverified owner", "authentication"} {
		t.Run(failure, func(t *testing.T) {
			base := initStore(t)
			paths := pathsFor(t, base)
			// Removing this fixture's released lock makes any embedded-open
			// fallback observable even if it subsequently fails.
			if err := os.Remove(paths.LockFile()); err != nil {
				t.Fatal(err)
			}
			want := "check for a live store owner"
			switch failure {
			case "unreadable pidfile":
				if err := os.Mkdir(paths.PidFile(), 0o755); err != nil {
					t.Fatal(err)
				}
			case "unverified owner":
				unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "fixture owner unavailable", http.StatusServiceUnavailable)
				}))
				defer unhealthy.Close()
				endpoint, err := url.Parse(unhealthy.URL)
				if err != nil {
					t.Fatal(err)
				}
				port, err := strconv.Atoi(endpoint.Port())
				if err != nil {
					t.Fatal(err)
				}
				lock, err := singleowner.Acquire(paths.PidFile(), singleowner.Options{
					Detail: map[string]any{"port": port, "token": "status-test"},
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lock.Release() }()
			case "authentication":
				server, err := ipc.Listen(ipc.Config{
					BaseDir: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
					Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						http.Error(w, "fixture authentication refusal", http.StatusUnauthorized)
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = server.Close() }()
				want = "401"
			}
			if got := runMemdoltErr(t, "repo", "status", "--dir", base); !strings.Contains(got, want) {
				t.Fatalf("owner failure = %q, want %q", got, want)
			}
			if _, err := os.Stat(paths.LockFile()); !os.IsNotExist(err) {
				t.Fatalf("owner failure attempted an embedded open: lock stat %v", err)
			}
		})
	}
}

func TestRepoStatusReportsReadOutputAndCloseFailures(t *testing.T) {
	fixtureError := errors.New("injected status failure")
	closeError := errors.New("injected close failure")
	base := initStore(t)
	for _, failure := range []string{"inspection", "human output", "json output", "close"} {
		t.Run(failure, func(t *testing.T) {
			backend := &repoFaultStore{
				commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base},
				closeErr:     closeError,
			}
			cmd := &cobra.Command{Use: "status"}
			cmd.SetContext(context.Background())
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			jsonOutput = failure == "json output"
			switch failure {
			case "inspection":
				backend.statusErr = fixtureError
			case "human output", "json output":
				backend.closeErr = nil
				cmd.SetOut(repoFailWriter{fixtureError})
			}
			err := runRepoStatus(cmd, backend, localdolt.RepoStatusOptions{Local: true})
			if (backend.closeErr != nil && !errors.Is(err, closeError)) || !backend.closed {
				t.Fatalf("close error was lost: %v, closed=%t", err, backend.closed)
			}
			if failure != "close" && !errors.Is(err, fixtureError) {
				t.Fatalf("status failure was lost: %v", err)
			}
			if failure != "close" && out.Len() != 0 {
				t.Fatalf("failed status emitted a successful report: %q", out)
			}
		})
	}
}

type repoFaultStore struct {
	commandStore
	closeErr, statusErr error
	closed              bool
}

func (s *repoFaultStore) RepoStatus(ctx context.Context, opts localdolt.RepoStatusOptions) (localdolt.RepoStatusReport, error) {
	if s.statusErr != nil {
		return localdolt.RepoStatusReport{}, s.statusErr
	}
	return s.commandStore.RepoStatus(ctx, opts)
}

func (s *repoFaultStore) Close() error {
	s.closed = true
	return errors.Join(s.commandStore.Close(), s.closeErr)
}

type repoFailWriter struct{ err error }

func (w repoFailWriter) Write([]byte) (int, error) { return 0, w.err }

func serveRepoBackend(t *testing.T, base string, backend storeipc.Backend) *ipc.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := storeipc.NewHandler(storeipc.Config{
		Store: backend, Logger: logger,
		ReviewAccept: func(context.Context, string, string, store.Actor, bool) (localdolt.AcceptResult, error) {
			return localdolt.AcceptResult{}, errors.New("status must not accept proposals")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: handler, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func openRepoFixtureDB(t *testing.T, base string) *sql.DB {
	t.Helper()
	connector, err := embedded.NewConnector(embedded.Config{
		Directory: pathsFor(t, base).DoltDataDir(), Database: localdolt.DatabaseName,
		CommitName: cliActor.Name, CommitEmail: cliActor.Email,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Include every branch head and both complete main roots, so a status call
// cannot pass by preserving counts while changing rows or staging them.
func repoStateSnapshot(t *testing.T, st store.Store) [][]string {
	t.Helper()
	var snapshot [][]string
	for _, query := range []string{
		"SELECT name, hash FROM dolt_branches ORDER BY name",
		"SELECT DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED')",
	} {
		rows, err := st.Query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			pair := make([]string, 2)
			if err := rows.Scan(&pair[0], &pair[1]); err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, pair)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func repoFiles(t *testing.T, base string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(base, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}
