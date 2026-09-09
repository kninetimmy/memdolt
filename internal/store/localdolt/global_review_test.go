package localdolt

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

var globalReviewActor = store.Actor{Name: "agent:codex", Email: "agent-codex@memdolt.invalid"}

func globalReviewSource(t *testing.T) *Store {
	t.Helper()
	repo, global := globalStoreFixture(t)
	if err := global.Close(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func globalReviewProposal() Proposal {
	return Proposal{Rationale: "review this exact global claim", Actor: globalReviewActor, Target: TargetGlobal}
}

func openReviewGlobal(t *testing.T, repo *Store) *Store {
	t.Helper()
	st, err := OpenGlobal(context.Background(), repo.paths.Base())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func globalReviewRows(t *testing.T, st *Store, hash, table string) []InteropRow {
	t.Helper()
	conn, err := st.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := readInteropRows(context.Background(), conn, hash, table)
	if err = errors.Join(err, conn.Close()); err != nil {
		t.Fatal(err)
	}
	return rows
}

func testGlobalAccept(ctx context.Context, repo *Store, id string, options AcceptOptions, hooks globalAcceptHooks) (AcceptResult, error) {
	repo.proposalMu.Lock()
	defer repo.proposalMu.Unlock()
	conn, err := repo.db.Conn(ctx)
	if err != nil {
		return AcceptResult{}, err
	}
	defer func() { _ = conn.Close() }()
	record, err := findProposalBranch(ctx, conn, ProposalBranch(id))
	if err != nil {
		return AcceptResult{}, err
	}
	return repo.acceptGlobalProposal(ctx, conn, record, memory.UserActor.CommitAuthor(), options, hooks)
}

func TestGlobalReviewNativePayloadHistoryAndRepeatedAcceptance(t *testing.T) {
	for _, kind := range []ProposalKind{KindFact, KindDecision} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			repo := globalReviewSource(t)
			head := internalString(t, repo, "SELECT DOLT_HASHOF('main')")
			other, err := repo.ProposeFact(ctx, Proposal{Rationale: "unrelated", Actor: globalReviewActor, Target: TargetRepo}, Fact{Key: "repo.only", Value: "retain"})
			if err != nil {
				t.Fatal(err)
			}
			table := "facts"
			if kind == KindDecision {
				table = "decisions"
			}
			row := emptyInteropRow(table)
			row["id"], row["source"] = interopString(newID()), interopString(globalReviewActor.Name)
			if kind == KindFact {
				row["key"], row["value"], row["kind"] = interopString("build.global"), interopString("exact\nmultiline payload"), interopString("")
				row["verified_at"] = interopString("2025-01-02 03:04:05")
			} else {
				row["title"], row["rationale"], row["summary"], row["status"] = interopString("Use boring tools"), interopString(" Preserve exact rationale.\r\n"), interopString(""), interopString("active")
			}
			staged, err := repo.stage(ctx, stagedWrite{kind: kind, proposal: globalReviewProposal(), rowID: interopValue(row, "id"), now: time.Now().UTC(), message: "propose exact global payload", statements: []store.Statement{interopInsert(table, row)}, text: []string{"exact global payload"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AcceptProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "terminal") {
				t.Fatalf("ordinary/MCP gate admitted global: %v", err)
			}
			if _, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true, ExpectedCommit: staged.Commit}); err == nil {
				t.Fatal("expected-commit gate admitted global")
			}
			result, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
			if err != nil || result.Commit == "" || result.GlobalStageCommit == "" || result.Proposal.Commit != staged.Commit || result.SourceMainCommit != head || !result.SourceRetained || result.AlreadyAccepted {
				t.Fatalf("global accept=%+v, %v", result, err)
			}
			global := openReviewGlobal(t, repo)
			rows := globalReviewRows(t, global, result.Commit, table)
			if len(rows) != 1 || !sameInteropRow(rows[0], row) {
				t.Fatalf("nullable payload changed: %#v, want %#v", rows, row)
			}
			sourceMetadata := globalReviewRows(t, repo, staged.Commit, "proposals")
			metadata := globalReviewRows(t, global, result.Commit, "proposals")
			if len(metadata) != 1 || !sameInteropRow(metadata[0], sourceMetadata[0]) {
				t.Fatal("source proposal metadata changed")
			}
			if got := internalString(t, global, "SELECT committer FROM dolt_log WHERE commit_hash = ?", result.GlobalStageCommit); got != globalReviewActor.Name {
				t.Fatalf("staging provenance=%s", got)
			}
			if got := internalString(t, global, "SELECT committer FROM dolt_log WHERE commit_hash = ?", result.Commit); got != "user" {
				t.Fatalf("review provenance=%s", got)
			}
			if got := internalString(t, global, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index = 1", result.Commit); got != result.GlobalStageCommit {
				t.Fatalf("not a real staging/reviewer merge: %s", got)
			}
			before := internalCount(t, global, "SELECT COUNT(*) FROM dolt_log")
			if err := global.Close(); err != nil {
				t.Fatal(err)
			}
			repo = reopenConfirmedStore(t, repo)
			repeated, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
			if err != nil || !repeated.AlreadyAccepted || repeated.Commit != result.Commit || repeated.GlobalStageCommit != result.GlobalStageCommit {
				t.Fatalf("repeat=%+v, %v", repeated, err)
			}
			global = openReviewGlobal(t, repo)
			if got := internalCount(t, global, "SELECT COUNT(*) FROM dolt_log"); got != before {
				t.Fatalf("duplicate acceptance history: %d -> %d", before, got)
			}
			if got := internalString(t, repo, "SELECT DOLT_HASHOF('main')"); got != head {
				t.Fatal("repository main moved")
			}
			pending, err := repo.PendingProposals(ctx)
			if err != nil || len(pending) != 2 {
				t.Fatalf("source branches changed: %+v %v", pending, err)
			}
			for _, p := range pending {
				if p.ID == other.ID && p.Commit != other.Commit {
					t.Fatal("unrelated proposal moved")
				}
			}
		})
	}
}

