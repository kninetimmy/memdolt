package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

func identityGit(t *testing.T, base, origin string) {
	t.Helper()
	for _, args := range [][]string{{"-c", "init.templateDir=", "init", "--quiet", base}, {"-C", base, "config", "--local", "remote.origin.url", origin}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare isolated Git fixture: %s, %v", out, err)
		}
	}
}

func TestProjectIdentityOfflineCompatibilityAndRefusals(t *testing.T) {
	ctx := context.Background()
	want := ProjectIdentity{ProjectID: "memdolt-414c4f88", Origin: "github.com/kninetimmy/memdolt", Database: "proj_memdolt_414c4f88"}
	for _, origin := range []string{"https://GitHub.com/Kninetimmy/Memdolt.git/", "git@github.com:kninetimmy/memdolt", "ssh://git@github.com/kninetimmy/memdolt.git", "HTTPS://GITHUB.COM/KNINETIMMY/MEMDOLT"} {
		base := t.TempDir()
		identityGit(t, base, origin)
		got, err := ResolveProjectIdentity(ctx, base)
		if err != nil || got != want {
			t.Fatalf("identity = %+v, %v; want %+v", got, err, want)
		}
	}
	for _, origin := range []string{"", "file:///secret/repo", `C:\secret\repo`, "https://token@github.com/owner/repo", "ssh://git:password@github.com/owner/repo", "https://github.com/owner/repo?token=secret", "https://github.com:443/owner/repo", "ssh://git@github.com:22/owner/repo", "git@github.com:owner/../repo", "git@github.com:owner/%2e/repo", "https://github.com./owner/repo", "https://github.com/owner/repo\n"} {
		if _, err := projectIdentity(origin); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password@") {
			t.Fatalf("unsafe origin refusal = %v", err)
		}
	}
	base := t.TempDir()
	if got, err := ResolveProjectIdentity(ctx, base); err != nil || got != (ProjectIdentity{}) {
		t.Fatalf("no Git = %+v, %v", got, err)
	}
	identityGit(t, base, "https://github.com/owner/repo")
	if err := exec.Command("git", "-C", base, "config", "--local", "--unset-all", "remote.origin.url").Run(); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveProjectIdentity(ctx, base); err != nil || got != (ProjectIdentity{}) {
		t.Fatalf("missing origin = %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(base, ".git", "config"), []byte("[broken secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProjectIdentity(ctx, base); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("malformed Git config = %v", err)
	}
	a, _ := projectIdentity("https://github.com/owner16000/repo")
	b, _ := projectIdentity("https://github.com/owner38016/repo")
	if a.ProjectID != b.ProjectID || matchProjectIdentity(a, b) == nil {
		t.Fatal("known short-hash collision was not distinguished by full canonical origin")
	}
	t.Run("Git errors are not missing origin", func(t *testing.T) {
		base := t.TempDir()
		identityGit(t, base, "https://github.com/owner/repo")
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := ResolveProjectIdentity(canceled, base); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled resolver = %v", err)
		}
		if err := exec.Command("git", "-C", base, "config", "--add", "remote.origin.url", "https://github.com/other/repo").Run(); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveProjectIdentity(ctx, base); err == nil {
			t.Fatal("multiple Git origins accepted")
		}
		t.Setenv("PATH", t.TempDir())
		if _, err := ResolveProjectIdentity(ctx, base); err == nil {
			t.Fatal("missing git executable became missing origin")
		}
	})
}

