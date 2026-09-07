package localdolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/render"
	"github.com/kninetimmy/memdolt/internal/store"
)

func renderStore(t *testing.T) *Store {
	t.Helper()
	s := openInternalTestStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRenderPreservesDirtyWorkingSetProposalsAndHistory(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	_, head, err := memory.New(s, memory.UserActor).SetNarrative(ctx, memory.StateNarrative, "committed narrative")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ProposeFact(ctx, Proposal{Actor: s.cfg.Actor, Rationale: "pending", Target: TargetRepo}, Fact{Key: "pending.fact", Value: "pending fact excluded"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE project_state SET body = ?", "dirty narrative excluded"); err != nil {
		t.Fatal(err)
	}
	commits := internalCount(t, s, "SELECT COUNT(*) FROM dolt_log")
	result, err := s.Render(ctx)
	if err != nil || result.SourceCommit != head {
		t.Fatalf("dirty render=%+v, %v", result, err)
	}
	raw, err := os.ReadFile(result.WrittenFiles[0])
	if err != nil || !strings.Contains(string(raw), "committed narrative") || strings.Contains(string(raw), "dirty narrative") {
		t.Fatalf("rendered dirty text: %s, %v", raw, err)
	}
	if got := internalCount(t, s, "SELECT COUNT(*) FROM dolt_log"); got != commits {
		t.Fatal("render added a memory commit")
	}
	if got := internalCount(t, s, "SELECT COUNT(*) FROM dolt_status"); got != 1 {
		t.Fatalf("render changed dirty working set: %d", got)
	}
	if diff, err := s.ProposalDiff(ctx, pending.ID); err != nil || diff.Proposal.Commit != pending.Commit {
		t.Fatalf("render changed proposal: %+v, %v", diff, err)
	}
}

type renderReadHook struct {
	*Store
	before func(string) error
	close  bool
}

func (s renderReadHook) Query(ctx context.Context, query string, args ...any) (store.Rows, error) {
	if err := s.before(query); err != nil {
		return nil, err
	}
	rows, err := s.Store.Query(ctx, query, args...)
	if err == nil && s.close {
		return renderCloseRows{Rows: rows}, nil
	}
	return rows, err
}

type renderCloseRows struct{ store.Rows }

func (r renderCloseRows) Close() error {
	return errors.Join(r.Rows.Close(), errors.New("synthetic source close error"))
}

func TestRenderPinsEveryCategoryAndHistoryToCapturedMain(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	lanes := memory.New(s, memory.UserActor)
	_, head, err := lanes.SetNarrative(ctx, memory.StateNarrative, "state before concurrent commit")
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	source := renderReadHook{Store: s, before: func(query string) error {
		if strings.Contains(query, "FROM facts AS OF") && !changed {
			changed = true
			_, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "concurrent note must not mix snapshots", Text: []string{"concurrent note", "concurrent fact"}, Statements: []store.Statement{
				{SQL: "INSERT INTO session_notes (id, text, actor, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F01', 'concurrent note', 'user', NOW())"},
				{SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F02', 'concurrent.fact', 'concurrent fact', 'user', NOW())"},
			}})
			return err
		}
		return nil
	}}
	// Run exercises the same capture/file implementation as Store.Render while
	// permitting a commit between queries, like a nonparticipating writer.
	result, err := render.Run(ctx, s.paths.Base(), source)
	if err != nil || !changed || result.SourceCommit != head || transferMain(t, s) == head {
		t.Fatalf("captured result=%+v, changed=%t, err=%v", result, changed, err)
	}
	for _, file := range result.WrittenFiles {
		raw, err := os.ReadFile(file)
		if err != nil || strings.Contains(string(raw), "concurrent note") || strings.Contains(string(raw), "concurrent fact") || !strings.Contains(string(raw), head) {
			t.Fatalf("mixed snapshot: %s, %v", raw, err)
		}
	}
}

func TestRenderReadAndSchemaFailuresDoNotReplaceOutputs(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	first, err := s.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(first.WrittenFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, failAt := range []string{"dolt_branches", "meta AS OF", "project_state AS OF", "project_arch AS OF", "session_notes AS OF", "decisions AS OF", "tasks AS OF", "facts AS OF", "DOLT_LOG", "close"} {
		t.Run(failAt, func(t *testing.T) {
			source := renderReadHook{Store: s, close: failAt == "close", before: func(query string) error {
				if strings.Contains(query, failAt) {
					return errors.New("synthetic source read error")
				}
				return nil
			}}
			got, err := render.Run(ctx, s.paths.Base(), source)
			if err == nil || !strings.Contains(err.Error(), "synthetic source") || len(got.WrittenFiles) != 0 || len(got.BackupFiles) != 0 {
				t.Fatalf("failed capture=%+v, %v", got, err)
			}
			data, err := os.ReadFile(first.WrittenFiles[0])
			if err != nil || string(data) != string(original) {
				t.Fatal("source failure replaced original output")
			}
		})
	}
	for _, schema := range []string{"1", "999", "broken"} {
		_, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "synthetic schema", NoText: true,
			Statements: []store.Statement{{SQL: "UPDATE meta SET v = ? WHERE k = 'schema_version'", Args: []any{schema}}}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Render(ctx)
		if err == nil || !strings.Contains(err.Error(), "unsupported committed schema") || len(got.WrittenFiles) != 0 {
			t.Fatalf("schema %s render=%+v, %v", schema, got, err)
		}
	}
}

func TestRenderRecentNoteLimitAndEmptyCategories(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	var statements []store.Statement
	for i := range 12 {
		statements = append(statements, store.Statement{
			SQL:  "INSERT INTO session_notes (id, text, actor, created_at) VALUES (?, ?, 'user', ?)",
			Args: []any{fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i), fmt.Sprintf("note number %02d", i), time.Now().UTC().Add(time.Duration(i) * time.Second)},
		})
	}
	if _, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "twelve notes", Text: []string{"fixture notes"}, Statements: statements}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	project, err := os.ReadFile(result.WrittenFiles[0])
	if err != nil || strings.Count(string(project), "note number") != 10 || strings.Contains(string(project), "note number 01") || !strings.Contains(string(project), "No project_state recorded") || !strings.Contains(string(project), "No project_arch recorded") {
		t.Fatalf("recent/empty project=%s, %v", project, err)
	}
	ledger, err := os.ReadFile(result.WrittenFiles[1])
	if err != nil || !strings.Contains(string(ledger), "No decisions recorded") || !strings.Contains(string(ledger), "No tasks recorded") || !strings.Contains(string(ledger), "No facts recorded") {
		t.Fatalf("empty ledger=%s, %v", ledger, err)
	}
	if _, err := os.Stat(filepath.Join(s.paths.Dir(), "backups", "rendered")); err != nil {
		t.Fatal(err)
	}
}

func TestRenderStalenessUsesPositiveInt64DayHorizon(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	if _, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "dated facts", Text: []string{"verified", "unverified"}, Statements: []store.Statement{
		{SQL: "INSERT INTO facts (id, `key`, value, verified_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F01', 'dated.verified', 'verified', '2020-01-01')"},
		{SQL: "INSERT INTO facts (id, `key`, value, verified_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F02', 'dated.unverified', 'unverified', NULL)"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.paths.ConfigFile(), []byte("[retrieval]\nfact_stale_after_days = 9223372036854775807\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := s.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := os.ReadFile(result.WrittenFiles[1])
	if err != nil || strings.Count(string(ledger), "**Stale:** false") != 1 || strings.Count(string(ledger), "**Stale:** true") != 1 {
		t.Fatalf("stale horizon overflowed or NULL verification was trusted: %s, %v", ledger, err)
	}
}
