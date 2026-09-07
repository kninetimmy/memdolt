package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

func statusSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var snapshot string
	for _, query := range []string{
		"SELECT name, hash FROM dolt_branches ORDER BY name",
		"SELECT * FROM dolt_tags ORDER BY tag_name",
		"SELECT name, hash FROM dolt_remote_branches WHERE name <> 'remotes/origin/main' ORDER BY name",
		"SELECT DOLT_HASHOF_DB('WORKING'), DOLT_HASHOF_DB('STAGED'), DOLT_HASHOF_DB('HEAD')",
		"SELECT * FROM dolt_status ORDER BY table_name, staged, status",
		"SELECT * FROM dolt_merge_status",
		"SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY commit_hash",
	} {
		snapshot += cloneRows(t, s.db, query)
	}
	return snapshot
}

func statusPreserved(t *testing.T, s *Store, remote string, opts RepoStatusOptions, want string) RepoStatusReport {
	t.Helper()
	before := statusSnapshot(t, s)
	source := cloneFileTree(t, remote)
	report, err := s.RepoStatus(context.Background(), opts)
	if err != nil || report.Status != want {
		t.Fatalf("status = %+v, %v; want %s", report, err, want)
	}
	if got := statusSnapshot(t, s); got != before {
		t.Fatalf("status changed refs/history/roots/merge state:\nbefore %s\nafter %s", before, got)
	}
	if after := cloneFileTree(t, remote); !reflect.DeepEqual(source, after) {
		t.Fatal("status changed the file source")
	}
	return report
}