func TestGlobalReviewNativeLateOutcomesAndHeadChanges(t *testing.T) {
	for _, failure := range []string{"stage finalization", "merge finalization", "stage cancellation", "merge cancellation", "source changed after stage", "source changed after merge", "disabled after stage"} {
		t.Run(failure, func(t *testing.T) {
			repo := globalReviewSource(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			staged, err := repo.ProposeFact(ctx, globalReviewProposal(), Fact{Key: "global.recovery", Value: "keep one acceptance"})
			if err != nil {
				t.Fatal(err)
			}
			hooks := globalAcceptHooks{}
			finalize := func(tx *sql.Tx) error { return errors.Join(tx.Commit(), errConfirmed) }
			canceled := func(tx *sql.Tx) error { cancel(); return tx.Commit() }
			change := func() {
				if _, err := repo.db.ExecContext(context.Background(), "CALL DOLT_BRANCH('-f', ?, 'main')", staged.Branch); err != nil {
					t.Fatal(err)
				}
			}
			switch failure {
			case "stage finalization":
				hooks.finalizeStage = finalize
			case "merge finalization":
				hooks.finalizeMerge = finalize
			case "stage cancellation":
				hooks.finalizeStage = canceled
			case "merge cancellation":
				hooks.finalizeMerge = canceled
			case "source changed after stage":
				hooks.afterStage = change
			case "source changed after merge":
				hooks.afterMerge = change
			case "disabled after stage":
				hooks.afterStage = func() {
					if _, err := SetGlobalEnabled(repo.paths.Base(), false); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := testGlobalAccept(ctx, repo, staged.ID, AcceptOptions{Force: true}, hooks)
			if err == nil || result.GlobalStageCommit == "" || result.Proposal.Commit != staged.Commit || !result.SourceRetained || !strings.Contains(err.Error(), result.GlobalStageCommit) {
				t.Fatalf("late outcome lost: %+v %v", result, err)
			}
			merged := strings.Contains(failure, "merge")
			if (result.Commit != "") != merged {
				t.Fatalf("wrong confirmed merge: %+v %v", result, err)
			}
			if _, err := SetGlobalEnabled(repo.paths.Base(), true); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(failure, "source changed") {
				if internalString(t, repo, "SELECT hash FROM dolt_branches WHERE name = ?", staged.Branch) == staged.Commit {
					t.Fatal("source change hidden")
				}
				if _, err := repo.db.ExecContext(context.Background(), "CALL DOLT_BRANCH('-f', ?, ?)", staged.Branch, staged.Commit); err != nil {
					t.Fatal(err)
				}
			}
			repo = reopenConfirmedStore(t, repo)
			repeated, err := repo.AcceptTerminalProposal(context.Background(), staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
			if err != nil || repeated.GlobalStageCommit != result.GlobalStageCommit || repeated.AlreadyAccepted != merged || repeated.Commit == "" {
				t.Fatalf("recovery replayed/lost progress: %+v %v", repeated, err)
			}
			global := openReviewGlobal(t, repo)
			if internalCount(t, global, "SELECT COUNT(*) FROM facts") != 1 || internalString(t, global, "SELECT COUNT(*) FROM dolt_log WHERE message = ?", "review accept fact "+staged.ID) != "1" {
				t.Fatal("duplicate rows/acceptance")
			}
		})
	}
}

func TestGlobalReviewDestinationBeforeImages(t *testing.T) {
	for _, operation := range []string{"overwrite", "supersede"} {
		for _, destination := range []string{"matching", "missing", "changed"} {
			t.Run(operation+"/"+destination, func(t *testing.T) {
				ctx := context.Background()
				repo, global := globalStoreFixture(t)
				fact, err := repo.FactAdd(ctx, FactAddOptions{Fact: Fact{Key: "shared.key", Value: "original"}, Actor: memory.UserActor, Source: "user"})
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := factSnapshotByID(ctx, mustReviewConn(t, repo), fact.ID)
				if err != nil {
					t.Fatal(err)
				}
				row := globalReviewRows(t, repo, fact.Commit, "facts")[0]
				if destination != "missing" {
					row = maps.Clone(row)
					if destination == "changed" {
						row["value"] = interopString("changed globally")
					}
					if _, err := global.Commit(ctx, store.CommitRequest{Statements: []store.Statement{interopInsert("facts", row)}, Text: []string{"fixture"}, Message: "seed matching global before-image", Author: memory.UserActor.CommitAuthor()}); err != nil {
						t.Fatal(err)
					}
				}
				before := internalString(t, global, "SELECT DOLT_HASHOF('main')")
				if err := global.Close(); err != nil {
					t.Fatal(err)
				}
				resolution := FactResolutionOverwrite
				if operation == "supersede" {
					resolution = FactResolutionSupersede
				}
				staged, err := repo.ProposeFactResolution(ctx, globalReviewProposal(), snapshot, Fact{Key: "shared.key", Value: "replacement"}, resolution)
				if err != nil {
					t.Fatal(err)
				}
				result, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: operation != "supersede"})
				if destination == "matching" {
					if err != nil || result.Commit == "" {
						t.Fatalf("valid %s refused: %+v %v", operation, result, err)
					}
				} else if err == nil || result.Commit != "" || result.GlobalStageCommit != "" {
					t.Fatalf("wrong destination accepted: %+v %v", result, err)
				}
				global = openReviewGlobal(t, repo)
				if destination != "matching" && internalString(t, global, "SELECT DOLT_HASHOF('main')") != before {
					t.Fatal("refusal moved global main")
				}
			})
		}
	}
}

func mustReviewConn(t *testing.T, s *Store) *sql.Conn {
	t.Helper()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func internalString(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	var value string
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

type globalReviewScorer struct {
	score            float32
	inference, close error
	passages         []string
}

func (s *globalReviewScorer) Rerank(_, passage string) (float32, error) {
	s.passages = append(s.passages, passage)
	return s.score, s.inference
}
func (s *globalReviewScorer) Close() error { return s.close }

func TestGlobalReviewContradictionUsesDestinationAndFailsClosed(t *testing.T) {
	for _, failure := range []string{"none", "threshold", "configuration", "open", "inference", "nonfinite", "close", "force"} {
		t.Run(failure, func(t *testing.T) {
			repo, global := globalStoreFixture(t)
			ctx := context.Background()
			if _, err := global.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "global.durable", Value: "only global candidate"}}); err != nil {
				t.Fatal(err)
			}
			before := internalString(t, global, "SELECT DOLT_HASHOF('main')")
			if err := global.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "repo.durable", Value: "must not score repository candidate"}}); err != nil {
				t.Fatal(err)
			}
			staged, err := repo.ProposeFact(ctx, globalReviewProposal(), Fact{Key: "global.incoming", Value: "new claim"})
			if err != nil {
				t.Fatal(err)
			}
			scorer := &globalReviewScorer{score: -10}
			configCalls, opens := 0, 0
			options := AcceptOptions{Force: failure == "force", ValidateContradictionConfig: func() error {
				configCalls++
				if failure == "configuration" || failure == "force" {
					return errConfirmed
				}
				return nil
			}, OpenContradictionScorer: func(context.Context) (ContradictionScorer, error) {
				opens++
				if failure == "open" {
					return nil, errConfirmed
				}
				return scorer, nil
			}}
			switch failure {
			case "threshold":
				scorer.score = 2
			case "inference":
				scorer.inference = errConfirmed
			case "nonfinite":
				scorer.score = float32(math.NaN())
			case "close":
				scorer.close = errConfirmed
			}
			result, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), options)
			success := failure == "none" || failure == "force"
			if (err == nil) != success || (result.Commit != "") != success {
				t.Fatalf("probe %s: %+v %v", failure, result, err)
			}
			if failure == "force" && (configCalls != 0 || opens != 0) {
				t.Fatal("force invoked model guard")
			}
			if failure == "threshold" && !errors.Is(err, ErrContradiction) {
				t.Fatal(err)
			}
			for _, passage := range scorer.passages {
				if !strings.Contains(passage, "only global candidate") || strings.Contains(passage, "repository candidate") {
					t.Fatalf("wrong durable scope: %s", passage)
				}
			}
			global = openReviewGlobal(t, repo)
			if !success && (internalString(t, global, "SELECT DOLT_HASHOF('main')") != before || internalCount(t, global, "SELECT COUNT(*) FROM dolt_branches") != 1 || result.GlobalStageCommit != "") {
				t.Fatal("failed model probe left durable effects")
			}
		})
	}
}

