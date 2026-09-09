package localdolt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/kninetimmy/memdolt/internal/store"
)

func TestRepoConfigValidationAndProtectedReplacement(t *testing.T) {
	for _, raw := range []string{
		"[repo]\ntopology='live'", "[repo]\ntopology=3", "[repo]\ntopology='clnoe'",
		"[repo]\nauto_pull_on_session_start=true", "[repo]\ntopology='local'\nauto_pull_on_session_start=true",
		"[repo]\nremote_url='https://user:secret@host/db'", "[repo]\nremote_url='http://host:0/db'",
		"[repo]\nremote_url=7", "[repo]\nauto_pull_on_session_start='true'", "[repo]\nauto_pull=true", "[repo",
	} {
		if _, err := decodeRepoConfig([]byte(raw)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config refusal = %v", err)
		}
	}
	s := openInternalTestStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := "project_name='preserve name'\n[retrieval]\ninclude_docs_in_default=true\n[unknown]\nitems=['one','two']\n"
	if err := os.WriteFile(s.paths.ConfigFile(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := RepoConfig{Topology: "clone", RemoteURL: "http://127.0.0.1:1/memory", AutoPullOnSessionStart: true}
	if changed, err := SetRepoConfig(s.paths.Base(), cfg); err != nil || !changed {
		t.Fatalf("set config = %t, %v", changed, err)
	}
	if got, err := ReadRepoConfig(s.paths.Base()); err != nil || got != cfg {
		t.Fatalf("read config = %+v, %v", got, err)
	}
	var values map[string]any
	if _, err := toml.DecodeFile(s.paths.ConfigFile(), &values); err != nil || values["project_name"] != "preserve name" || values["unknown"] == nil || values["retrieval"] == nil {
		t.Fatalf("unrelated config lost = %+v, %v", values, err)
	}
	if changed, err := SetRepoConfig(s.paths.Base(), cfg); err != nil || changed {
		t.Fatalf("repeat config = %t, %v", changed, err)
	}
	for _, kind := range []string{"symlink", "owner-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			state := filepath.Join(base, ".memdolt")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(state, "server.pid")
			if err := os.WriteFile(source, []byte("protected credential fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(source, filepath.Join(state, "config.toml")); err != nil {
				t.Skipf("links unavailable: %v", err)
			}
			if _, err := ReadRepoConfig(base); err == nil || strings.Contains(err.Error(), "credential fixture") {
				t.Fatalf("protected config = %v", err)
			}
		})
	}
}

func TestRepoConfigDefaultTargetAndLocalPolicy(t *testing.T) {
	ctx := context.Background()
	s, _ := transferFixture(t)
	remotes, err := s.ListRemotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remote := remotes[0]
	if _, err := s.db.Exec("CALL DOLT_REMOTE('remove', 'origin')"); err != nil {
		t.Fatal(err)
	}
	if changed, err := SetRepoConfig(s.paths.Base(), RepoConfig{Topology: "clone", RemoteURL: remote.URL}); err != nil || !changed {
		t.Fatalf("config default = %t, %v", changed, err)
	}
	if got, err := s.Push(ctx, TransferOptions{}); err != nil || !got.Changed {
		t.Fatalf("TOML-only push = %+v, %v", got, err)
	}
	if remotes, err := s.ListRemotes(ctx); err != nil || len(remotes) != 0 {
		t.Fatalf("TOML default silently changed native remotes: %+v, %v", remotes, err)
	}
	if got, err := s.RepoStatus(ctx, RepoStatusOptions{}); err != nil || got.Status != "current" {
		t.Fatalf("TOML-only status = %+v, %v", got, err)
	}
	if _, err := s.AddRemote(ctx, Remote{Name: "origin", URL: "http://127.0.0.1:1/other"}); err == nil {
		t.Fatal("native origin configuration accepted a conflicting default")
	}
	if _, err := s.db.Exec("CALL DOLT_REMOTE('add', 'origin', 'http://127.0.0.1:1/other')"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Pull(ctx, TransferOptions{}); err == nil || got.Changed || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting default = %+v, %v", got, err)
	}
	if _, err := s.AddRemote(ctx, Remote{Name: "selected", URL: remote.URL}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Pull(ctx, TransferOptions{Remote: "selected"}); err != nil || got.Status != "current" {
		t.Fatalf("explicit remote = %+v, %v", got, err)
	}
	if _, err := SetRepoConfig(s.paths.Base(), RepoConfig{Topology: "local"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RepoStatus(ctx, RepoStatusOptions{}); err != nil || !got.LocalOnly || got.Status != "offline" {
		t.Fatalf("local topology contacted remote = %+v, %v", got, err)
	}
	if got, err := s.PullOnSessionStart(ctx); err != nil || got.Operation != "" {
		t.Fatalf("local startup contacted remote = %+v, %v", got, err)
	}
	if _, err := SetRepoConfig(s.paths.Base(), RepoConfig{Topology: "clone", AutoPullOnSessionStart: true}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PullOnSessionStart(canceled); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "no automatic retry") {
		t.Fatalf("canceled startup = %v", err)
	}
}

func TestRepoStartupPreservesUncertainAndConfirmedPullResults(t *testing.T) {
	s := openInternalTestStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := SetRepoConfig(s.paths.Base(), RepoConfig{Topology: "clone", AutoPullOnSessionStart: true}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"unknown", "changed", "conflicted"} {
		calls := 0
		want := TransferResult{Operation: "pull", Status: status, Changed: status == "changed", MainCommit: "00000000000000000000000000000000", Remedy: "inspect conflicts"}
		got, err := s.pullOnSessionStart(context.Background(), func(_ context.Context, opts TransferOptions) (TransferResult, error) {
			calls++
			if opts.Resolution != nil || opts.User != "" || opts.Author.Name != "memdolt" {
				t.Fatalf("startup invented choices, credentials or attribution: %+v", opts)
			}
			if status == "conflicted" {
				return want, nil
			}
			return want, errors.New("fixture uncertain or late native failure")
		})
		if calls != 1 || got.Status != want.Status || got.Changed != want.Changed || got.MainCommit != want.MainCommit || err == nil || !strings.Contains(err.Error(), "inspect") {
			t.Fatalf("startup result/replay = %+v, %v (calls %d)", got, err, calls)
		}
	}
}

func TestRepoStartupNativeRefusalAndLateMerge(t *testing.T) {
	for _, failure := range []string{"conflict", "dirty", "unreachable", "newer", "late"} {
		t.Run(failure, func(t *testing.T) {
			kind := "independent"
			if failure == "conflict" {
				kind = "value"
			}
			a, b, _ := pullFixture(t, kind)
			if _, err := SetRepoConfig(b.paths.Base(), RepoConfig{Topology: "clone", AutoPullOnSessionStart: true}); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "dirty":
				if _, err := b.db.Exec("INSERT INTO tasks (id,title) VALUES ('dirty','preserve')"); err != nil {
					t.Fatal(err)
				}
			case "unreachable":
				if _, err := b.db.Exec("CALL DOLT_REMOTE('remove', 'origin')"); err != nil {
					t.Fatal(err)
				}
				if _, err := b.AddRemote(context.Background(), Remote{Name: "origin", URL: "http://127.0.0.1:1/memory"}); err != nil {
					t.Fatal(err)
				}
			case "newer":
				statusCommit(t, a, store.Statement{SQL: "UPDATE meta SET v=CAST(v AS UNSIGNED)+1 WHERE k='schema_version'"})
				if _, err := a.db.Exec("CALL DOLT_PUSH('origin', 'main')"); err != nil {
					t.Fatal(err)
				}
			}
			before := transferMain(t, b)
			got, err := b.pullOnSessionStart(context.Background(), func(ctx context.Context, opts TransferOptions) (TransferResult, error) {
				hooks := transferHooks{}
				if failure == "late" {
					hooks.afterMove = func() error { return errors.New("fixture late native merge") }
				}
				return b.transfer(ctx, "pull", opts, hooks)
			})
			if err == nil || !strings.Contains(err.Error(), "session-start pull did not finish") {
				t.Fatalf("startup %s = %+v, %v", failure, got, err)
			}
			if failure == "late" {
				var author string
				if !got.Changed || got.MainCommit == before || transferMain(t, b) != got.MainCommit {
					t.Fatalf("confirmed startup merge lost = %+v, %v", got, err)
				}
				if err := b.db.QueryRow("SELECT committer FROM dolt_log WHERE commit_hash=?", got.MainCommit).Scan(&author); err != nil || author != "memdolt" {
					t.Fatalf("startup merge attribution = %s, %v", author, err)
				}
			} else if got.Changed || transferMain(t, b) != before {
				t.Fatalf("refused startup changed main = %+v, %v", got, err)
			}
		})
	}
}