func statusCommit(t *testing.T, s *Store, statements ...store.Statement) string {
	t.Helper()
	result, err := s.Commit(context.Background(), store.CommitRequest{
		Statements: statements, NoText: true, Message: "status fixture", Author: s.cfg.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Hash
}

func statusPush(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Push(context.Background(), TransferOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoStatusOfflineNoRemoteAndSelection(t *testing.T) {
	ctx := context.Background()
	s := openInternalTestStore(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := statusSnapshot(t, s)
	for _, opts := range []RepoStatusOptions{{}, {Diff: true}, {Local: true}} {
		report, err := s.RepoStatus(ctx, opts)
		want := "no-remote"
		if opts.Local {
			want = "offline"
		}
		if err != nil || report.Status != want || !report.LocalOnly || !report.Clean || report.Diff != nil || report.RemoteCommit != "" {
			t.Fatalf("status = %+v, %v", report, err)
		}
		if !opts.Local && !strings.Contains(report.Remedy, "remote add") {
			t.Fatal("no-remote lacks setup remedy")
		}
	}
	for _, opts := range []RepoStatusOptions{{Remote: "origin"}, {Remote: "missing"}, {Remote: "--force"}, {Remote: "' OR 1=1"}, {Remote: "origin/main"}, {Local: true, Remote: "origin"}, {Local: true, Diff: true}, {Local: true, User: "user"}, {User: "--force"}} {
		if report, err := s.RepoStatus(ctx, opts); err == nil || report.Diff != nil {
			t.Fatalf("accepted invalid selection: %+v, %v", report, err)
		}
	}
	if statusSnapshot(t, s) != before {
		t.Fatal("offline/refused status changed local state")
	}
	// No server exists here. Offline must ignore even unsafe native configuration.
	if _, err := s.AddRemote(ctx, Remote{Name: "origin", URL: "http://127.0.0.1:1/db"}); err != nil {
		t.Fatal(err)
	}
	s = transferConfiguredRemote(t, s, "http://synthetic:secret@127.0.0.1:1/db", "{}")
	if report, err := s.RepoStatus(ctx, RepoStatusOptions{Local: true}); err != nil || report.Status != "offline" {
		t.Fatalf("offline = %+v, %v", report, err)
	}
	if _, err := s.RepoStatus(ctx, RepoStatusOptions{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe configuration = %v", err)
	}
}

func TestRepoStatusAncestryAndExactNullableDiff(t *testing.T) {
	a, remote := transferFixture(t)
	initial := statusCommit(t, a,
		store.Statement{SQL: "INSERT INTO tasks (id, title, notes) VALUES ('modified', 'before', NULL), ('deleted', 'gone', '')"})
	statusPush(t, a)
	b := transferClone(t, a)
	current := statusPreserved(t, b, remote, RepoStatusOptions{Diff: true}, "current")
	if current.MainCommit != initial || current.RemoteCommit != initial || current.Diff == nil || len(current.Diff.Tables) != 0 {
		t.Fatalf("current = %+v", current)
	}
	remoteHead := statusCommit(t, a,
		store.Statement{SQL: "UPDATE tasks SET notes = '', title = NULL WHERE id = 'modified'"},
		store.Statement{SQL: "DELETE FROM tasks WHERE id = 'deleted'"},
		store.Statement{SQL: "INSERT INTO tasks (id, title) VALUES ('added', 'remote row')"})
	statusPush(t, a)
	behind := statusPreserved(t, b, remote, RepoStatusOptions{Diff: true}, "behind")
	if behind.MainCommit != initial || behind.RemoteCommit != remoteHead || behind.MergeBase != initial || behind.Diff.Direction != "local-to-remote" || behind.Diff.FromCommit != initial || behind.Diff.ToCommit != remoteHead {
		t.Fatalf("captured diff hashes = %+v", behind)
	}
	if len(behind.Diff.Tables) != 1 || behind.Diff.Tables[0].Table != "tasks" || len(behind.Diff.Tables[0].Rows) != 3 {
		t.Fatalf("diff = %+v", behind.Diff)
	}
	rows := behind.Diff.Tables[0].Rows
	if rows[0].Type != "added" || rows[0].From != nil || *rows[0].To["id"] != "added" ||
		rows[1].Type != "deleted" || rows[1].To != nil || *rows[1].From["id"] != "deleted" ||
		rows[2].Type != "modified" || *rows[2].From["title"] != "before" || rows[2].To["title"] != nil ||
		rows[2].From["notes"] != nil || *rows[2].To["notes"] != "" {
		t.Fatalf("nullable images/classifications = %+v", rows)
	}
	if len(rows[2].From) != 6 || len(rows[2].To) != 6 {
		t.Fatal("a NULL column was omitted instead of retained")
	}
	if ordinary := statusPreserved(t, b, remote, RepoStatusOptions{}, "behind"); ordinary.Diff != nil {
		t.Fatal("ordinary status exposed row bodies")
	}
	aheadHead := transferWrite(t, a, "ahead", "not yet published")
	ahead := statusPreserved(t, a, remote, RepoStatusOptions{}, "ahead")
	if ahead.MainCommit != aheadHead || ahead.RemoteCommit != remoteHead {
		t.Fatalf("ahead = %+v", ahead)
	}
	local := transferWrite(t, b, "independent", "local main")
	diverged := statusPreserved(t, b, remote, RepoStatusOptions{Diff: true}, "diverged-mergeable")
	if diverged.MainCommit != local || diverged.RemoteCommit != remoteHead || diverged.Assessment != "mergeable" {
		t.Fatalf("divergence = %+v", diverged)
	}
}

func TestRepoStatusMergeConflictsAndSatisfiedSupersession(t *testing.T) {
	for _, kind := range []string{"cell", "live key", "supersession", "document path"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			a, remote := transferFixture(t)
			statusCommit(t, a, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('base', 'shared.key', 'base')"})
			statusPush(t, a)
			b := transferClone(t, a)
			want := "conflicted"
			switch kind {
			case "cell":
				statusCommit(t, a, store.Statement{SQL: "UPDATE facts SET value = 'remote cell' WHERE id = 'base'"})
				statusCommit(t, b, store.Statement{SQL: "UPDATE facts SET value = 'local cell' WHERE id = 'base'"})
			case "live key":
				statusCommit(t, a, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('remote', 'collision.key', 'remote row')"})
				statusCommit(t, b, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('local', 'collision.key', 'local row')"})
			case "document path":
				statusCommit(t, a, store.Statement{SQL: "INSERT INTO documents (id, path) VALUES ('remote', 'shared.md')"})
				statusCommit(t, b, store.Statement{SQL: "INSERT INTO documents (id, path) VALUES ('local', 'shared.md')"})
			case "supersession":
				// The same link-first/replacement-second shape as accepted supersede.
				statusCommit(t, a,
					store.Statement{SQL: "UPDATE facts SET superseded_by = 'replacement' WHERE id = 'base'"},
					store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('replacement', 'shared.key', 'replacement')"})
				transferWrite(t, b, "independent", "independent note")
				want = "diverged-mergeable"
			}
			statusPush(t, a)
			if _, err := b.ProposeFact(ctx, Proposal{Rationale: "must survive preview", Actor: b.cfg.Actor, Target: TargetGlobal}, Fact{Key: "pending.key", Value: "unreviewed"}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.Exec("CALL DOLT_TAG('preview-local-tag', 'main')"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				report := statusPreserved(t, b, remote, RepoStatusOptions{}, want)
				if report.PendingProposals.Global != 1 {
					t.Fatalf("proposal count = %+v", report.PendingProposals)
				}
				if kind == "cell" && (len(report.Conflicts) != 1 || report.Conflicts[0].Data != 1) {
					t.Fatalf("cell conflict = %+v", report.Conflicts)
				}
				if (kind == "live key" || kind == "document path") && (len(report.Conflicts) != 1 || report.Conflicts[0].Constraints != 2) {
					t.Fatalf("unique collision = %+v", report.Conflicts)
				}
			}
		})
	}
}

func TestRepoStatusDirtyStagedArtifactsAndCommittedDiff(t *testing.T) {
	a, remote := transferFixture(t)
	statusPush(t, a)
	b := transferClone(t, a)
	local := transferWrite(t, b, "local", "local committed body")
	remoteHead := transferWrite(t, a, "remote", "remote committed body")
	statusPush(t, a)
	if _, err := b.ProposeFact(context.Background(), Proposal{Actor: b.cfg.Actor, Rationale: "uncommitted isolation", Target: TargetRepo}, Fact{Key: "pending.fact", Value: "secret proposal body"}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE session_notes SET text = 'staged body' WHERE id = 'local'",
		"CALL DOLT_ADD('session_notes')",
		"UPDATE session_notes SET text = 'dirty body' WHERE id = 'local'",
		"ALTER TABLE tasks DROP COLUMN notes",
	} {
		if _, err := b.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := map[string]os.FileInfo{}
	for _, path := range []string{b.paths.ConfigFile(), b.paths.EmbeddingsFile(), filepath.Join(b.paths.Base(), ".memdolt", "rendered", "PROJECT.md")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# preserve existing local artifact\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[path] = info
	}
	report := statusPreserved(t, b, remote, RepoStatusOptions{Diff: true}, "diverged-unassessed")
	if report.MainCommit != local || report.RemoteCommit != remoteHead || report.Clean || report.Assessment != "unassessed" || !strings.Contains(report.Remedy, "clean main") || len(report.Changes) != 3 {
		t.Fatalf("dirty status = %+v", report)
	}
	if len(report.Diff.Tables) != 1 || report.Diff.Tables[0].Table != "session_notes" || *report.Diff.Tables[0].Rows[0].From["text"] != "local committed body" {
		t.Fatalf("dirty/proposal/schema data leaked into committed diff: %+v", report.Diff)
	}
	for path, before := range artifacts {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "# preserve existing local artifact\n" {
			t.Fatalf("read preserved artifact %s: %v", path, err)
		}
		after, err := os.Stat(path)
		if err != nil || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("status changed local artifact %s", path)
		}
	}
}

func TestRepoStatusPreviewFailureCancellationAndRollback(t *testing.T) {
	a, remote := transferFixture(t)
	statusPush(t, a)
	b := transferClone(t, a)
	transferWrite(t, b, "local", "local")
	transferWrite(t, a, "remote", "remote")
	statusPush(t, a)
	for _, failure := range []string{"read", "rollback", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("synthetic preview failure")
			hooks := repoStatusHooks{}
			switch failure {
			case "read":
				hooks.afterMerge = func() error { return injected }
			case "rollback":
				hooks.rollback = func(tx *sql.Tx) error { return errors.Join(tx.Rollback(), injected) }
			case "cancel":
				hooks.afterMerge = func() error { cancel(); return nil }
			}
			before, source := statusSnapshot(t, b), cloneFileTree(t, remote)
			report, err := b.repoStatus(ctx, RepoStatusOptions{Diff: true}, hooks)
			if err == nil || report.Status != "refused" || report.Diff != nil || (failure != "cancel" && !errors.Is(err, injected)) {
				t.Fatalf("preview failure = %+v, %v", report, err)
			}
			if statusSnapshot(t, b) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
				t.Fatal("failed preview changed durable refs or roots/source")
			}
		})
	}
}

func TestRepoStatusSnapshotSerializesParticipatingWrites(t *testing.T) {
	ctx := context.Background()
	a, _ := transferFixture(t)
	statusPush(t, a)
	b := transferClone(t, a)
	local := transferWrite(t, b, "local", "local")
	transferWrite(t, a, "remote", "remote")
	statusPush(t, a)
	writeDone, proposalDone, transferDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	got, err := b.repoStatus(ctx, RepoStatusOptions{Diff: true}, repoStatusHooks{afterMerge: func() error {
		if b.proposalMu.TryLock() {
			b.proposalMu.Unlock()
			return errors.New("preview does not own the mutation mutex")
		}
		go func() {
			_, err := b.Commit(ctx, store.CommitRequest{Author: b.cfg.Actor, Message: "waiting write", NoText: true, Statements: []store.Statement{{SQL: "INSERT INTO tasks (id, title) VALUES ('waiting', 'durable after preview')"}}})
			writeDone <- err
		}()
		go func() {
			_, err := b.ProposeFact(ctx, Proposal{Actor: b.cfg.Actor, Rationale: "waiting proposal", Target: TargetRepo}, Fact{Key: "waiting.key", Value: "pending"})
			proposalDone <- err
		}()
		go func() { _, err := b.Pull(ctx, TransferOptions{}); transferDone <- err }()
		return nil
	}})
	if err != nil || got.MainCommit != local || got.Status != "diverged-mergeable" || got.PendingProposals.Repo != 0 {
		t.Fatalf("captured snapshot = %+v, %v", got, err)
	}
	for _, done := range []chan error{writeDone, proposalDone} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-transferDone; err != nil {
		t.Fatalf("waiting pull = %v", err)
	}
	if countInternal(t, b, "SELECT COUNT(*) FROM tasks WHERE id = 'waiting'") != 1 || transferMain(t, b) == local {
		t.Fatal("completed write was lost")
	}
	proposals, err := b.PendingProposals(ctx)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("completed proposal was lost: %+v, %v", proposals, err)
	}
}

func TestRepoStatusMalformedIncomingSchemaPreservesState(t *testing.T) {
	for _, statement := range []string{
		"ALTER TABLE tasks DROP COLUMN notes",
		"ALTER TABLE facts DROP INDEX uk_fact_live_key",
		"ALTER TABLE tasks ADD CONSTRAINT unknown_check CHECK (title <> '')",
		"UPDATE meta SET v = '999' WHERE k = 'schema_version'",
		"CREATE TABLE unknown_table (id INT PRIMARY KEY)",
	} {
		t.Run(statement, func(t *testing.T) {
			a, remote := transferFixture(t)
			statusPush(t, a)
			b := transferClone(t, a)
			if _, err := a.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec("CALL DOLT_COMMIT('-A', '-m', 'foreign incompatible schema')"); err != nil {
				t.Fatal(err)
			}
			// A foreign client bypasses memdolt's guarded upload.
			if _, err := a.db.Exec("CALL DOLT_PUSH('origin', 'main')"); err != nil {
				t.Fatal(err)
			}
			before, source := statusSnapshot(t, b), cloneFileTree(t, remote)
			report, err := b.RepoStatus(context.Background(), RepoStatusOptions{Diff: true})
			if err == nil || !strings.Contains(err.Error(), "incompatible incoming") || report.Diff != nil || report.Status == "current" {
				t.Fatalf("malformed candidate = %+v, %v", report, err)
			}
			if statusSnapshot(t, b) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
				t.Fatal("schema refusal changed refs, working/staged roots or source")
			}
		})
	}
}

func TestRepoStatusMissingMainSchemaDivergenceAndUnknownAncestry(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	before, source := statusSnapshot(t, a), cloneFileTree(t, remote)
	if _, err := a.RepoStatus(ctx, RepoStatusOptions{}); err == nil {
		t.Fatal("empty file source reported current or no-remote")
	}
	if statusSnapshot(t, a) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
		t.Fatal("empty source was initialized or local state changed")
	}
	if _, err := a.db.Exec("CALL DOLT_PUSH('origin', 'main:other')"); err != nil {
		t.Fatal(err)
	}
	before, source = statusSnapshot(t, a), cloneFileTree(t, remote)
	if _, err := a.RepoStatus(ctx, RepoStatusOptions{}); err == nil || !strings.Contains(err.Error(), "main") {
		t.Fatalf("missing main = %v", err)
	}
	if statusSnapshot(t, a) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
		t.Fatal("missing-main refusal changed source or local state")
	}
	statusPush(t, a)
	b := transferClone(t, a)
	transferWrite(t, b, "local", "independent")
	statusCommit(t, a, store.Statement{SQL: "ALTER TABLE tasks ALTER COLUMN notes SET DEFAULT 'different'"})
	ahead := statusPreserved(t, a, remote, RepoStatusOptions{Diff: true}, "ahead")
	if len(ahead.Diff.Tables) != 1 || ahead.Diff.Tables[0].Table != "tasks" ||
		len(ahead.Diff.Tables[0].Rows) != 0 || ahead.Diff.Tables[0].FromSchema == "" || ahead.Diff.Tables[0].ToSchema == "" ||
		ahead.Diff.Tables[0].FromSchema == ahead.Diff.Tables[0].ToSchema {
		t.Fatalf("exact table-definition diff = %+v", ahead.Diff)
	}
	statusPush(t, a)
	before, source = statusSnapshot(t, b), cloneFileTree(t, remote)
	if report, err := b.RepoStatus(ctx, RepoStatusOptions{Diff: true}); err == nil || report.Diff != nil || !strings.Contains(err.Error(), "schema changes") {
		t.Fatalf("schema divergence = %+v, %v", report, err)
	}
	if statusSnapshot(t, b) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
		t.Fatal("schema-merge refusal changed source or local state")
	}
	c := openInternalTestStore(t)
	if _, err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	remotes, err := a.ListRemotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddRemote(ctx, remotes[0]); err != nil {
		t.Fatal(err)
	}
	before, source = statusSnapshot(t, c), cloneFileTree(t, remote)
	if report, err := c.RepoStatus(ctx, RepoStatusOptions{}); err == nil || !strings.Contains(err.Error(), "ancestry") || report.Status != "refused" {
		t.Fatalf("unrelated histories = %+v, %v", report, err)
	}
	if statusSnapshot(t, c) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
		t.Fatal("ancestry refusal changed source or local state")
	}
}