func TestGlobalReviewRefusesCollisionsAndMalformedCommits(t *testing.T) {
	for _, failure := range []string{"key", "identity identical", "identity different", "proposal identity", "extra table", "extra schema", "extra commit", "extra fact", "false supersede", "source actor", "dirty", "disabled", "deny", "deny author email", "older", "newer", "contended"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			repo, global := globalStoreFixture(t)
			row := emptyInteropRow("facts")
			row["id"], row["key"], row["value"], row["source"] = interopString(newID()), interopString("global.collision"), interopString("incoming deniedbeacon"), interopString(globalReviewActor.Name)
			w := stagedWrite{kind: KindFact, proposal: globalReviewProposal(), rowID: interopValue(row, "id"), now: time.Now().UTC(), message: "propose global fixture", text: []string{"fixture"}, statements: []store.Statement{interopInsert("facts", row)}}
			if failure == "deny author email" {
				w.proposal.Actor.Email = "deniedemail@memdolt.invalid"
			}
			switch failure {
			case "extra table":
				w.statements = append(w.statements, store.Statement{SQL: "INSERT INTO session_notes (id, text) VALUES (?, ?)", Args: []any{newID(), "unreviewed"}})
			case "extra schema":
				w.statements = append(w.statements, store.Statement{SQL: "CREATE TABLE unreviewed (id INT)"})
			case "extra fact":
				extra := maps.Clone(row)
				extra["id"], extra["key"] = interopString(newID()), interopString("extra.fact")
				w.statements = append(w.statements, interopInsert("facts", extra))
			case "false supersede":
				w.kind = KindSupersede
			case "source actor":
				row["source"] = interopString("agent:other")
				w.statements = []store.Statement{interopInsert("facts", row)}
			}
			staged, err := repo.stage(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "key":
				if _, err := global.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "global.collision", Value: "existing"}}); err != nil {
					t.Fatal(err)
				}
			case "identity identical", "identity different", "proposal identity":
				statement := interopInsert("facts", row)
				if failure == "identity different" {
					copy := maps.Clone(row)
					copy["value"] = interopString("different")
					statement = interopInsert("facts", copy)
				}
				if failure == "proposal identity" {
					statement = interopInsert("proposals", globalReviewRows(t, repo, staged.Commit, "proposals")[0])
				}
				if _, err := global.Commit(ctx, store.CommitRequest{Statements: []store.Statement{statement}, Text: []string{"fixture"}, Message: "fixture collision", Author: memory.UserActor.CommitAuthor()}); err != nil {
					t.Fatal(err)
				}
			case "extra commit":
				conn := mustReviewConn(t, repo)
				if err := checkoutBranch(ctx, conn, staged.Branch); err != nil {
					t.Fatal(err)
				}
				if _, err := repo.commitConn(ctx, conn, store.CommitRequest{Statements: []store.Statement{{SQL: "UPDATE facts SET evidence = ? WHERE id = ?", Args: []any{"unreviewed.md", staged.RowID}}}, Text: []string{"fixture"}, Message: "extra commit", Author: globalReviewActor}); err != nil {
					t.Fatal(err)
				}
				if err := checkoutBranch(ctx, conn, MainBranch); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if _, err := global.db.ExecContext(ctx, "INSERT INTO session_notes (id, text) VALUES (?, 'dirty')", newID()); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := SetGlobalEnabled(repo.paths.Base(), false); err != nil {
					t.Fatal(err)
				}
			case "deny":
				writeDocumentFixture(t, repo.paths.ConfigFile(), "[global]\nenabled=true\n[deny_list]\npatterns=['deniedbeacon']\n")
			case "deny author email":
				writeDocumentFixture(t, repo.paths.ConfigFile(), "[global]\nenabled=true\n[deny_list]\npatterns=['deniedemail']\n")
			case "older", "newer":
				version := "3"
				if failure == "newer" {
					version = "999"
				}
				if _, err := global.Commit(ctx, store.CommitRequest{Statements: []store.Statement{{SQL: "UPDATE meta SET v = ? WHERE k = ?", Args: []any{version, store.SchemaVersionKey}}}, NoText: true, Message: "fixture schema version", Author: memory.UserActor.CommitAuthor()}); err != nil {
					t.Fatal(err)
				}
			}
			before := internalString(t, global, "SELECT DOLT_HASHOF('main')")
			if failure != "contended" {
				if err := global.Close(); err != nil {
					t.Fatal(err)
				}
			}
			result, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
			if err == nil || result.Commit != "" || result.GlobalStageCommit != "" {
				t.Fatalf("unsafe %s admitted: %+v %v", failure, result, err)
			}
			if strings.HasPrefix(failure, "deny") && !errors.Is(err, store.ErrDenied) {
				t.Fatal(err)
			}
			if failure == "contended" {
				if internalString(t, global, "SELECT DOLT_HASHOF('main')") != before {
					t.Fatal("contended write")
				}
				return
			}
			if failure == "older" || failure == "newer" || failure == "disabled" {
				return
			}
			global = openReviewGlobal(t, repo)
			if internalString(t, global, "SELECT DOLT_HASHOF('main')") != before || internalCount(t, global, "SELECT COUNT(*) FROM dolt_branches") != 1 {
				t.Fatal("refusal changed destination")
			}
		})
	}
}

