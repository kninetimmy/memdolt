package localdolt

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

func historyFixture(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	s := openInternalTestStore(t)
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	added := statusCommit(t, s,
		store.Statement{SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES ('f1', 'history.fact', NULL, 'imported author', '2001-02-03 04:05:06')"},
		store.Statement{SQL: "INSERT INTO decisions (id, title, rationale, status, source, decided_at) VALUES ('d1', 'history decision', NULL, NULL, '', NULL)"},
		store.Statement{SQL: "INSERT INTO project_state (id, body, actor, actor_raw, created_at) VALUES ('s1', NULL, NULL, '', NULL)"},
		store.Statement{SQL: "INSERT INTO project_arch (id, body, actor, actor_raw, created_at) VALUES ('a1', 'first architecture', 'imported', NULL, '2001-02-03 04:05:06')"})
	edited := statusCommit(t, s,
		store.Statement{SQL: "UPDATE facts SET value = '', evidence = 'source pointer' WHERE id = 'f1'"},
		store.Statement{SQL: "UPDATE decisions SET rationale = '', status = 'draft' WHERE id = 'd1'"},
		store.Statement{SQL: "UPDATE project_state SET body = '' WHERE id = 's1'"},
		store.Statement{SQL: "UPDATE project_arch SET body = 'edited architecture' WHERE id = 'a1'"})
	deleted := statusCommit(t, s,
		store.Statement{SQL: "DELETE FROM facts WHERE id = 'f1'"},
		store.Statement{SQL: "DELETE FROM decisions WHERE id = 'd1'"},
		store.Statement{SQL: "DELETE FROM project_arch WHERE id = 'a1'"},
		store.Statement{SQL: "INSERT INTO project_state (id, body, created_at) VALUES ('s2', 'later status', '2020-01-01 00:00:00'), ('s3', 'current status', '2020-01-01 00:00:00')"})
	return s, added, edited, deleted
}

func TestHistoryNativeSubjectsValuesBlameAndPreservation(t *testing.T) {
	ctx := context.Background()
	s, added, edited, deleted := historyFixture(t)
	before := statusSnapshot(t, s)
	for _, subject := range []string{"fact", "decision", "state", "arch"} {
		opts := HistoryOptions{Subject: subject, Limit: 25}
		if subject == "fact" || subject == "decision" {
			opts.ID = interopString(subject[:1] + "1")
		}
		result, err := s.History(ctx, opts)
		if err != nil {
			t.Fatalf("%s history: %v", subject, err)
		}
		if result.MainCommit != deleted || result.Revision != deleted || len(result.Changes) < 3 || result.Changes[0].Commit.Hash != deleted {
			t.Fatalf("%s history = %+v", subject, result)
		}
		if subject == "state" {
			if interopValue(result.Current, "id") != "s3" || result.Blame == nil || result.Blame.Hash != deleted || len(result.Changes) != 4 || interopValue(result.Changes[0].To, "id") != "s2" || interopValue(result.Changes[1].To, "id") != "s3" {
				t.Fatalf("narrative selection/order = %+v", result)
			}
		} else if result.Current != nil || result.Blame != nil || result.Changes[0].Type != "deleted" || result.Changes[0].To != nil {
			t.Fatalf("deleted %s row = %+v", subject, result)
		}
		opts.AsOf = &edited
		past, err := s.History(ctx, opts)
		if err != nil || past.MainCommit != deleted || past.Revision != edited || past.Current == nil || past.Blame == nil || past.Blame.Hash != edited || len(past.Changes) != 2 {
			t.Fatalf("%s past = %+v, %v", subject, past, err)
		}
		field := map[string]string{"fact": "value", "decision": "rationale", "state": "body", "arch": "body"}[subject]
		if subject != "arch" && (past.Changes[0].From[field] != nil || past.Changes[0].To[field] == nil || *past.Changes[0].To[field] != "") {
			t.Fatalf("%s NULL to empty collapsed: %+v", subject, past)
		}
		if past.Changes[1].Commit.Hash != added || past.Changes[1].From != nil || past.Changes[1].Type != "added" {
			t.Fatal(past.Changes)
		}
		opts.Limit = 1
		limited, err := s.History(ctx, opts)
		if err != nil || len(limited.Changes) != 1 || !reflect.DeepEqual(limited.Changes[0], past.Changes[0]) {
			t.Fatalf("limit = %+v, %v", limited, err)
		}
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatalf("history changed native state:\n%s\n%s", before, after)
	}
	if _, _, err := memory.New(s, memory.UserActor).LogNote(ctx, "normal operation after historical blame"); err != nil {
		t.Fatalf("history contaminated later write: %v", err)
	}
}

func TestHistorySelectionCancellationAndMissingRows(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	ctx := context.Background()
	before := statusSnapshot(t, s)
	for _, opts := range []HistoryOptions{
		{Subject: "task", Limit: 25}, {Subject: "fact", Limit: 25},
		{Subject: "fact", ID: interopString(""), Limit: 25},
		{Subject: "fact", ID: interopString(string([]byte{0xff})), Limit: 25},
		{Subject: "state", ID: interopString(""), Limit: 25},
		{Subject: "arch", Limit: 0}, {Subject: "arch", Limit: -1}, {Subject: "arch", Limit: store.DefaultMaxRows + 1},
		{Subject: "arch", Limit: 25, AsOf: interopString("main")},
		{Subject: "arch", Limit: 25, AsOf: interopString("")},
		{Subject: "arch", Limit: 25, AsOf: interopString(strings.Repeat("0", 32))},
	} {
		if got, err := s.History(ctx, opts); err == nil || got.Current != nil || got.Changes != nil {
			t.Fatalf("invalid history selection = %+v: %+v %v", opts, got, err)
		}
	}
	missing, err := s.History(ctx, HistoryOptions{Subject: "fact", ID: interopString("missing"), Limit: store.DefaultMaxRows})
	if err != nil || missing.Current != nil || missing.Blame != nil || missing.Changes == nil || len(missing.Changes) != 0 {
		t.Fatalf("missing history = %+v, %v", missing, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.History(canceled, HistoryOptions{Subject: "state", Limit: 25}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled history = %v", err)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("refusal changed native state")
	}
	canceled, cancel = context.WithCancel(ctx)
	if got, err := s.history(canceled, HistoryOptions{Subject: "state", Limit: 25}, cancel); !errors.Is(err, context.Canceled) || got.Current != nil || got.Changes != nil {
		t.Fatalf("canceled after native blame: %+v, %v", got, err)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("native cancellation changed roots or refs")
	}
	if _, _, err := memory.New(s, memory.UserActor).LogNote(ctx, "normal operation after canceled native blame"); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryDirtyUnacceptedAndTaggedRevisions(t *testing.T) {
	ctx := context.Background()
	s, added, _, _ := historyFixture(t)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []store.Statement{
		{SQL: "CALL DOLT_CHECKOUT('-b', 'proposal/history-private')"},
		{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('f1', 'history.private', 'UNACCEPTED')"},
		{SQL: "INSERT INTO decisions (id, title) VALUES ('d1', 'UNACCEPTED')"},
		{SQL: "UPDATE project_state SET body = 'UNACCEPTED'"},
		{SQL: "INSERT INTO project_arch (id, body) VALUES ('a1', 'UNACCEPTED')"},
		{SQL: "CALL DOLT_COMMIT('-Am', 'unaccepted history fixture')"},
		{SQL: "CALL DOLT_TAG('history-private')"},
		{SQL: "CALL DOLT_CHECKOUT('main')"},
		{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('f1', 'history.dirty', 'DIRTY')"},
		{SQL: "INSERT INTO decisions (id, title) VALUES ('d1', 'DIRTY')"},
		{SQL: "UPDATE project_state SET body = 'STAGED'"},
		{SQL: "CALL DOLT_ADD('facts', 'project_state')"},
		{SQL: "ALTER TABLE project_arch ADD COLUMN dirty_only TEXT"},
		{SQL: "INSERT INTO project_arch (id, body) VALUES ('a1', 'DIRTY')"},
	} {
		if _, err := conn.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			t.Fatal(err)
		}
	}
	var unaccepted string
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('history-private')").Scan(&unaccepted); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := statusSnapshot(t, s)
	for _, subject := range []string{"fact", "decision", "state", "arch"} {
		opts := HistoryOptions{Subject: subject, Limit: 25}
		if subject == "fact" || subject == "decision" {
			opts.ID = interopString(subject[:1] + "1")
		}
		for _, revision := range []*string{nil, &added} {
			opts.AsOf = revision
			got, err := s.History(ctx, opts)
			if err != nil {
				t.Fatalf("dirty %s history: %v", subject, err)
			}
			for _, row := range []map[string]*string{got.Current, got.Changes[0].From, got.Changes[0].To} {
				for _, cell := range row {
					if cell != nil && (*cell == "DIRTY" || *cell == "STAGED" || *cell == "UNACCEPTED") {
						t.Fatalf("history exposed excluded %s row: %+v", subject, got)
					}
				}
			}
		}
		opts.AsOf = &unaccepted
		if got, err := s.History(ctx, opts); err == nil || got.Current != nil || got.Changes != nil || strings.Contains(err.Error(), "UNACCEPTED") {
			t.Fatalf("exposed unaccepted/tagged %s: %+v, %v", subject, got, err)
		}
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("history changed dirty/staged roots, proposal, tags or native log")
	}
}

func TestHistoryNativeAuthorCommitterAndImportedTimes(t *testing.T) {
	ctx := context.Background()
	s := openInternalTestStore(t)
	migrated, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO project_state (id, body, actor, actor_raw, created_at) VALUES ('imported', 'metadata remains separate', NULL, 'source database actor', '2001-02-03 04:05:06')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SET @@dolt_committer_name = 'Native Committer', @@dolt_committer_email = 'committer@example.invalid', @@dolt_committer_date = '2020-01-02T03:04:05Z'"); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := conn.QueryRowContext(ctx, "CALL DOLT_COMMIT('-Am', 'native imported history fixture', '--author', 'Original Author <original@example.invalid>', '--date', '2010-01-02T03:04:05Z')").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	var expected HistoryCommit
	expected.Hash = hash
	if err := conn.QueryRowContext(ctx, "SELECT author, author_email, author_date, committer, email, date, message FROM DOLT_LOG(?) WHERE commit_hash = ?", hash, hash).Scan(&expected.Author, &expected.AuthorEmail, &expected.AuthorDate, &expected.Committer, &expected.CommitterEmail, &expected.Date, &expected.Message); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := s.History(ctx, HistoryOptions{Subject: "state", Limit: 25})
	if err != nil || !reflect.DeepEqual(got.Blame, &expected) || len(got.Changes) != 1 || !reflect.DeepEqual(got.Changes[0].Commit, expected) {
		t.Fatalf("native metadata lost: %+v, %v; want %+v", got, err, expected)
	}
	if expected.Author == nil || expected.Committer == nil || *expected.Author == *expected.Committer || *expected.Author != "Original Author" || got.Current["actor"] != nil || interopValue(got.Current, "created_at") != "2001-02-03 04:05:06" || expected.AuthorDate == nil || expected.AuthorDate.Year() != 2010 {
		t.Fatalf("native/imported metadata conflated: %+v", got)
	}
	first := migrated.Applied[0].Commit
	for _, subject := range []string{"state", "arch"} {
		empty, err := s.History(ctx, HistoryOptions{Subject: subject, AsOf: &first, Limit: 25})
		if err != nil || empty.Current != nil || empty.Blame != nil || empty.Changes == nil || len(empty.Changes) != 0 {
			t.Fatalf("schema 1 empty history = %+v, %v", empty, err)
		}
	}
}

func TestHistoryUnsupportedHistoricalSchemaRefusesWithoutMigration(t *testing.T) {
	s, added, _, _ := historyFixture(t)
	bad := statusCommit(t, s, store.Statement{SQL: "ALTER TABLE project_state ADD COLUMN unsupported TEXT"})
	statusCommit(t, s, store.Statement{SQL: "ALTER TABLE project_state DROP COLUMN unsupported"})
	before := statusSnapshot(t, s)
	for _, revision := range []*string{nil, &bad} {
		got, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25, AsOf: revision})
		if err == nil || !strings.Contains(err.Error(), "unsupported history schema") || got.Current != nil || got.Changes != nil {
			t.Fatalf("unsupported history fabricated contents: %+v, %v", got, err)
		}
	}
	if _, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25, AsOf: &added}); err != nil {
		t.Fatalf("compatible older revision refused: %v", err)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("schema refusal migrated or changed state")
	}
	if _, _, err := memory.New(s, memory.UserActor).LogNote(context.Background(), "write after schema refusal"); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryNativeMergeOrderAndParentEvidence(t *testing.T) {
	s, _, edited, deleted := historyFixture(t)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []store.Statement{
		{SQL: "CALL DOLT_CHECKOUT('-b', 'history-side', ?)", Args: []any{edited}},
		{SQL: "INSERT INTO project_state (id, body) VALUES ('side', 'side ancestry')"},
		{SQL: "CALL DOLT_COMMIT('-Am', 'history side')"},
		{SQL: "CALL DOLT_CHECKOUT('main')"},
		{SQL: "CALL DOLT_MERGE('--no-ff', '-m', 'history merge', 'history-side')"},
	} {
		if _, err := conn.ExecContext(ctx, statement.SQL, statement.Args...); err != nil {
			t.Fatal(err)
		}
	}
	log, err := historyLog(ctx, conn, transferMain(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before := statusSnapshot(t, s)
	got, err := s.History(ctx, HistoryOptions{Subject: "state", Limit: 200000})
	if err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for i, entry := range log {
		positions[entry.Hash] = i
	}
	previous, mergeParents := -1, map[string]bool{}
	for _, change := range got.Changes {
		position, ok := positions[change.Commit.Hash]
		if !ok || position < previous {
			t.Fatal("changes do not follow native log order")
		}
		previous = position
		if change.Commit.Hash == got.Revision {
			mergeParents[change.Parent] = true
		}
	}
	if len(mergeParents) != 2 || !mergeParents[deleted] {
		t.Fatalf("merge parent evidence missing: %+v", got.Changes)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("merge history changed native state")
	}
}

func TestHistoryActualImportPreservesSourceMetadataWithoutInventingCommits(t *testing.T) {
	ctx := context.Background()
	s := renderStore(t)
	imported, err := s.ImportMemory(ctx, ImportMemoryOptions{File: interopTestFile(t, legacyInteropFixture(t)), FromMemhub: true})
	if err != nil {
		t.Fatal(err)
	}
	mapped := map[string]string{}
	for _, identity := range imported.IdentityMap {
		mapped[identity.Table+":"+identity.SourceID] = identity.Value
	}
	before := statusSnapshot(t, s)
	for _, selection := range []struct {
		subject, key string
	}{
		{"fact", "facts:11"}, {"fact", "facts:12"},
		{"decision", "decisions:21"}, {"decision", "decisions:22"},
		{"state", ""}, {"arch", ""},
	} {
		opts := HistoryOptions{Subject: selection.subject, Limit: 25}
		if selection.key != "" {
			id := mapped[selection.key]
			opts.ID = &id
		}
		got, err := s.History(ctx, opts)
		if err != nil || got.Current == nil || got.Blame == nil || got.Blame.Hash != imported.MainCommit {
			t.Fatalf("import history = %+v, %v", got, err)
		}
		for _, change := range got.Changes {
			if change.Type != "added" || change.From != nil || change.Commit.Hash != imported.MainCommit || change.Commit.Date == nil || change.Commit.Date.Year() < 2026 {
				t.Fatal("import fabricated historical native commits")
			}
		}
		if selection.key == "facts:11" && (interopValue(got.Current, "created_at") != "2020-02-01 01:02:03" || interopValue(got.Current, "source") != "agent:codex" || interopValue(got.Current, "superseded_by") != mapped["facts:12"]) {
			t.Fatal("imported source metadata or supersession changed")
		}
		if selection.key == "decisions:21" && (got.Current["summary"] == nil || *got.Current["summary"] != "") || selection.key == "decisions:22" && got.Current["summary"] != nil {
			t.Fatal("actual import lost NULL versus empty summary")
		}
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("history changed imported data or pending refs")
	}
}

func TestHistoryNativeTextPreservesBytesAndRefusesLossyOutput(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	body := strings.Repeat("大きい本文 🦦\r\n", 1500)
	hash := statusCommit(t, s, store.Statement{SQL: "UPDATE project_state SET body = ? WHERE id = 's3'", Args: []any{body}})
	got, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25})
	if err != nil || interopValue(got.Current, "body") != body || interopValue(got.Changes[0].To, "body") != body || got.Changes[0].Commit.Hash != hash {
		t.Fatalf("native TEXT changed bytes: %v", err)
	}
	// Some native SQL types can contain bytes which JSON would replace with
	// U+FFFD. The boundary rejects that conversion even for imported metadata.
	if err := historyText(map[string]*string{"actor_raw": interopString(string([]byte{0xff}))}); err == nil {
		t.Fatal("invalid UTF-8 was accepted for lossy JSON conversion")
	}
	statusCommit(t, s,
		store.Statement{SQL: "ALTER TABLE project_state MODIFY body BLOB"},
		store.Statement{SQL: "UPDATE project_state SET body = 0xff WHERE id = 's3'"})
	before := statusSnapshot(t, s)
	if got, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25}); err == nil || got.Current != nil || got.Changes != nil {
		t.Fatalf("unsupported native text shape returned contents: %+v, %v", got, err)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("unsupported text refusal changed native state")
	}
}

