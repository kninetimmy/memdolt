package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

func pullFixture(t *testing.T, kind string) (*Store, *Store, string) {
	t.Helper()
	a, remote := transferFixture(t)
	statusCommit(t, a,
		store.Statement{SQL: "INSERT INTO meta (k, v) VALUES ('pull_fixture', 'base')"},
		store.Statement{SQL: "INSERT INTO facts (id, `key`, value, created_at) VALUES ('base', 'shared.key', 'base', '2026-01-01 00:00:00')"},
		store.Statement{SQL: "INSERT INTO tasks (id, title, status, created_at, updated_at) VALUES ('task', 'original', 'open', '2026-01-01 00:00:00', '2026-01-01 00:00:00')"})
	statusPush(t, a)
	b := transferClone(t, a)
	switch kind {
	case "value":
		statusCommit(t, a, store.Statement{SQL: "UPDATE facts SET value = 'theirs' WHERE id = 'base'"})
		statusCommit(t, b, store.Statement{SQL: "UPDATE facts SET value = 'ours' WHERE id = 'base'"})
	case "key":
		statusCommit(t, a, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('theirs', 'collision.key', 'theirs')"})
		statusCommit(t, b, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('ours', 'collision.key', 'ours')"})
	case "satisfied":
		statusCommit(t, a,
			store.Statement{SQL: "UPDATE facts SET superseded_by = 'replacement' WHERE id = 'base'"},
			store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('replacement', 'shared.key', 'replacement')"})
		statusCommit(t, b, store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('local', 'other.key', 'independent')"})
	case "task":
		statusCommit(t, a, store.Statement{SQL: "UPDATE tasks SET title = 'edited', updated_at = '2026-01-03 00:00:00' WHERE id = 'task'"})
		statusCommit(t, b, store.Statement{SQL: "UPDATE tasks SET status = 'done', updated_at = '2026-01-02 00:00:00' WHERE id = 'task'"})
	case "independent":
		transferWrite(t, a, "remote", "remote note")
		transferWrite(t, b, "local", "local note")
	default:
		t.Fatal("unknown synthetic pull fixture")
	}
	statusPush(t, a)
	return a, b, remote
}

func TestPullMergeIndependentAndSatisfiedHistory(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"independent", "satisfied"} {
		t.Run(kind, func(t *testing.T) {
			a, b, remote := pullFixture(t, kind)
			local, incoming := transferMain(t, b), transferMain(t, a)
			before := cloneFileTree(t, remote)
			pending, err := b.ProposeFact(ctx, Proposal{Rationale: "preserve", Actor: b.cfg.Actor, Target: TargetGlobal}, Fact{Key: "pending.key", Value: "pending"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.db.Exec("CALL DOLT_TAG('preserve', 'main')"); err != nil {
				t.Fatal(err)
			}
			tags := cloneRows(t, b.db, "SELECT * FROM dolt_tags")
			author := store.Actor{Name: "agent:pull-test", Email: "agent-pull-test@memdolt.invalid"}
			result, err := b.Pull(ctx, TransferOptions{Author: author})
			if err != nil || result.Status != "changed" || !result.Changed || result.LocalCommit != local || result.RemoteCommit != incoming || result.MainCommit == incoming || result.MainCommit == local {
				t.Fatalf("merge = %+v, %v", result, err)
			}
			for _, parent := range []string{local, incoming} {
				var base string
				if err := b.db.QueryRow("SELECT DOLT_MERGE_BASE(?, ?)", result.MainCommit, parent).Scan(&base); err != nil || base != parent {
					t.Fatalf("parent lost: %s %v", base, err)
				}
			}
			var parents, committer, email string
			if err := b.db.QueryRow("SELECT parents, committer, email FROM DOLT_LOG('--parents') WHERE commit_hash = ?", result.MainCommit).Scan(&parents, &committer, &email); err != nil {
				t.Fatal(err)
			}
			if parents != local+", "+incoming && parents != local+","+incoming && parents != local+" "+incoming {
				t.Fatalf("parents = %q", parents)
			}
			if committer != author.Name || email != author.Email {
				t.Fatalf("author = %s <%s>", committer, email)
			}
			if kind == "satisfied" && (len(result.Cleared) != 1 || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE id = 'base' AND superseded_by = 'replacement'") != 1) {
				t.Fatalf("satisfied records = %+v", result)
			}
			proposals, err := b.PendingProposals(ctx)
			if err != nil || len(proposals) != 1 || proposals[0].Commit != pending.Commit || cloneRows(t, b.db, "SELECT * FROM dolt_tags") != tags {
				t.Fatalf("refs changed: %+v %v", proposals, err)
			}
			if !reflect.DeepEqual(before, cloneFileTree(t, remote)) {
				t.Fatal("merge changed file source")
			}
			if countInternal(t, b, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatal("merge left working changes")
			}
		})
	}
}

func TestPullConflictRowsAndAtomicResolution(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"value", "key", "task"} {
		t.Run(kind, func(t *testing.T) {
			_, b, remote := pullFixture(t, kind)
			before, source := statusSnapshot(t, b), cloneFileTree(t, remote)
			result, err := b.Pull(ctx, TransferOptions{})
			if err != nil || result.Status != "conflicted" || result.Changed || len(result.Conflicts) != 1 || result.Remedy == "" {
				t.Fatalf("conflict = %+v, %v", result, err)
			}
			if statusSnapshot(t, b) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
				t.Fatal("conflict inspection changed roots, refs or source")
			}
			conflict := result.Conflicts[0]
			for _, row := range conflict.Rows {
				if row.Ours != nil && (row.OursBlame == nil || row.OursBlame.Author == "" || row.OursBlame.Message == "") || row.Theirs != nil && (row.TheirsBlame == nil || row.TheirsBlame.Author == "") {
					t.Fatalf("missing real blame: %+v", row)
				}
				if row.Ours != nil && row.OursBlame.Commit != result.LocalCommit || row.Theirs != nil && row.TheirsBlame.Commit != result.RemoteCommit {
					t.Fatalf("blame is not the actual changing commit: %+v", row)
				}
			}
			resolution := &PullResolution{LocalCommit: result.LocalCommit, RemoteCommit: result.RemoteCommit}
			if _, err := b.Pull(ctx, TransferOptions{Resolution: resolution}); err == nil || statusSnapshot(t, b) != before {
				t.Fatalf("incomplete resolution = %v", err)
			}
			choice := PullChoice{Conflict: conflict.ID, Take: "theirs"}
			if kind == "key" {
				choice.Take, choice.Winner = "winner", "theirs"
			}
			resolution.Choices = []PullChoice{choice}
			if kind == "task" {
				if _, err := b.Pull(ctx, TransferOptions{Resolution: resolution}); err == nil || !strings.Contains(err.Error(), "reopen") || statusSnapshot(t, b) != before {
					t.Fatalf("implicit reopen = %v", err)
				}
				choice.Take, choice.Row = "manual", maps.Clone(conflict.Rows[0].Theirs)
				done := "done"
				choice.Row["status"] = &done
				resolution.Choices[0] = choice
			}
			merged, err := b.Pull(ctx, TransferOptions{Resolution: resolution})
			if err != nil || merged.Status != "changed" || merged.MainCommit == result.LocalCommit {
				t.Fatalf("resolved merge = %+v, %v", merged, err)
			}
			if kind == "key" && (countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE `key` = 'collision.key'") != 2 || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE id = 'ours' AND superseded_by = 'theirs'") != 1) {
				t.Fatal("fact loser was removed or not superseded")
			}
			if kind == "task" && countInternal(t, b, "SELECT COUNT(*) FROM tasks WHERE id = 'task' AND status = 'done'") != 1 {
				t.Fatal("done lost")
			}
			if countInternal(t, b, "SELECT COUNT(*) FROM dolt_conflicts") != 0 || countInternal(t, b, "SELECT COUNT(*) FROM dolt_constraint_violations") != 0 {
				t.Fatal("uncleared failure surfaces")
			}
			var author string
			if err := b.db.QueryRow("SELECT committer FROM dolt_log WHERE commit_hash = ?", merged.MainCommit).Scan(&author); err != nil || author != "user" {
				t.Fatalf("human attribution = %s, %v", author, err)
			}
			after := statusSnapshot(t, b)
			if _, err := b.Pull(ctx, TransferOptions{Resolution: resolution}); err == nil || statusSnapshot(t, b) != after {
				t.Fatalf("stale/replayed resolution = %v", err)
			}
		})
	}
}

func TestPullConfirmedMergeRetainsLateFailure(t *testing.T) {
	for _, failure := range []string{"after move", "SQL finalization", "late cancellation"} {
		t.Run(failure, func(t *testing.T) {
			_, b, _ := pullFixture(t, "independent")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("synthetic post-merge failure")
			hooks := transferHooks{afterMove: func() error { return injected }}
			if failure != "after move" {
				hooks.afterMove = nil
				hooks.finalize = func(tx *sql.Tx) error {
					if failure == "late cancellation" {
						cancel()
						injected = ctx.Err()
					}
					return errors.Join(tx.Commit(), injected)
				}
			}
			result, err := b.transfer(ctx, "pull", TransferOptions{}, hooks)
			if !errors.Is(err, injected) || !result.Changed || result.Status != "changed" || transferMain(t, b) != result.MainCommit {
				t.Fatalf("late failure = %+v %v", result, err)
			}
		})
	}
}

func TestPullManualChoicesRejectMalformedAndPreserveSnapshot(t *testing.T) {
	_, b, _ := pullFixture(t, "value")
	ctx := context.Background()
	shown, err := b.Pull(ctx, TransferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := statusSnapshot(t, b)
	for _, kind := range []string{"missing", "extra", "identity", "source", "generated", "timestamp", "too long", "denied", "duplicate", "unknown", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			row := maps.Clone(shown.Conflicts[0].Rows[0].Ours)
			delete(row, "live_key")
			value := "operator manual value"
			row["value"] = &value
			choice := PullChoice{Conflict: shown.Conflicts[0].ID, Take: "manual", Row: row}
			switch kind {
			case "missing":
				delete(row, "kind")
			case "extra":
				row["unexpected"] = &value
			case "identity":
				row["id"] = &value
			case "source":
				row["source"] = &value
			case "generated":
				row["live_key"] = &value
			case "timestamp":
				row["verified_at"] = &value
			case "too long":
				value = strings.Repeat("x", 65536)
			case "denied":
				if err := os.WriteFile(b.paths.ConfigFile(), []byte("[deny_list]\npatterns=['operator manual']\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.WriteFile(b.paths.ConfigFile(), nil, 0o600); err != nil {
						t.Error(err)
					}
				}()
			case "unknown":
				choice.Conflict = "facts:row:' OR 1=1"
			case "malformed":
				choice.Take = "newest"
			}
			choices := []PullChoice{choice}
			if kind == "duplicate" {
				choices = append(choices, choice)
			}
			_, err := b.Pull(ctx, TransferOptions{Resolution: &PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: choices}})
			if err == nil || statusSnapshot(t, b) != before {
				t.Fatalf("%s choices = %v; roots/refs must remain unchanged", kind, err)
			}
		})
	}
	row := maps.Clone(shown.Conflicts[0].Rows[0].Ours)
	delete(row, "live_key")
	value := "accepted manual value"
	row["value"] = &value
	result, err := b.Pull(ctx, TransferOptions{Resolution: &PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: []PullChoice{{Conflict: shown.Conflicts[0].ID, Take: "manual", Row: row}}}})
	if err != nil || !result.Changed || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE id='base' AND CHAR_LENGTH(value)=21") != 1 {
		t.Fatalf("manual final row = %+v, %v", result, err)
	}
}