func TestProjectIdentityAdoptionPreservesPendingAndConfirmedResults(t *testing.T) {
	ctx := context.Background()
	s := openInternalTestStore(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	identityGit(t, s.paths.Base(), "git@github.com:kninetimmy/memdolt.git")
	pending, err := s.ProposeFact(ctx, Proposal{Actor: s.cfg.Actor, Rationale: "preserve pending", Target: TargetRepo}, Fact{Key: "pending", Value: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	before := transferMain(t, s)
	if got, err := s.InitializeIdentity(ctx, false); err == nil || !strings.Contains(err.Error(), "--adopt-identity") || got.Commit != "" || before != transferMain(t, s) {
		t.Fatalf("implicit adoption = %+v, %v", got, err)
	}
	if _, err := s.db.Exec("INSERT INTO tasks (id,title) VALUES ('dirty','preserve dirty')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeIdentity(ctx, true); err == nil || before != transferMain(t, s) || countInternal(t, s, "SELECT COUNT(*) FROM tasks WHERE id='dirty'") != 1 {
		t.Fatalf("dirty adoption = %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM tasks WHERE id='dirty'"); err != nil {
		t.Fatal(err)
	}
	late := errors.New("fixture finalization failure")
	got, err := s.initializeIdentity(ctx, true, func(tx *sql.Tx) error { return errors.Join(tx.Commit(), late) })
	if !errors.Is(err, late) || got.Commit == "" || got.ProjectID != "memdolt-414c4f88" || transferMain(t, s) != got.Commit {
		t.Fatalf("confirmed adoption = %+v, %v", got, err)
	}
	var author, email string
	if err := s.db.QueryRow("SELECT committer, email FROM dolt_log WHERE commit_hash = ?", got.Commit).Scan(&author, &email); err != nil || author != s.cfg.Actor.Name || email != s.cfg.Actor.Email {
		t.Fatalf("adoption author = %s %s, %v", author, email, err)
	}
	if _, err := s.ProposalDiff(ctx, pending.ID); err != nil {
		t.Fatalf("adoption changed pending proposal: %v", err)
	}
	if again, err := s.InitializeIdentity(ctx, true); err != nil || again.Commit != "" || again.ProjectIdentity != got.ProjectIdentity || transferMain(t, s) != got.Commit {
		t.Fatalf("repeat adoption = %+v, %v", again, err)
	}
	cfg := s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := New(cfg)
	if err := reopened.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if transferMain(t, reopened) != got.Commit {
		t.Fatal("reopen changed identity history")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	identityGit(t, cfg.BaseDir, "https://github.com/other/project")
	wrong, _ := New(cfg)
	if err := wrong.Open(ctx); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("changed origin = %v", err)
	}
}

func TestProjectIdentityNativeRoundTripAndMismatch(t *testing.T) {
	ctx := context.Background()
	a, _ := transferFixture(t)
	// Publish pre-adoption history first: initial identity may be explicitly
	// pushed onto that remote by the same fast-forward operation.
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	legacy := transferClone(t, a)
	identityGit(t, a.paths.Base(), "https://github.com/kninetimmy/memdolt")
	adopted, err := a.InitializeIdentity(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	legacyBefore := transferMain(t, legacy)
	if got, err := legacy.Pull(ctx, TransferOptions{}); err == nil || got.Changed || transferMain(t, legacy) != legacyBefore {
		t.Fatalf("implicit incoming adoption = %+v, %v", got, err)
	}
	b := transferClone(t, a)
	identityGit(t, b.paths.Base(), "ssh://git@github.com/kninetimmy/memdolt.git")
	transferWrite(t, a, "machine-a", "local A")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	transferWrite(t, b, "machine-b", "local B")
	merged, err := b.Pull(ctx, TransferOptions{})
	if err != nil || !merged.Changed {
		t.Fatalf("merge = %+v, %v", merged, err)
	}
	if _, err := b.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pull(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT * FROM meta ORDER BY k", "SELECT * FROM session_notes ORDER BY id", "SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY commit_hash"} {
		if cloneRows(t, a.db, query) != cloneRows(t, b.db, query) {
			t.Fatalf("round trip changed %s", query)
		}
	}
	if countInternal(t, a, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash='"+adopted.Commit+"'") != 1 {
		t.Fatal("adoption history lost")
	}
	remotes, err := a.ListRemotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrongBase := cloneScratch(t)
	identityGit(t, wrongBase, "https://github.com/other/repository")
	if got, err := Clone(ctx, Config{BaseDir: wrongBase, Actor: a.cfg.Actor}, remotes[0].URL, ""); err == nil || got.MainCommit != "" {
		t.Fatalf("wrong clone = %+v, %v", got, err)
	}
	// Simulate a foreign native writer changing identity on a descendant. No
	// memdolt push may publish it over a nonempty different remote identity.
	other, _ := projectIdentity("https://github.com/other/repository")
	if _, err := b.Commit(ctx, store.CommitRequest{Author: b.cfg.Actor, Message: "foreign identity fixture", NoText: true, Statements: []store.Statement{
		{SQL: "UPDATE meta SET v=? WHERE k='project_id'", Args: []any{other.ProjectID}},
		{SQL: "UPDATE meta SET v=? WHERE k='project_origin'", Args: []any{other.Origin}},
	}}); err != nil {
		t.Fatal(err)
	}
	identityGit(t, b.paths.Base(), "https://github.com/other/repository")
	cfg := b.cfg
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if got, err := b.Push(ctx, TransferOptions{}); err == nil || got.Status != "refused" {
		t.Fatalf("conflicting remote identity push = %+v, %v", got, err)
	}
	if got, err := a.Pull(ctx, TransferOptions{}); err != nil || got.MainCommit != merged.MainCommit {
		t.Fatalf("remote changed despite refusal = %+v, %v", got, err)
	}
}