func TestGlobalReviewNativeUnobservedMergeIsUnknownAndNeverReplayed(t *testing.T) {
	repo := globalReviewSource(t)
	staged, err := repo.ProposeFact(context.Background(), globalReviewProposal(), Fact{Key: "unknown.merge", Value: "persist before result observation"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := testGlobalAccept(context.Background(), repo, staged.ID, AcceptOptions{Force: true}, globalAcceptHooks{finalizeStage: func(tx *sql.Tx) error { return errors.Join(tx.Commit(), errConfirmed) }})
	if err == nil || result.GlobalStageCommit == "" || result.Commit != "" {
		t.Fatalf("stage=%+v %v", result, err)
	}
	global := openReviewGlobal(t, repo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := global.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	proposal := result.Proposal
	proposal.Commit = result.GlobalStageCommit
	if err := doltMerge(ctx, tx, proposal); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)", "review accept fact "+staged.ID, memory.UserActor.CommitAuthor().String())
	if err != nil || !rows.Next() {
		t.Fatalf("native result unavailable: %v", err)
	}
	cancel()
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var hash string
	scanErr := rows.Scan(&hash)
	unknown, err := nativeCommitResult(hash, 0, scanErr)
	if unknown.Hash != "" || !errors.Is(err, store.ErrCommitUnknown) {
		t.Fatalf("unknown merge=%+v %v", unknown, err)
	}
	_ = tx.Rollback()
	_ = conn.Close()
	if err := global.Close(); err != nil {
		t.Fatal(err)
	}
	repeated, err := repo.AcceptTerminalProposal(context.Background(), staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
	if err != nil || repeated.Commit == "" || !repeated.AlreadyAccepted || repeated.GlobalStageCommit != result.GlobalStageCommit {
		t.Fatalf("unknown native merge replayed: %+v %v", repeated, err)
	}
}

func TestGlobalReviewOwnershipHasNoCrossStoreWaitCycle(t *testing.T) {
	repo := globalReviewSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	staged, err := repo.ProposeFact(ctx, globalReviewProposal(), Fact{Key: "lock.order", Value: "bounded ownership"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := testGlobalAccept(ctx, repo, staged.ID, AcceptOptions{Force: true}, globalAcceptHooks{afterCapture: func() { close(entered); <-release }})
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("accept never acquired ownership")
	}
	write := make(chan error, 1)
	go func() {
		_, err := repo.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "concurrent.repo", Value: "serialize"}})
		write <- err
	}()
	if global, err := OpenGlobal(ctx, repo.paths.Base()); err == nil {
		_ = global.Close()
		t.Error("competing recall owner acquired global lock")
	}
	close(release)
	for _, done := range []chan error{finished, write} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("cross-store deadlock")
		}
	}
}