func TestPullExplicitTaskReopenAndManualFactWinner(t *testing.T) {
	for _, kind := range []string{"task", "key"} {
		t.Run(kind, func(t *testing.T) {
			_, b, _ := pullFixture(t, kind)
			ctx := context.Background()
			shown, err := b.Pull(ctx, TransferOptions{})
			if err != nil {
				t.Fatal(err)
			}
			choice := PullChoice{Conflict: shown.Conflicts[0].ID, Take: "theirs", Reopen: true}
			if kind == "key" {
				winner := shown.Conflicts[0].Rows[0]
				row := maps.Clone(winner.Merged)
				delete(row, "live_key")
				value := "human durable value"
				row["value"] = &value
				choice = PullChoice{Conflict: shown.Conflicts[0].ID, Take: "manual", Winner: winner.ID, Row: row}
			}
			result, err := b.Pull(ctx, TransferOptions{Resolution: &PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: []PullChoice{choice}}})
			if err != nil || !result.Changed {
				t.Fatalf("explicit %s = %+v, %v", kind, result, err)
			}
			if kind == "task" && countInternal(t, b, "SELECT COUNT(*) FROM tasks WHERE id='task' AND status='open'") != 1 {
				t.Fatal("explicit reopen was lost")
			}
			if kind == "key" && (countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE live_key='collision.key'") != 1 || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE `key`='collision.key'") != 2) {
				t.Fatal("manual winner lost live uniqueness or historical rows")
			}
		})
	}
}