func TestHistoryNativeIncompatibleKeyWarningRefuses(t *testing.T) {
	s, added, _, _ := historyFixture(t)
	statusCommit(t, s,
		store.Statement{SQL: "ALTER TABLE project_state RENAME COLUMN id TO old_id"},
		store.Statement{SQL: "ALTER TABLE project_state ADD COLUMN id CHAR(26)"},
		store.Statement{SQL: "UPDATE project_state SET id = old_id"},
		store.Statement{SQL: "ALTER TABLE project_state DROP PRIMARY KEY"},
		store.Statement{SQL: "ALTER TABLE project_state ADD PRIMARY KEY (id)"},
		store.Statement{SQL: "ALTER TABLE project_state DROP COLUMN old_id"})
	// No current row means blame cannot catch the incompatible key; the
	// native diff warning must refuse instead of losing the earlier changes.
	statusCommit(t, s, store.Statement{SQL: "DELETE FROM project_state"})
	before := statusSnapshot(t, s)
	got, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25})
	if err == nil || !strings.Contains(err.Error(), "diff warning") || got.Current != nil || got.Changes != nil {
		t.Fatalf("native primary-key warning silently lost changes: %+v, %v", got, err)
	}
	if _, err := s.History(context.Background(), HistoryOptions{Subject: "state", Limit: 25, AsOf: &added}); err != nil {
		t.Fatal(err)
	}
	if after := statusSnapshot(t, s); after != before {
		t.Fatal("incompatible-key refusal changed native state")
	}
}
