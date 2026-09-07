package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
)

func TestHumanMemorySharedCommitChangePreservesLateStagingResidue(t *testing.T) {
	ctx := context.Background()
	st, _ := documentFixture(t)
	actor, err := memory.NormalizeActor("codex")
	if err != nil {
		t.Fatal(err)
	}
	fact := Fact{Key: "staged.residue", Value: "keep the failed staging commit inspectable"}
	id, now := newID(), time.Now().UTC()
	late := errors.New("synthetic transaction finalization error")
	_, err = st.stage(ctx, stagedWrite{
		kind: KindFact, proposal: Proposal{Actor: actor.CommitAuthor(), Rationale: "residue regression", Target: TargetRepo},
		rowID: id, now: now, message: "propose fact staged.residue", text: fact.text(), newFactKey: fact.Key,
		statements:     []store.Statement{insertFact(id, fact, actor.CommitAuthor(), now)},
		finalizeCommit: func(tx *sql.Tx) error { return errors.Join(tx.Commit(), late) },
	})
	if !errors.Is(err, late) || !strings.Contains(err.Error(), "confirmed") {
		t.Fatalf("staging finalization evidence lost: %v", err)
	}
	pending, readErr := st.PendingProposals(ctx)
	if readErr != nil || len(pending) != 1 || !strings.Contains(err.Error(), pending[0].Commit) {
		t.Fatalf("late error deleted or obscured staged residue: %+v %v, staging: %v", pending, readErr, err)
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'") != 0 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatal("failed staging changed durable or working main")
	}
}

func TestHumanMemoryCommitHashSurvivesTransactionFinalization(t *testing.T) {
	st, _ := documentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := st.handle()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	id := newID()
	result, err := st.commitConnFinalize(ctx, conn, store.CommitRequest{
		Author: memory.UserActor.CommitAuthor(), Message: "fact add " + id, Text: []string{"late.key", "confirmed"}, RequireClean: true,
		Statements: []store.Statement{{SQL: "INSERT INTO facts(id, `key`, value) VALUES (?, 'late.key', 'confirmed')", Args: []any{id}}},
	}, func(tx *sql.Tx) error {
		// The real DOLT_COMMIT has returned. Cancel before database/sql's outer
		// COMMIT, exercising the actual driver's late failure, not a fake hash.
		cancel()
		return tx.Commit()
	})
	if err == nil || result.Hash == "" || !strings.Contains(err.Error(), result.Hash) || !strings.Contains(err.Error(), "confirmed") {
		t.Fatalf("lost finalized Dolt result: %+v %v", result, err)
	}
	fresh, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	head, err := branchHead(context.Background(), fresh, MainBranch)
	if err != nil || head != result.Hash || internalCount(t, st, "SELECT COUNT(*) FROM facts AS OF 'main'") != 1 {
		t.Fatalf("native late-commit fixture did not retain main: head=%s result=%+v %v", head, result, err)
	}
}

func TestHumanMemoryChangedTextInvalidatesProductionVectors(t *testing.T) {
	ctx := context.Background()
	st, _ := documentFixture(t)
	fact, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "build.command", Value: "retiredbeacon"}, Source: "user", Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := st.DecisionAdd(ctx, DecisionAddOptions{Decision: Decision{Title: "Choose storage", Rationale: "durable rows", Summary: "retiredsummary"}, Source: "observed", Actor: memory.UserActor})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedding.Rebuild(ctx, st.paths.EmbeddingsFile(), sources, documentInference{}.Embed); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "build.command", Value: "currentbeacon"}, Source: "user", Actor: memory.UserActor}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecisionSetSummary(ctx, decision.ID, "currentsummary", memory.UserActor); err != nil {
		t.Fatal(err)
	}
	sources, err = st.EmbeddingSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := embedding.Status(ctx, st.paths.EmbeddingsFile(), sources)
	if err != nil || status.ContentHashMismatched != 2 || status.Current != 0 {
		t.Fatalf("changed source hashes: %+v %v", status, err)
	}
	for _, source := range sources {
		if strings.Contains(source.Text, "retired") {
			t.Fatalf("old embedding input retained: %+v", source)
		}
	}
	cfg := retrieval.DefaultConfig()
	cfg.Mode = retrieval.ModeHybrid
	old, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), documentInference{}, cfg, retrieval.Options{Query: "retiredbeacon"})
	if err != nil || old.ReturnedCount != 0 || len(old.Warnings) == 0 {
		t.Fatalf("old vector entered current retrieval: %+v %v", old, err)
	}
	current, err := retrieval.Recall(ctx, st, st.paths.EmbeddingsFile(), documentInference{}, cfg, retrieval.Options{Query: "currentbeacon", Provenance: true})
	if err != nil || current.ReturnedCount != 1 || current.Results[0].SourceID != fact.ID || current.Results[0].Body != "currentbeacon" || len(current.Warnings) == 0 {
		t.Fatalf("current lexical fallback: %+v %v", current, err)
	}
	if current.Results[0].LastChanged == nil || current.Results[0].LastChanged.Author != "user" {
		t.Fatal("retrieval lost human provenance")
	}
	rebuilt, err := embedding.Rebuild(ctx, st.paths.EmbeddingsFile(), sources, documentInference{}.Embed)
	if err != nil || rebuilt.Refreshed != 2 || rebuilt.Created != 0 {
		t.Fatalf("rebuild did not refresh changed rows: %+v %v", rebuilt, err)
	}
}