func TestPullRefusesInvalidMergedConstraintsAndSupersession(t *testing.T) {
	for _, kind := range []string{"document uniqueness", "dangling", "cycle", "schema", "unknown schema", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			a, b, remote := pullFixture(t, "independent")
			switch kind {
			case "document uniqueness":
				statusCommit(t, a, store.Statement{SQL: "INSERT INTO documents (id, path) VALUES ('remote', 'same.md')"})
				statusCommit(t, b, store.Statement{SQL: "INSERT INTO documents (id, path) VALUES ('local', 'same.md')"})
			case "dangling":
				statusCommit(t, a, store.Statement{SQL: "UPDATE facts SET superseded_by='missing' WHERE id='base'"})
			case "cycle":
				statusCommit(t, a, store.Statement{SQL: "UPDATE facts SET superseded_by='next' WHERE id='base'"}, store.Statement{SQL: "INSERT INTO facts (id, `key`, superseded_by) VALUES ('next','shared.key','base')"})
			case "schema":
				statusCommit(t, a, store.Statement{SQL: "ALTER TABLE tasks ALTER COLUMN notes SET DEFAULT 'new'"})
			case "unknown schema":
				statusCommit(t, a, store.Statement{SQL: "ALTER TABLE tasks ADD CONSTRAINT unexpected CHECK (title <> '')"})
			case "metadata":
				statusCommit(t, a, store.Statement{SQL: "UPDATE meta SET v='a' WHERE k='pull_fixture'"})
				statusCommit(t, b, store.Statement{SQL: "UPDATE meta SET v='b' WHERE k='pull_fixture'"})
			}
			statusPush(t, a)
			before, source := statusSnapshot(t, b), cloneFileTree(t, remote)
			result, err := b.Pull(context.Background(), TransferOptions{})
			if err == nil || result.Changed || statusSnapshot(t, b) != before || !reflect.DeepEqual(source, cloneFileTree(t, remote)) {
				t.Fatalf("unsafe %s = %+v %v", kind, result, err)
			}
		})
	}
}

