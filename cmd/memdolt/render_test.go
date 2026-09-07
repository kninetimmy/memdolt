package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func seedRender(t *testing.T, base string) string {
	t.Helper()
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw, err := os.ReadFile(repoFile("internal", "render", "testdata", "memory.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var statements []store.Statement
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), ";\n") {
		statements = append(statements, store.Statement{SQL: strings.TrimSuffix(text, ";")})
	}
	commit, err := st.Commit(context.Background(), store.CommitRequest{
		Statements: statements, Text: []string{string(raw)}, Author: cliActor, Message: "render fixture provenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeFact(context.Background(), localdolt.Proposal{
		Actor: memory.UserActor.CommitAuthor(), Rationale: "keep proposal private", Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "fixture.pending", Value: "NEVER_RENDER_PENDING"}); err != nil {
		t.Fatal(err)
	}
	return commit.Hash
}

func TestRenderCLICommittedMemoryDirectAndOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owner"}[owner], func(t *testing.T) {
			base := initStore(t)
			if owner {
				serveStore(t, base)
			}
			head := seedRender(t, base)
			before := runMemdolt(t, "repo", "status", "--dir", base, "--json")
			first := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			if first.Status != "written" || first.SourceCommit != head || first.SchemaVersion != store.LatestSchemaVersion() || len(first.WrittenFiles) != 2 || len(first.BackupFiles) != 0 {
				t.Fatalf("render result = %+v", first)
			}
			files := make(map[string][]byte)
			for _, path := range first.WrittenFiles {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				files[filepath.Base(path)] = raw
				for _, text := range []string{render.Marker, head, "Source schema v4"} {
					if !bytes.Contains(raw, []byte(text)) {
						t.Errorf("%s lacks %q", path, text)
					}
				}
				for _, text := range []string{"NEVER_RENDER_PENDING", "Token Accounting", "writes_log", "Earlier state"} {
					if bytes.Contains(raw, []byte(text)) {
						t.Errorf("%s includes excluded %q", path, text)
					}
				}
			}
			for name, texts := range map[string][]string{
				"PROJECT.md":        {"Building durable memory\n\nKeep this paragraph", "Local owner → committed Dolt", "by agent:codex", "Codex", "Recorded session\n\nSecond paragraph stays.", "ses_fixture", "fixture-provider", "fixture-model", "precise"},
				"PROJECT_LEDGER.md": {"Earlier choice", "Keep historical rationale.", "Historical summary", "shared writable files", "prior evidence", "Superseded by (decision ULID)", "old | command\nsecond line", "Superseded by (fact ULID)", "01ARZ3NDEKTSV4RRFFQ69G5F06", "**Stale:** true", "**Verified:** never", "render fixture provenance", "user@memdolt.invalid", "Open task", "Blocked task", "Done task"},
			} {
				for _, text := range texts {
					if !bytes.Contains(files[name], []byte(text)) {
						t.Errorf("%s lacks %q\n%s", name, text, files[name])
					}
				}
			}
			ledger := string(files["PROJECT_LEDGER.md"])
			if strings.Index(ledger, "Open task") > strings.Index(ledger, "Blocked task") || strings.Index(ledger, "Blocked task") > strings.Index(ledger, "Done task") {
				t.Fatal("backlog is not ordered by status")
			}
			second := decodeJSON[render.Result](t, runMemdolt(t, "render", "--dir", base, "--json"))
			if second.SourceCommit != head || len(second.BackupFiles) != 2 {
				t.Fatalf("repeated render = %+v", second)
			}
			for _, backup := range second.BackupFiles {
				got, err := os.ReadFile(backup)
				if err != nil {
					t.Fatal(err)
				}
				name := strings.Split(filepath.Base(backup), ".bak-")[0]
				if !bytes.Equal(got, files[name]) {
					t.Fatalf("backup does not preserve %s", name)
				}
			}
			if after := runMemdolt(t, "repo", "status", "--dir", base, "--json"); after != before {
				t.Fatalf("render changed store status\nbefore %s\nafter %s", before, after)
			}
			if human := runMemdolt(t, "render", "--dir", base); !strings.Contains(human, "written:") || !strings.Contains(human, "recoverable backup:") {
				t.Fatalf("human render = %s", human)
			}
		})
	}
}

type renderCloseFailure struct{ commandStore }

func (s renderCloseFailure) Close() error {
	return errors.Join(s.commandStore.Close(), errors.New("synthetic render store close failure"))
}

func TestRenderCLIMissingConfigOutputAndCloseErrors(t *testing.T) {
	missing := t.TempDir()
	if got := runMemdoltErr(t, "render", "--dir", missing); !strings.Contains(got, "init") {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(missing, ".memdolt")); !os.IsNotExist(err) {
		t.Fatalf("missing render initialized metadata: %v", err)
	}
	base := initStore(t)
	config := filepath.Join(base, ".memdolt", "config.toml")
	if err := os.WriteFile(config, []byte("[render]\noutput_dir = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runMemdoltErr(t, "render", "--dir", base); !strings.Contains(got, "configuration") {
		t.Fatal(got)
	}
	if err := os.WriteFile(config, []byte("[render]\noutput_dir = 'views'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := newRenderCommand()
	cmd.SetContext(context.Background())
	cmd.SetOut(&output)
	jsonOutput = true
	err = runRender(cmd, renderCloseFailure{st})
	if err == nil || !strings.Contains(err.Error(), "store close failure") {
		t.Fatalf("close error = %v", err)
	}
	got := decodeJSON[render.Result](t, output.String())
	if len(got.WrittenFiles) != 2 || got.Error == "" || filepath.Base(got.OutputDir) != "views" {
		t.Fatalf("lost confirmed outputs on close failure: %+v", got)
	}
	st, err = openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetOut(failingRenderOutput{})
	if err := runRender(cmd, st); err == nil || !strings.Contains(err.Error(), "output reporting failed") || !strings.Contains(err.Error(), "PROJECT.md") {
		t.Fatalf("output failure = %v", err)
	}
}

type failingRenderOutput struct{}

func (failingRenderOutput) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}
