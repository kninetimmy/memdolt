package localdolt

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/chunks"

	"github.com/kninetimmy/memdolt/internal/store"
)

func transferFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := openInternalTestStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(cloneScratch(t), "remote with spaces")
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(remote)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	raw := (&url.URL{Scheme: "file", Path: path}).String()
	if _, err := s.db.Exec("CALL DOLT_REMOTE('add', 'origin', ?)", raw); err != nil {
		t.Fatal(err)
	}
	return s, remote
}

func transferClone(t *testing.T, source *Store) *Store {
	t.Helper()
	var raw string
	if err := source.db.QueryRow("SELECT url FROM dolt_remotes WHERE name = 'origin'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if filepath.Separator == '\\' && !strings.HasPrefix(raw, "file:///") {
		raw = "file:///" + strings.TrimPrefix(raw, "file://")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw = parsed.String()
	cfg := source.cfg
	cfg.BaseDir = cloneScratch(t)
	if _, err := Clone(context.Background(), cfg, raw, ""); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func transferWrite(t *testing.T, s *Store, id, text string) string {
	t.Helper()
	result, err := s.Commit(context.Background(), store.CommitRequest{
		Author: s.cfg.Actor, Message: "synthetic transfer note", Text: []string{text},
		Statements: []store.Statement{{
			SQL:  "INSERT INTO session_notes (id, actor, actor_raw, text, created_at, session_id, agent_id, provider_id, model_id, variant) VALUES (?, 'agent:opencode', 'cli', ?, NOW(), 'ses_fixture', NULL, 'provider', 'model', '')",
			Args: []any{id, text},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Hash
}

func transferMain(t *testing.T, s *Store) string {
	t.Helper()
	var main string
	if err := s.db.QueryRow("SELECT DOLT_HASHOF('main')").Scan(&main); err != nil {
		t.Fatal(err)
	}
	return main
}

func TestTransferRoundTripPreservesHistoryProvenanceAndProposals(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	first := transferWrite(t, a, "first", "first note")
	pending, err := a.ProposeFact(ctx, Proposal{Rationale: "pending", Actor: a.cfg.Actor, Target: TargetRepo}, Fact{Key: "pending.fact", Value: "unreviewed"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := a.Push(ctx, TransferOptions{})
		if err != nil || got.RemoteCommit != first || got.LocalCommit != first || got.Changed != (i == 0) {
			t.Fatalf("push %d = %+v, %v", i, got, err)
		}
	}
	b := transferClone(t, a)
	if got := countInternal(t, b, "SELECT COUNT(*) FROM facts"); got != 0 {
		t.Fatal("push leaked an unreviewed fact")
	}
	if got := countInternal(t, b, "SELECT COUNT(*) FROM dolt_branches"); got != 1 {
		t.Fatal("push leaked proposal refs")
	}
	second := transferWrite(t, a, "second", "second note")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(remote, "unrelated.txt")
	if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := cloneFileTree(t, remote)
	for i := 0; i < 2; i++ {
		got, err := b.Pull(ctx, TransferOptions{})
		if err != nil || got.RemoteCommit != second || got.MainCommit != second || got.Changed != (i == 0) {
			t.Fatalf("pull %d = %+v, %v", i, got, err)
		}
	}
	if after := cloneFileTree(t, remote); !reflect.DeepEqual(before, after) {
		t.Fatal("pull changed its source")
	}
	for _, query := range []string{
		"SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY commit_hash",
		"SELECT * FROM session_notes ORDER BY id",
	} {
		if got, want := cloneRows(t, b.db, query), cloneRows(t, a.db, query); got != want {
			t.Fatalf("%s: got %s want %s", query, got, want)
		}
	}
	proposals, err := a.PendingProposals(ctx)
	if err != nil || len(proposals) != 1 || proposals[0].Commit != pending.Commit {
		t.Fatalf("pending proposal changed: %+v, %v", proposals, err)
	}
	// A third independent clone confirms the final published graph and null data.
	c := transferClone(t, a)
	if transferMain(t, c) != second || cloneRows(t, c.db, "SELECT * FROM session_notes ORDER BY id") != cloneRows(t, a.db, "SELECT * FROM session_notes ORDER BY id") {
		t.Fatal("independent clone differs")
	}
	third := transferWrite(t, b, "third", "client B")
	current, err := b.Pull(ctx, TransferOptions{})
	if err != nil || current.Changed || current.MainCommit != third {
		t.Fatalf("contained remote = %+v, %v", current, err)
	}
}

func TestTransferDivergenceAndDirtyPreservation(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	b := transferClone(t, a)
	local := transferWrite(t, b, "local", "B diverges")
	remoteMain := transferWrite(t, a, "remote", "A diverges")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	before := cloneFileTree(t, remote)
	if _, err := b.Pull(ctx, TransferOptions{}); err == nil || !strings.Contains(err.Error(), "divergent") {
		t.Fatalf("divergent pull = %v", err)
	}
	if _, err := b.Push(ctx, TransferOptions{}); err == nil || !strings.Contains(err.Error(), "inspect remote main") {
		t.Fatalf("non-ff push = %v", err)
	}
	if got := transferMain(t, b); got != local {
		t.Fatal("divergence moved local main")
	}
	if after := cloneFileTree(t, remote); !reflect.DeepEqual(before, after) {
		t.Fatal("refused transfers changed remote files")
	}
	c := transferClone(t, a)
	if transferMain(t, c) != remoteMain {
		t.Fatal("non-ff push removed remote work")
	}
	if _, err := b.db.Exec("INSERT INTO tasks (id, title, status) VALUES ('dirty', 'preserve me', 'open')"); err != nil {
		t.Fatal(err)
	}
	status := cloneRows(t, b.db, "SELECT * FROM dolt_status")
	for _, op := range []string{"push", "pull"} {
		_, err := b.transfer(ctx, op, TransferOptions{}, transferHooks{afterCapture: func() { t.Fatal("dirty transfer started") }})
		if err == nil || !strings.Contains(err.Error(), "dirty main") {
			t.Fatalf("dirty %s = %v", op, err)
		}
	}
	if cloneRows(t, b.db, "SELECT * FROM dolt_status") != status || transferMain(t, b) != local || countInternal(t, b, "SELECT COUNT(*) FROM tasks WHERE id='dirty'") != 1 {
		t.Fatal("dirty refusal changed local work")
	}
}

func TestTransferRejectsIncomingSchemaAndDenyList(t *testing.T) {
	ctx := context.Background()
	for _, change := range []string{
		"UPDATE meta SET v='999' WHERE k='schema_version'",
		"UPDATE meta SET v='invalid' WHERE k='schema_version'",
		"DELETE FROM meta WHERE k='schema_version'",
		"DROP TABLE meta",
		"ALTER TABLE session_notes DROP COLUMN model_id",
		"ALTER TABLE commands MODIFY cmdline INT",
		"CREATE TABLE unexpected (id INT)",
		"UPDATE session_notes SET text='blocked-fixture'",
		"UPDATE session_notes SET model_id='blocked-fixture'",
		"INSERT INTO proposals VALUES ('p', 'fact', 'blocked-fixture', 'user', NOW(), 'repo')",
	} {
		t.Run(change, func(t *testing.T) {
			a, remote := transferFixture(t)
			transferWrite(t, a, "note", "safe")
			if _, err := a.Push(ctx, TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			b := transferClone(t, a)
			initial := transferMain(t, b)
			if _, err := a.Commit(ctx, store.CommitRequest{Author: a.cfg.Actor, Message: "synthetic incompatible remote", NoText: true, Statements: []store.Statement{{SQL: change}}}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(b.paths.ConfigFile(), []byte("[deny_list]\npatterns = ['blocked-fixture']\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(a.paths.ConfigFile(), []byte("[deny_list]\npatterns = ['blocked-fixture']\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Push(ctx, TransferOptions{}); err == nil {
				t.Fatal("memdolt uploaded incompatible or denied snapshot")
			}
			// Native fixture upload simulates a remote that bypassed memdolt.
			if _, err := a.db.Exec("CALL DOLT_PUSH('origin', 'main')"); err != nil {
				t.Fatal(err)
			}
			before := cloneFileTree(t, remote)
			got, err := b.Pull(ctx, TransferOptions{})
			if err == nil || !strings.Contains(err.Error(), "may remain") || got.Changed || transferMain(t, b) != initial {
				t.Fatalf("unsafe pull = %+v, %v", got, err)
			}
			if after := cloneFileTree(t, remote); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed source")
			}
		})
	}
}

func TestTransferScanConfigurationAndUnchangedText(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	transferWrite(t, a, "note", "previously allowed")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	b := transferClone(t, a)
	transferWrite(t, a, "new", "safe new text")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	initial := transferMain(t, b)
	for _, invalid := range []string{"[deny_list]\npatterns = ['[']", "invalid = ["} {
		if err := os.WriteFile(b.paths.ConfigFile(), []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Pull(ctx, TransferOptions{}); err == nil || transferMain(t, b) != initial {
			t.Fatalf("invalid pull config = %v", err)
		}
	}
	if err := os.WriteFile(b.paths.ConfigFile(), []byte("[deny_list]\npatterns = ['previously allowed']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Pull(ctx, TransferOptions{}); err != nil || !got.Changed {
		t.Fatalf("pull rescanned unchanged text: %+v, %v", got, err)
	}
	before := cloneFileTree(t, remote)
	for _, config := range []string{"[deny_list]\npatterns = ['previously allowed']", "[deny_list]\npatterns = ['[']", "invalid = ["} {
		if err := os.WriteFile(b.paths.ConfigFile(), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Push(ctx, TransferOptions{}); err == nil {
			t.Fatalf("upload accepted %s", config)
		}
	}
	if after := cloneFileTree(t, remote); !reflect.DeepEqual(before, after) {
		t.Fatal("upload refusal contacted/changed remote")
	}
}

func TestTransferCapturedHashAndMutationSerialization(t *testing.T) {
	ctx := context.Background()
	a, _ := transferFixture(t)
	initial := transferMain(t, a)
	var foreign string
	got, err := a.transfer(ctx, "push", TransferOptions{}, transferHooks{afterCapture: func() {
		// A foreign session is outside proposalMu. The uploaded hash must still
		// be the captured one, despite this deterministic main advance.
		if _, err := a.db.Exec("INSERT INTO tasks (id, title) VALUES ('foreign', 'foreign')"); err != nil {
			t.Fatal(err)
		}
		if err := a.db.QueryRow("CALL DOLT_COMMIT('-A', '-m', 'foreign after capture')").Scan(&foreign); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil || got.RemoteCommit != initial || foreign == initial || transferMain(t, a) != foreign {
		t.Fatalf("captured push = %+v, foreign %s, %v", got, foreign, err)
	}
	b := transferClone(t, a)
	if transferMain(t, b) != initial {
		t.Fatal("push re-resolved main after capture")
	}
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	proposalDone := make(chan error, 1)
	got, err = b.transfer(ctx, "pull", TransferOptions{}, transferHooks{beforeMove: func() {
		if b.proposalMu.TryLock() {
			b.proposalMu.Unlock()
			t.Fatal("pull does not own mutation boundary")
		}
		go func() {
			_, err := b.Commit(ctx, store.CommitRequest{Author: b.cfg.Actor, Message: "concurrent write", Text: []string{"concurrent"}, Statements: []store.Statement{{SQL: "INSERT INTO tasks (id, title) VALUES ('concurrent', 'concurrent')"}}})
			writeDone <- err
		}()
		go func() {
			_, err := b.ProposeFact(ctx, Proposal{Rationale: "concurrent proposal", Actor: b.cfg.Actor, Target: TargetRepo}, Fact{Key: "concurrent.fact", Value: "pending"})
			proposalDone <- err
		}()
	}})
	if err != nil || got.MainCommit != foreign {
		t.Fatalf("pull with waiting writers = %+v, %v", got, err)
	}
	for _, done := range []chan error{writeDone, proposalDone} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if countInternal(t, b, "SELECT COUNT(*) FROM tasks") != 2 || countInternal(t, b, "SELECT COUNT(*) FROM facts") != 0 {
		t.Fatal("concurrent main write lost or proposal published")
	}
	proposals, err := b.PendingProposals(ctx)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("concurrent proposal lost: %+v, %v", proposals, err)
	}
}

func TestTransferPostSuccessResultAndFileRefusal(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	b := transferClone(t, a)
	newMain := transferWrite(t, a, "incoming", "incoming")
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := b.transfer(ctx, "pull", TransferOptions{}, transferHooks{afterMove: func() error { return errors.New("synthetic post-success failure") }})
	if err == nil || !got.Changed || got.MainCommit != newMain || transferMain(t, b) != newMain || !strings.Contains(err.Error(), "confirmed main") {
		t.Fatalf("post-success pull = %+v, %v", got, err)
	}
	// Removing only this test-created empty oldgen simulates a valid source the
	// native opener cannot preserve. Pull's custom reader must preserve it.
	if err := os.Remove(filepath.Join(remote, "oldgen")); err != nil {
		t.Fatal(err)
	}
	before := cloneFileTree(t, remote)
	if _, err := b.Pull(ctx, TransferOptions{}); err != nil {
		t.Fatalf("source-preserving missing oldgen pull = %v", err)
	}
	if after := cloneFileTree(t, remote); !reflect.DeepEqual(before, after) {
		t.Fatal("missing oldgen pull initialized source")
	}
}

func TestTransferRemoteAdvanceAndForeignLocalWriteRefuseSafely(t *testing.T) {
	ctx := context.Background()
	a, _ := transferFixture(t)
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	b := transferClone(t, a)
	local := transferWrite(t, a, "a", "A local")
	remote := transferWrite(t, b, "b", "B remote")
	_, err := a.transfer(ctx, "push", TransferOptions{}, transferHooks{afterCapture: func() {
		if _, err := b.Push(ctx, TransferOptions{}); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || transferMain(t, a) != local {
		t.Fatalf("concurrent remote advance = %v", err)
	}
	c := transferClone(t, b)
	if transferMain(t, c) != remote {
		t.Fatal("concurrent remote work lost")
	}
	d := transferClone(t, b)
	transferWrite(t, b, "later", "later remote")
	if _, err := b.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = d.transfer(ctx, "pull", TransferOptions{}, transferHooks{beforeMove: func() {
		if _, err := d.db.Exec("INSERT INTO tasks(id,title,status) VALUES ('foreign-dirty','keep','open')"); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || transferMain(t, d) != remote || countInternal(t, d, "SELECT COUNT(*) FROM tasks WHERE id='foreign-dirty'") != 1 {
		t.Fatalf("foreign dirty write lost: %v", err)
	}
}

func TestTransferMissingMainEmptyAndJournaledFileSources(t *testing.T) {
	ctx := context.Background()
	a, remote := transferFixture(t)
	before := cloneFileTree(t, remote)
	if _, err := a.Pull(ctx, TransferOptions{}); err == nil {
		t.Fatal("empty remote accepted")
	}
	if !reflect.DeepEqual(before, cloneFileTree(t, remote)) {
		t.Fatal("empty source initialized")
	}
	if _, err := a.db.Exec("CALL DOLT_PUSH('origin', 'main:other')"); err != nil {
		t.Fatal(err)
	}
	before = cloneFileTree(t, remote)
	if _, err := a.Pull(ctx, TransferOptions{}); err == nil {
		t.Fatal("remote without main accepted")
	}
	if !reflect.DeepEqual(before, cloneFileTree(t, remote)) {
		t.Fatal("missing main refusal changed source")
	}
	if _, err := a.Push(ctx, TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(remote, chunks.JournalFileID)
	if err := os.WriteFile(journal, []byte("synthetic unsupported journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = cloneFileTree(t, remote)
	if _, err := a.Pull(ctx, TransferOptions{}); err == nil {
		t.Fatal("journaled source accepted")
	}
	if !reflect.DeepEqual(before, cloneFileTree(t, remote)) {
		t.Fatal("journal refusal changed source")
	}
}