func TestHumanMemoryNullableRowsAndSchemaRefusals(t *testing.T) {
	ctx := context.Background()
	st, _ := documentFixture(t)
	id, decisionID := newID(), newID()
	check := func(query, id string) {
		t.Helper()
		rows, err := st.Query(ctx, query, id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var valid bool
		if !rows.Next() {
			t.Fatal("missing nullable fixture")
		}
		if err := rows.Scan(&valid); err != nil || !valid {
			t.Fatalf("nullable fields changed: %t %v", valid, err)
		}
	}
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "nullable synthetic rows", Statements: []store.Statement{
		{SQL: "INSERT INTO facts(id, `key`) VALUES (?, 'nullable.key')", Args: []any{id}},
		{SQL: "INSERT INTO decisions(id, status) VALUES (?, 'active')", Args: []any{decisionID}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FactVerify(ctx, id, memory.UserActor); err != nil {
		t.Fatal(err)
	}
	check("SELECT value IS NULL AND source = 'user' AND kind IS NULL AND evidence IS NULL AND created_at IS NULL AND verified_at IS NOT NULL FROM facts WHERE id = ?", id)
	updated, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "nullable.key", Value: "asserted", Kind: strings.Repeat(" ", 80)}, Source: "observed", Actor: memory.UserActor})
	if err != nil || updated.ID != id || updated.Status != "updated" || internalCount(t, st, "SELECT COUNT(*) FROM facts WHERE kind IS NULL AND created_at IS NULL") != 1 {
		t.Fatalf("nullable upsert or blank normalization: %+v %v", updated, err)
	}
	if _, err := st.DecisionSetSummary(ctx, decisionID, " new summary ", memory.UserActor); err != nil {
		t.Fatal(err)
	}
	check("SELECT title IS NULL AND rationale IS NULL AND source IS NULL AND alternatives_rejected IS NULL AND evidence IS NULL AND decided_at IS NULL FROM decisions WHERE id = ?", decisionID)
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "unsupported synthetic generation", Statements: []store.Statement{
		{SQL: "ALTER TABLE facts DROP INDEX uk_fact_live_key"}, {SQL: "ALTER TABLE facts DROP COLUMN live_key"},
		{SQL: "ALTER TABLE facts ADD COLUMN live_key VARCHAR(255) GENERATED ALWAYS AS ('unexpected') STORED"},
	}}); err != nil {
		t.Fatal(err)
	}
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	if _, err := st.FactVerify(ctx, id, memory.UserActor); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("modified generation accepted: %v", err)
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
		t.Fatal("schema refusal changed memory")
	}
}