func TestPullPreparedCommitCancellationAndInterleaving(t *testing.T) {
	for _, failure := range []string{"cancel", "prepare error", "changed head"} {
		t.Run(failure, func(t *testing.T) {
			_, b, _ := pullFixture(t, "independent")
			before := statusSnapshot(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hooks := transferHooks{beforeCommit: func() error {
				if b.proposalMu.TryLock() {
					b.proposalMu.Unlock()
					t.Fatal("merge does not own mutation mutex")
				}
				if failure == "cancel" {
					cancel()
					return nil
				}
				return errors.New("synthetic pre-promotion failure")
			}}
			if failure == "changed head" {
				hooks.beforeCommit = nil
				hooks.beforeMove = func() {
					if _, err := b.db.Exec("CALL DOLT_COMMIT('--allow-empty', '-m', 'foreign writer')"); err != nil {
						t.Fatal(err)
					}
					before = statusSnapshot(t, b)
				}
			}
			result, err := b.transfer(ctx, "pull", TransferOptions{}, hooks)
			if err == nil || result.Changed || statusSnapshot(t, b) != before {
				t.Fatalf("%s = %+v, %v", failure, result, err)
			}
		})
	}
}

func TestPullChoiceJSONRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"take":"ours","take":"theirs"}`, `{"Take":"ours","take":"theirs"}`, `{"row":{"id":"a","id":"b"}}`, `{"take":"ours","unknown":1}`, `{"take":"ours"} {}`, `{"row":{"id":2}}`, `[]`, `{`} {
		if _, err := DecodePullChoice(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted ambiguous choice %s", raw)
		}
	}
}

func TestPullResolvesDataAndLiveKeyTogetherWithoutLosingEitherChoice(t *testing.T) {
	a, b, _ := pullFixture(t, "value")
	statusCommit(t, a,
		store.Statement{SQL: "UPDATE facts SET superseded_by='replacement' WHERE id='base'"},
		store.Statement{SQL: "INSERT INTO facts (id, `key`, value) VALUES ('replacement', 'shared.key', 'new durable value')"})
	statusPush(t, a)
	ctx := context.Background()
	shown, err := b.Pull(ctx, TransferOptions{})
	if err != nil || shown.Status != "conflicted" || len(shown.Conflicts) != 2 {
		t.Fatalf("combined conflict = %+v, %v", shown, err)
	}
	choices := []PullChoice{}
	for _, conflict := range shown.Conflicts {
		choice := PullChoice{Conflict: conflict.ID, Take: "ours"}
		if conflict.Kind == "live-fact-key" {
			choice.Take, choice.Winner = "winner", "replacement"
		}
		choices = append(choices, choice)
	}
	result, err := b.Pull(ctx, TransferOptions{Resolution: &PullResolution{LocalCommit: shown.LocalCommit, RemoteCommit: shown.RemoteCommit, Choices: choices}})
	if err != nil || !result.Changed || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE id='base' AND superseded_by='replacement'") != 1 || countInternal(t, b, "SELECT COUNT(*) FROM facts WHERE live_key='shared.key' AND id='replacement'") != 1 {
		t.Fatalf("combined choices = %+v, %v", result, err)
	}
	var value string
	if err := b.db.QueryRow("SELECT value FROM facts WHERE id='base'").Scan(&value); err != nil || value != "ours" {
		t.Fatalf("the selected data value was lost: %q, %v", value, err)
	}
}