func TestGlobalReviewChangedDestinationHeadRetainsStaging(t *testing.T) {
	ctx := context.Background()
	repo := globalReviewSource(t)
	staged, err := repo.ProposeFact(ctx, globalReviewProposal(), Fact{Key: "changed.head", Value: "reviewed payload"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := testGlobalAccept(ctx, repo, staged.ID, AcceptOptions{Force: true}, globalAcceptHooks{beforeMerge: func(global *Store) {
		// A separate native session does not cooperate with proposalMu.
		conn, err := global.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, commitErr := global.commitConn(ctx, conn, store.CommitRequest{Statements: []store.Statement{{SQL: "INSERT INTO session_notes (id, text) VALUES (?, ?)", Args: []any{newID(), "foreign session"}}}, Text: []string{"foreign session"}, Message: "foreign global main advance", Author: memory.UserActor.CommitAuthor()})
		if err := errors.Join(commitErr, conn.Close()); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || result.Commit != "" || result.GlobalStageCommit == "" || !strings.Contains(err.Error(), "main changed") {
		t.Fatalf("changed global head ignored: %+v %v", result, err)
	}
	repeated, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
	if err != nil || repeated.Commit == "" || repeated.GlobalStageCommit != result.GlobalStageCommit || repeated.AlreadyAccepted {
		t.Fatalf("safe explicit resume failed: %+v %v", repeated, err)
	}
	global := openReviewGlobal(t, repo)
	if internalCount(t, global, "SELECT COUNT(*) FROM session_notes") != 1 || internalCount(t, global, "SELECT COUNT(*) FROM facts") != 1 {
		t.Fatal("foreign or reviewed rows lost")
	}
}

func TestGlobalReviewNativeReceiptRejectsChangedSourceIdentity(t *testing.T) {
	ctx := context.Background()
	repo := globalReviewSource(t)
	staged, err := repo.ProposeFact(ctx, globalReviewProposal(), Fact{Key: "stable.identity", Value: "original reviewed payload"})
	if err != nil {
		t.Fatal(err)
	}
	row := globalReviewRows(t, repo, staged.Commit, "facts")[0]
	metadata := globalReviewRows(t, repo, staged.Commit, "proposals")[0]
	accepted, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	global := openReviewGlobal(t, repo)
	changed, err := global.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "stable.identity", Value: "later human edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := global.Close(); err != nil {
		t.Fatal(err)
	}
	prior, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
	if err != nil || !prior.AlreadyAccepted || prior.Commit != accepted.Commit {
		t.Fatalf("later global edit triggered replay: %+v %v", prior, err)
	}
	conn := mustReviewConn(t, repo)
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH('-f', ?, 'main')", staged.Branch); err != nil {
		t.Fatal(err)
	}
	if err := checkoutBranch(ctx, conn, staged.Branch); err != nil {
		t.Fatal(err)
	}
	row["value"] = interopString("different single-commit source payload")
	if _, err := repo.commitConn(ctx, conn, store.CommitRequest{Statements: []store.Statement{interopInsert("facts", row), interopInsert("proposals", metadata)}, Text: []string{"source fixture"}, Message: "replace source proposal", Author: globalReviewActor}); err != nil {
		t.Fatal(err)
	}
	if err := checkoutBranch(ctx, conn, MainBranch); err != nil {
		t.Fatal(err)
	}
	refused, err := repo.AcceptTerminalProposal(ctx, staged.ID, memory.UserActor.CommitAuthor(), AcceptOptions{Force: true})
	if err == nil || refused.Commit != "" || refused.AlreadyAccepted || refused.GlobalStageCommit != "" {
		t.Fatalf("same-id/different-payload source reused acceptance: %+v %v", refused, err)
	}
	global = openReviewGlobal(t, repo)
	if internalString(t, global, "SELECT DOLT_HASHOF('main')") != changed.Commit || internalString(t, global, "SELECT value FROM facts WHERE id = ?", staged.RowID) != "later human edit" {
		t.Fatal("repeat overwrote later global edit")
	}
}