func TestHumanMemoryActorMetadataAndChainValidation(t *testing.T) {
	ctx := context.Background()
	st, _ := documentFixture(t)
	add := func(key string) HumanMemoryResult {
		t.Helper()
		result, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: key, Value: "safe"}, Source: "user", Actor: memory.UserActor})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	a, b, c := add("test.a"), add("test.b"), add("test.c")
	for _, actor := range []memory.Actor{
		{Name: "agent:user", Raw: "user"}, {Name: "user", Raw: "codex"}, {Name: "agent:codex", Raw: "codex"}, {},
	} {
		if _, err := st.FactVerify(ctx, a.ID, actor); err == nil || !strings.Contains(err.Error(), "trusted human") {
			t.Fatalf("forged actor accepted: %+v %v", actor, err)
		}
	}
	for _, tc := range []struct {
		pattern, source string
		actor           memory.Actor
	}{
		{"observed", "observed", memory.UserActor}, {"USER", "user", memory.Actor{Name: "user", Raw: "USER"}},
	} {
		writeDocumentFixture(t, st.paths.ConfigFile(), "[deny_list]\npatterns=['"+tc.pattern+"']\n")
		if _, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "test.denied", Value: "safe"}, Source: tc.source, Actor: tc.actor}); !errors.Is(err, store.ErrDenied) {
			t.Fatalf("provenance escaped deny scan: %v", err)
		}
	}
	writeDocumentFixture(t, st.paths.ConfigFile(), "")
	// Existing corrupt replacement chains must refuse, not amplify their
	// dangling links or loop forever. This fixture is deliberately raw SQL.
	for _, link := range []string{"missing", b.ID} {
		if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), NoText: true, Message: "corrupt synthetic chain", Statements: []store.Statement{
			{SQL: "UPDATE facts SET superseded_by = ? WHERE id = ?", Args: []any{link, b.ID}},
		}}); err != nil {
			t.Fatal(err)
		}
		before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
		if _, err := st.FactSupersede(ctx, a.ID, b.ID, memory.UserActor); err == nil {
			t.Fatal("corrupt replacement chain accepted")
		}
		if internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before || internalCount(t, st, "SELECT COUNT(*) FROM dolt_status") != 0 {
			t.Fatal("invalid chain left partial changes")
		}
	}
	if _, err := st.FactSupersede(ctx, b.ID, c.ID, memory.UserActor); err != nil {
		t.Fatalf("explicit repair to a valid replacement refused: %v", err)
	}
	if _, err := st.FactSupersede(ctx, a.ID, b.ID, memory.UserActor); err != nil {
		t.Fatalf("valid transitive supersession refused: %v", err)
	}
	if _, err := st.FactSupersede(ctx, c.ID, a.ID, memory.UserActor); err == nil {
		t.Fatal("transitive cycle accepted")
	}
	if _, err := st.FactVerify(ctx, "'; DROP TABLE facts; --", memory.UserActor); err == nil {
		t.Fatal("unknown bound operand accepted")
	}
	if internalCount(t, st, "SELECT COUNT(*) FROM facts") != 3 {
		t.Fatal("supersession deleted rows")
	}
}

func TestHumanMemoryConcurrentUpsertAndSameSecondVerification(t *testing.T) {
	ctx := context.Background()
	st, _ := documentFixture(t)
	before := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
	var wait sync.WaitGroup
	results := make(chan HumanMemoryResult, 4)
	for i := range 4 {
		wait.Go(func() {
			result, err := st.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "concurrent.key", Value: fmt.Sprint(i)}, Source: "user", Actor: memory.UserActor})
			if err != nil {
				t.Error(err)
			}
			results <- result
		})
	}
	wait.Wait()
	close(results)
	created, id := 0, ""
	for result := range results {
		if result.Status == "created" {
			created++
		}
		if id != "" && result.ID != id {
			t.Fatal("concurrent upsert duplicated the live key")
		}
		id = result.ID
	}
	if created != 1 || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != before+4 {
		t.Fatalf("upserts created %d rows or wrong commit count", created)
	}
	// A real clock boundary can legitimately change the second. Retry only the
	// test observation; no production write retries or fabricated timestamps.
	for range 5 {
		_, err := st.FactVerify(ctx, id, memory.UserActor)
		if err != nil {
			t.Fatal(err)
		}
		count := internalCount(t, st, "SELECT COUNT(*) FROM dolt_log")
		second, err := st.FactVerify(ctx, id, memory.UserActor)
		if err != nil {
			t.Fatal(err)
		}
		if second.Status == "unchanged" {
			if second.Commit != "" || internalCount(t, st, "SELECT COUNT(*) FROM dolt_log") != count {
				t.Fatalf("no-op made an empty commit: %+v", second)
			}
			return
		}
	}
	t.Fatal("could not observe a same-second verification")
}
