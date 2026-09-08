package localdolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

func globalStoreFixture(t *testing.T) (*Store, *Store) {
	t.Helper()
	home := interopTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo, _ := documentFixture(t)
	if _, err := SetGlobalEnabled(repo.paths.Base(), true); err != nil {
		t.Fatal(err)
	}
	paths, err := GlobalPaths()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := New(Config{BaseDir: paths.Base(), Actor: memory.UserActor.CommitAuthor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}
	global, err := OpenGlobal(context.Background(), repo.paths.Base())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := global.Close(); err != nil {
			t.Error(err)
		}
	})
	return repo, global
}

func TestGlobalPolicyCredentialAliasesAndLateDocumentFailure(t *testing.T) {
	repo, global := globalStoreFixture(t)
	ctx := context.Background()
	for _, paths := range []string{repo.paths.PidFile(), global.paths.PidFile()} {
		writeDocumentFixture(t, paths, "synthetic owner capability must not be ingested")
		alias := filepath.Join(interopTempDir(t), "credential.md")
		if err := os.Link(paths, alias); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{paths, alias} {
			if result, err := global.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor}); err == nil || result.Commit != "" {
				t.Fatalf("credential source admitted: %+v %v", result, err)
			}
		}
	}
	file := filepath.Join(interopTempDir(t), "shared.md")
	writeDocumentFixture(t, file, "# shared\n\nreal committed content\n")
	late := errors.New("synthetic config-finalization failure")
	result, err := global.docAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor}, func(*os.Root) (bool, error) { return false, late })
	if !errors.Is(err, late) || result.Commit == "" || result.Document == nil || len(result.Chunks) != 1 {
		t.Fatalf("late global document progress lost: %+v %v", result, err)
	}
	if got, err := global.DocShow(ctx, result.Document.ID); err != nil || got.Document.ContentHash != result.Document.ContentHash {
		t.Fatalf("late result disagrees with durable global document: %+v %v", got, err)
	}
	writeDocumentFixture(t, repo.paths.ConfigFile(), "[global]\nenabled=true\n[deny_list]\npatterns=['denybeacon']\n")
	if result, err := global.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "deny.key", Value: "denybeacon"}, Source: "user", Actor: memory.UserActor}); !errors.Is(err, store.ErrDenied) || result.Commit != "" {
		t.Fatalf("calling repository deny-list bypass: %+v %v", result, err)
	}
	if _, err := SetGlobalEnabled(repo.paths.Base(), false); err != nil {
		t.Fatal(err)
	}
	if result, err := global.DocRemove(ctx, result.Document.ID, memory.UserActor); err == nil || result.Commit != "" {
		t.Fatal("already-open global writer ignored disable")
	}
	if result, err := global.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "disabled.key", Value: "plain"}, Source: "user", Actor: memory.UserActor}); err == nil || result.Commit != "" {
		t.Fatal("already-open human writer ignored disable")
	}
}

func TestGlobalManagedPathsAndInvalidStores(t *testing.T) {
	for _, home := range []string{"relative", `\\server\share`, "//server/share"} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			if _, err := GlobalPaths(); err == nil {
				t.Fatal("unsafe global home accepted")
			}
		})
	}
	t.Run("linked-managed-directory", func(t *testing.T) {
		home := interopTempDir(t)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if err := os.Mkdir(filepath.Join(home, ".memdolt"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(interopTempDir(t), filepath.Join(home, ".memdolt", "global")); err != nil {
			t.Skipf("host does not permit symlink fixture: %v", err)
		}
		if _, err := GlobalPaths(); err == nil {
			t.Fatal("linked global directory accepted")
		}
	})
	t.Run("home-alias-to-ambiguous-path", func(t *testing.T) {
		root := interopTempDir(t)
		target, alias := filepath.Join(root, "percent%home"), filepath.Join(root, "home-alias")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, alias); err != nil {
			t.Skipf("host does not permit symlink fixture: %v", err)
		}
		t.Setenv("HOME", alias)
		t.Setenv("USERPROFILE", alias)
		if _, err := GlobalPaths(); err == nil {
			t.Fatal("home alias bypassed canonical path validation")
		}
	})
	for _, version := range []int{store.LatestSchemaVersion() - 1, store.LatestSchemaVersion() + 1} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			repo, global := globalStoreFixture(t)
			if _, err := global.Commit(context.Background(), store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "schema refusal fixture", Statements: []store.Statement{
				{SQL: "UPDATE meta SET v = ? WHERE k = 'schema_version'", Args: []any{strconv.Itoa(version)}},
			}}); err != nil {
				t.Fatal(err)
			}
			if err := global.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenGlobal(context.Background(), repo.paths.Base())
			if err == nil || opened != nil {
				t.Fatalf("unsupported schema opened: %v", err)
			}
			if version > store.LatestSchemaVersion() && !errors.Is(err, store.ErrSchemaTooNew) {
				t.Fatalf("newer schema lost typed refusal: %v", err)
			}
		})
	}
	t.Run("corrupt-manifest", func(t *testing.T) {
		repo, global := globalStoreFixture(t)
		if err := global.Close(); err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(global.paths.DoltDataDir(), DatabaseName, ".dolt", "noms", "manifest")
		writeDocumentFixture(t, manifest, "corrupt synthetic manifest\n")
		opened, err := OpenGlobal(context.Background(), repo.paths.Base())
		if err == nil || opened != nil {
			t.Fatal("corrupt replica opened")
		}
		data, err := os.ReadFile(manifest)
		if err != nil || string(data) != "corrupt synthetic manifest\n" {
			t.Fatalf("corrupt replica was implicitly replaced: %s %v", data, err)
		}
	})
}

func TestGlobalPromotionRefusesPendingSupersededAndDirtyDestination(t *testing.T) {
	if _, err := (&Store{}).CapturePromotion(context.Background(), "fact", ""); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("blank promotion operand was not refused before store access: %v", err)
	}
	repo, global := globalStoreFixture(t)
	ctx := context.Background()
	agent, err := memory.NormalizeActor("codex")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repo.ProposeFact(ctx, Proposal{Actor: agent.CommitAuthor(), Target: TargetGlobal, Rationale: "global remains reviewed later"}, Fact{Key: "pending.global", Value: "unaccepted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CapturePromotion(ctx, "fact", pending.RowID); err == nil {
		t.Fatal("promotion read proposal data")
	}
	if _, err := repo.AcceptProposal(ctx, pending.ID, memory.UserActor.CommitAuthor(), AcceptOptions{}); err == nil || !strings.Contains(err.Error(), "global") {
		t.Fatalf("global proposal became accepted: %v", err)
	}
	fact, err := repo.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "promote.live", Value: "committed"}, Source: "user", Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	record, err := repo.CapturePromotion(ctx, "fact", fact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := global.db.ExecContext(ctx, "INSERT INTO meta(k, v) VALUES ('synthetic_dirty', 'yes')"); err != nil {
		t.Fatal(err)
	}
	if result, err := global.PromoteGlobal(ctx, record, memory.UserActor); err == nil || result.Commit != "" {
		t.Fatalf("dirty global destination accepted promotion: %+v %v", result, err)
	}
}
