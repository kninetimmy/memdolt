package storeipc_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kninetimmy/memdolt/internal/ipc"
	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

var testActor = store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}

type fixedScorer float32

func (s fixedScorer) Rerank(string, string) (float32, error) { return float32(s), nil }
func (fixedScorer) Close() error                             { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// baseDir returns a scratch repository root. It does not use t.TempDir
// because that fails the test if the directory cannot be removed, and the
// embedded engine memory-maps files inside the Dolt data directory that
// Windows may still hold briefly after the store is closed.
func baseDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "memdolt-storeipc")
	if err != nil {
		t.Fatalf("create scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startOwner opens a store and serves it on this repository's IPC
// endpoint, exactly as the owning process does.
func startOwner(t *testing.T) (string, *localdolt.Store, *ipc.Server) {
	t.Helper()
	return startOwnerWrapping(t, nil)
}

// startOwnerWrapping is startOwner with the owner's routes wrapped, which is
// how a test puts a fault between the store and the answer that reports it.
func startOwnerWrapping(t *testing.T, wrap func(http.Handler) http.Handler) (string, *localdolt.Store, *ipc.Server) {
	t.Helper()
	return startOwnerConfigured(t, nil, wrap)
}

// startOwnerConfigured lets a review test inject deterministic contradiction
// scoring while every ordinary transport test uses the same permissive scorer.
func startOwnerConfigured(
	t *testing.T,
	configureAccept func(*localdolt.Store) storeipc.ReviewAcceptFunc,
	wrap func(http.Handler) http.Handler,
) (string, *localdolt.Store, *ipc.Server) {
	t.Helper()
	base := baseDir(t)

	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: testActor, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reviewAccept := reviewAcceptWithScorer(st, func() localdolt.ContradictionScorer { return fixedScorer(-100) })
	if configureAccept != nil {
		reviewAccept = configureAccept(st)
	}
	routes, err := storeipc.NewHandler(storeipc.Config{
		Store:        st,
		ReviewAccept: reviewAccept,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	if wrap != nil {
		routes = wrap(routes)
	}
	srv, err := ipc.Listen(ipc.Config{BaseDir: base, Handler: routes, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return base, st, srv
}

func reviewAcceptWithScorer(
	st *localdolt.Store,
	newScorer func() localdolt.ContradictionScorer,
) storeipc.ReviewAcceptFunc {
	return func(ctx context.Context, id, expectedCommit string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
		return st.AcceptProposal(ctx, id, reviewer, localdolt.AcceptOptions{
			Force:                       force,
			ExpectedCommit:              expectedCommit,
			ValidateContradictionConfig: func() error { return nil },
			OpenContradictionScorer: func(context.Context) (localdolt.ContradictionScorer, error) {
				return newScorer(), nil
			},
		})
	}
}

func readFactSnapshot(t *testing.T, st store.Store, id string) localdolt.FactSnapshot {
	t.Helper()
	rows, err := st.Query(context.Background(),
		"SELECT id, `key`, value, source, kind, evidence, verified_at, created_at, superseded_by "+
			"FROM facts AS OF 'main' WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("fact %s is missing", id)
	}
	var fact localdolt.FactSnapshot
	var source, kind, evidence, superseded sql.NullString
	var verified, created sql.NullTime
	if err := rows.Scan(
		&fact.ID, &fact.Key, &fact.Value, &source, &kind, &evidence, &verified, &created, &superseded,
	); err != nil {
		t.Fatal(err)
	}
	fact.Source = nullString(source)
	fact.Kind = nullString(kind)
	fact.Evidence = nullString(evidence)
	fact.SupersededBy = nullString(superseded)
	if verified.Valid {
		stamp := verified.Time
		fact.VerifiedAt = &stamp
	}
	if created.Valid {
		fact.CreatedAt = created.Time
	}
	return fact
}

func nullString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// TestClientWritesAndReadsThroughTheOwner covers the acceptance criterion
// that a second party's store operations are routed over the internal/ipc
// endpoint: the write must land in the owner's store, and the read must
// come back through the same endpoint.
func TestClientWritesAndReadsThroughTheOwner(t *testing.T) {
	ctx := context.Background()
	base, owner, _ := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	commit, err := client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY, v TEXT, n BIGINT)"},
			{SQL: "INSERT INTO probe (k, v, n) VALUES (?, ?, ?)", Args: []any{"msrv", "1.26.2", 7}},
		},
		// The probe table is not memory anyone wrote. Declaring that
		// keeps this a test of the transport, and proves the
		// declaration itself crosses the wire: an owner that did not
		// receive it would refuse the write (PRD §11.3).
		NoText:  true,
		Message: "propose fact msrv=1.26.2",
		Author:  storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	})
	if err != nil {
		t.Fatalf("commit through the endpoint: %v", err)
	}
	if commit.Hash == "" {
		t.Fatal("commit returned an empty hash")
	}
	if commit.RowsAffected != 1 {
		t.Fatalf("rows affected = %d, want 1", commit.RowsAffected)
	}

	grid, err := client.Query(ctx, storeipc.QueryRequest{
		SQL:  "SELECT v, n, NULL FROM probe WHERE k = ?",
		Args: []any{"msrv"},
	})
	if err != nil {
		t.Fatalf("query through the endpoint: %v", err)
	}
	if len(grid.Rows) != 1 || len(grid.Rows[0]) != 3 {
		t.Fatalf("result grid = %+v, want one row of three cells", grid.Rows)
	}
	if got := grid.Rows[0][0]; got == nil || *got != "1.26.2" {
		t.Fatalf("text cell = %v, want %q", got, "1.26.2")
	}
	// The integer must survive JSON as the integer it was, not as a float.
	if got := grid.Rows[0][1]; got == nil || *got != "7" {
		t.Fatalf("integer cell = %v, want %q", got, "7")
	}
	if grid.Rows[0][2] != nil {
		t.Fatalf("null cell = %v, want nil", *grid.Rows[0][2])
	}

	// The write really went into the owner's store, not somewhere the
	// endpoint invented.
	rows, err := owner.Query(ctx, "SELECT v FROM probe WHERE k = ?", "msrv")
	if err != nil {
		t.Fatalf("query the owner's store directly: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("the write routed over the endpoint is not in the owner's store (err %v)", rows.Err())
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if value != "1.26.2" {
		t.Fatalf("owner's store holds %q, want %q", value, "1.26.2")
	}
}

// TestDirectAndOwnerRoutedOperationsHaveParity exercises every shipped typed
// store family against one initialized owner. Direct calls use the owner's
// LocalStore; routed calls cross its authenticated endpoint, so any accidental
// second embedded open would instead fail on the lock this fixture holds.
func TestDirectAndOwnerRoutedOperationsHaveParity(t *testing.T) {
	ctx := context.Background()
	base, direct, _ := startOwner(t)
	if _, err := direct.Migrate(ctx); err != nil {
		t.Fatalf("migrate owner store: %v", err)
	}
	routed, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatalf("dial owner store: %v", err)
	}
	if err := routed.Open(ctx); err != nil {
		t.Fatalf("open routed store: %v", err)
	}

	directVersion, err := direct.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("direct schema version: %v", err)
	}
	routedVersion, err := routed.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("routed schema version: %v", err)
	}
	if directVersion != routedVersion || directVersion != store.LatestSchemaVersion() {
		t.Fatalf("schema versions direct=%d routed=%d latest=%d", directVersion, routedVersion, store.LatestSchemaVersion())
	}

	actor, err := memory.NormalizeActor("parity-agent")
	if err != nil {
		t.Fatalf("normalize actor: %v", err)
	}
	directMemory := memory.New(direct, actor)
	routedMemory := memory.New(routed, actor)
	if _, _, err := directMemory.AddTask(ctx, "direct parity task", "same initialized fixture"); err != nil {
		t.Fatalf("direct memory write: %v", err)
	}
	routedTask, routedCommit, err := routedMemory.AddTask(ctx, "routed parity task", "same initialized fixture")
	if err != nil {
		t.Fatalf("routed memory write: %v", err)
	}
	directTasks, err := directMemory.Tasks(ctx, memory.StatusAny)
	if err != nil {
		t.Fatalf("direct memory read: %v", err)
	}
	routedTasks, err := routedMemory.Tasks(ctx, memory.StatusAny)
	if err != nil {
		t.Fatalf("routed memory read: %v", err)
	}
	assertJSONParity(t, "memory reads", directTasks, routedTasks)
	if routedTask.ID == "" || routedCommit == "" {
		t.Fatalf("routed memory write returned task=%+v commit=%q", routedTask, routedCommit)
	}
	if got := queryString(t, direct, "SELECT committer FROM dolt_log WHERE commit_hash = ?", routedCommit); got != actor.Name {
		t.Fatalf("routed memory commit author = %q, want %q", got, actor.Name)
	}

	proposal := localdolt.Proposal{
		Rationale: "exercise direct and routed proposal operations",
		Actor:     testActor,
		Target:    localdolt.TargetRepo,
	}
	stagedFact, err := direct.ProposeFact(ctx, proposal, localdolt.Fact{
		Key: "routing.owner", Value: "the live owner carries store operations",
	})
	if err != nil {
		t.Fatalf("direct proposal write: %v", err)
	}
	assertPendingParity(t, ctx, direct, routed)
	directDiff, err := direct.ProposalDiff(ctx, stagedFact.ID)
	if err != nil {
		t.Fatalf("direct proposal diff: %v", err)
	}
	routedDiff, err := routed.ProposalDiff(ctx, stagedFact.ID)
	if err != nil {
		t.Fatalf("routed proposal diff: %v", err)
	}
	assertJSONParity(t, "proposal diffs", directDiff, routedDiff)
	if _, err := routed.ReviewAcceptExpected(ctx, stagedFact.ID, stagedFact.Commit, testActor, false); err != nil {
		t.Fatalf("routed review accept: %v", err)
	}

	stagedDecision, err := routed.ProposeDecision(ctx, proposal, localdolt.Decision{
		Title: "Route through the owner", Rationale: "embedded Dolt has one process owner",
	})
	if err != nil {
		t.Fatalf("routed proposal write: %v", err)
	}
	assertPendingParity(t, ctx, direct, routed)
	if _, err := routed.ReviewAccept(ctx, stagedDecision.ID, testActor, false); err != nil {
		t.Fatalf("routed review accept decision: %v", err)
	}
	commitBound, err := routed.ProposeFact(ctx, proposal, localdolt.Fact{
		Key: "routing.commit_bound", Value: "only the displayed commit may be accepted",
	})
	if err != nil {
		t.Fatalf("stage commit-bound proposal: %v", err)
	}
	if _, err := routed.ReviewAcceptExpected(ctx, commitBound.ID, "not-the-shown-commit", testActor, false); err == nil ||
		!strings.Contains(err.Error(), "nothing was promoted or removed") {
		t.Fatalf("routed expected-commit refusal = %v", err)
	}
	if _, err := routed.RejectProposal(ctx, commitBound.ID); err != nil {
		t.Fatalf("reject commit-bound proposal after refusal: %v", err)
	}

	directEmbedding, err := direct.EmbeddingSources(ctx)
	if err != nil {
		t.Fatalf("direct embedding sources: %v", err)
	}
	routedEmbedding, err := routed.EmbeddingSources(ctx)
	if err != nil {
		t.Fatalf("routed embedding sources: %v", err)
	}
	assertJSONParity(t, "embedding sources", directEmbedding, routedEmbedding)
	directRecall, err := direct.RecallSources(ctx)
	if err != nil {
		t.Fatalf("direct recall sources: %v", err)
	}
	routedRecall, err := routed.RecallSources(ctx)
	if err != nil {
		t.Fatalf("routed recall sources: %v", err)
	}
	assertJSONParity(t, "recall sources", directRecall, routedRecall)
	directFTS, err := direct.RecallFTS(ctx, "owner", []string{"fact", "decision", "task"})
	if err != nil {
		t.Fatalf("direct recall FTS: %v", err)
	}
	routedFTS, err := routed.RecallFTS(ctx, "owner", []string{"fact", "decision", "task"})
	if err != nil {
		t.Fatalf("routed recall FTS: %v", err)
	}
	assertJSONParity(t, "recall FTS", directFTS, routedFTS)
	directSearch, err := direct.SearchDecisions(ctx, "owner", 10)
	if err != nil {
		t.Fatalf("direct search: %v", err)
	}
	routedSearch, err := routed.SearchDecisions(ctx, "owner", 10)
	if err != nil {
		t.Fatalf("routed search: %v", err)
	}
	assertJSONParity(t, "search", directSearch, routedSearch)
	directChanged, err := direct.LastChanged(ctx, "fact", stagedFact.RowID)
	if err != nil {
		t.Fatalf("direct provenance: %v", err)
	}
	routedChanged, err := routed.LastChanged(ctx, "fact", stagedFact.RowID)
	if err != nil {
		t.Fatalf("routed provenance: %v", err)
	}
	assertJSONParity(t, "provenance", directChanged, routedChanged)
	overwrite, err := routed.ProposeFactResolution(ctx, proposal, readFactSnapshot(t, direct, stagedFact.RowID), localdolt.Fact{
		Key: "routing.owner", Value: "the proposed overwrite is routed through the owner",
	}, localdolt.FactResolutionOverwrite)
	if err != nil {
		t.Fatalf("routed overwrite proposal: %v", err)
	}
	directOverwriteDiff, err := direct.ProposalDiff(ctx, overwrite.ID)
	if err != nil {
		t.Fatalf("direct overwrite diff: %v", err)
	}
	routedOverwriteDiff, err := routed.ProposalDiff(ctx, overwrite.ID)
	if err != nil {
		t.Fatalf("routed overwrite diff: %v", err)
	}
	assertJSONParity(t, "overwrite diffs", directOverwriteDiff, routedOverwriteDiff)
	if _, err := routed.RejectProposal(ctx, overwrite.ID); err != nil {
		t.Fatalf("reject routed overwrite: %v", err)
	}
	supersede, err := routed.ProposeSupersede(ctx, proposal, stagedFact.RowID, localdolt.Fact{
		Key: "routing.owner", Value: "the replacement is staged through the owner",
	})
	if err != nil {
		t.Fatalf("routed supersede proposal: %v", err)
	}
	directSupersedeDiff, err := direct.ProposalDiff(ctx, supersede.ID)
	if err != nil {
		t.Fatalf("direct supersede diff: %v", err)
	}
	routedSupersedeDiff, err := routed.ProposalDiff(ctx, supersede.ID)
	if err != nil {
		t.Fatalf("routed supersede diff: %v", err)
	}
	assertJSONParity(t, "supersede diffs", directSupersedeDiff, routedSupersedeDiff)
	if _, err := routed.RejectProposal(ctx, supersede.ID); err != nil {
		t.Fatalf("reject routed supersede: %v", err)
	}

	toReject, err := routed.ProposeFact(ctx, proposal, localdolt.Fact{Key: "routing.reject", Value: "discard me"})
	if err != nil {
		t.Fatalf("stage rejection probe: %v", err)
	}
	if _, err := routed.RejectProposal(ctx, toReject.ID); err != nil {
		t.Fatalf("routed review reject: %v", err)
	}
	assertPendingParity(t, ctx, direct, routed)
}

func TestDirectAndOwnerRoutedReviewTrustBoundaryParity(t *testing.T) {
	t.Run("contradiction refusal and force", func(t *testing.T) {
		ctx := context.Background()
		var directAccept storeipc.ReviewAcceptFunc
		base, direct, _ := startOwnerConfigured(t, func(st *localdolt.Store) storeipc.ReviewAcceptFunc {
			directAccept = reviewAcceptWithScorer(st, func() localdolt.ContradictionScorer { return fixedScorer(2) })
			return directAccept
		}, nil)
		if _, err := direct.Migrate(ctx); err != nil {
			t.Fatalf("migrate owner store: %v", err)
		}
		routed, err := storeipc.DialOwnerStore(base)
		if err != nil {
			t.Fatalf("dial owner store: %v", err)
		}
		if _, err := direct.Commit(ctx, store.CommitRequest{
			Statements: []store.Statement{{
				SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
				Args: []any{"01J0000000000000000000BASE", "routing.baseline", "durable baseline", "user", "2026-08-30 12:00:00"},
			}},
			Text: []string{"routing.baseline", "durable baseline"}, Message: "record the durable baseline", Author: testActor,
		}); err != nil {
			t.Fatalf("seed durable fact: %v", err)
		}

		proposal := localdolt.Proposal{Rationale: "exercise contradiction parity", Actor: testActor, Target: localdolt.TargetRepo}
		directStaged, err := direct.ProposeFact(ctx, proposal, localdolt.Fact{Key: "routing.direct", Value: "direct claim"})
		if err != nil {
			t.Fatalf("stage direct contradiction: %v", err)
		}
		routedStaged, err := routed.ProposeFact(ctx, proposal, localdolt.Fact{Key: "routing.routed", Value: "routed claim"})
		if err != nil {
			t.Fatalf("stage routed contradiction: %v", err)
		}

		_, directErr := directAccept(ctx, directStaged.ID, "", testActor, false)
		_, routedErr := routed.ReviewAccept(ctx, routedStaged.ID, testActor, false)
		for label, acceptErr := range map[string]error{"direct": directErr, "routed": routedErr} {
			if acceptErr == nil || !strings.Contains(acceptErr.Error(), "contradict") {
				t.Fatalf("%s contradiction error = %v, want the contradiction refusal", label, acceptErr)
			}
		}
		assertPendingParity(t, ctx, direct, routed)

		directResult, err := directAccept(ctx, directStaged.ID, "", testActor, true)
		if err != nil {
			t.Fatalf("force direct accept: %v", err)
		}
		routedResult, err := routed.ReviewAccept(ctx, routedStaged.ID, testActor, true)
		if err != nil {
			t.Fatalf("force routed accept: %v", err)
		}
		if directResult.Commit == "" || routedResult.Commit == "" ||
			directResult.Proposal.ID != directStaged.ID || routedResult.Proposal.ID != routedStaged.ID {
			t.Fatalf("forced results direct=%+v routed=%+v", directResult, routedResult)
		}
		assertPendingParity(t, ctx, direct, routed)
	})

	t.Run("accept serialization", func(t *testing.T) {
		ctx := context.Background()
		var directAccept storeipc.ReviewAcceptFunc
		base, direct, _ := startOwnerConfigured(t, func(st *localdolt.Store) storeipc.ReviewAcceptFunc {
			directAccept = reviewAcceptWithScorer(st, func() localdolt.ContradictionScorer {
				return serializedAcceptScorer{}
			})
			return directAccept
		}, nil)
		if _, err := direct.Migrate(ctx); err != nil {
			t.Fatalf("migrate owner store: %v", err)
		}
		routed, err := storeipc.DialOwnerStore(base)
		if err != nil {
			t.Fatalf("dial owner store: %v", err)
		}
		if _, err := direct.Commit(ctx, store.CommitRequest{
			Statements: []store.Statement{{
				SQL:  "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?)",
				Args: []any{"01J0000000000000000000BASE", "serial.baseline", "durable baseline", "user", "2026-08-30 12:00:00"},
			}},
			Text: []string{"serial.baseline", "durable baseline"}, Message: "record serialization baseline", Author: testActor,
		}); err != nil {
			t.Fatalf("seed serialization baseline: %v", err)
		}
		proposal := localdolt.Proposal{Rationale: "exercise accept serialization", Actor: testActor, Target: localdolt.TargetRepo}
		first, err := direct.ProposeFact(ctx, proposal, localdolt.Fact{Key: "serial.first", Value: "serial candidate first"})
		if err != nil {
			t.Fatalf("stage direct serialized accept: %v", err)
		}
		second, err := routed.ProposeFact(ctx, proposal, localdolt.Fact{Key: "serial.second", Value: "serial candidate second"})
		if err != nil {
			t.Fatalf("stage routed serialized accept: %v", err)
		}

		type outcome struct {
			result localdolt.AcceptResult
			err    error
		}
		start := make(chan struct{})
		outcomes := make(chan outcome, 2)
		go func() {
			<-start
			result, acceptErr := directAccept(ctx, first.ID, "", testActor, false)
			outcomes <- outcome{result: result, err: acceptErr}
		}()
		go func() {
			<-start
			result, acceptErr := routed.ReviewAccept(ctx, second.ID, testActor, false)
			outcomes <- outcome{result: result, err: acceptErr}
		}()
		close(start)

		accepted, blocked := 0, 0
		for range 2 {
			got := <-outcomes
			switch {
			case got.err == nil && got.result.Commit != "":
				accepted++
			case got.err != nil && strings.Contains(got.err.Error(), "contradict"):
				blocked++
			default:
				t.Fatalf("serialized accept outcome = %+v, want a merge or contradiction", got)
			}
		}
		if accepted != 1 || blocked != 1 {
			t.Fatalf("serialized accepts merged=%d blocked=%d, want 1/1", accepted, blocked)
		}
		if got := queryInt(t, direct, "SELECT COUNT(*) FROM facts"); got != 2 {
			t.Fatalf("serialized accepts left %d durable facts, want baseline plus one accepted claim", got)
		}
		pending, err := direct.PendingProposals(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatalf("serialized accepts left pending=%+v err=%v, want one blocked proposal", pending, err)
		}
		assertPendingParity(t, ctx, direct, routed)
	})

	t.Run("validated supersede bypass", func(t *testing.T) {
		ctx := context.Background()
		var probes atomic.Int32
		var directAccept storeipc.ReviewAcceptFunc
		base, direct, _ := startOwnerConfigured(t, func(st *localdolt.Store) storeipc.ReviewAcceptFunc {
			directAccept = func(ctx context.Context, id, expectedCommit string, reviewer store.Actor, force bool) (localdolt.AcceptResult, error) {
				return st.AcceptProposal(ctx, id, reviewer, localdolt.AcceptOptions{
					Force:          force,
					ExpectedCommit: expectedCommit,
					ValidateContradictionConfig: func() error {
						probes.Add(1)
						return nil
					},
					OpenContradictionScorer: func(context.Context) (localdolt.ContradictionScorer, error) {
						probes.Add(1)
						return fixedScorer(2), nil
					},
				})
			}
			return directAccept
		}, nil)
		if _, err := direct.Migrate(ctx); err != nil {
			t.Fatalf("migrate owner store: %v", err)
		}
		routed, err := storeipc.DialOwnerStore(base)
		if err != nil {
			t.Fatalf("dial owner store: %v", err)
		}
		if _, err := direct.Commit(ctx, store.CommitRequest{
			Statements: []store.Statement{{
				SQL: "INSERT INTO facts (id, `key`, value, source, created_at) VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)",
				Args: []any{
					"01J0000000000000000000SUPA", "supersede.direct", "old direct", "user", "2026-08-30 12:00:00",
					"01J0000000000000000000SUPB", "supersede.routed", "old routed", "user", "2026-08-30 12:00:00",
				},
			}},
			Text:    []string{"supersede.direct", "old direct", "supersede.routed", "old routed"},
			Message: "record supersede baselines", Author: testActor,
		}); err != nil {
			t.Fatalf("seed supersede baselines: %v", err)
		}
		proposal := localdolt.Proposal{Rationale: "exercise supersede parity", Actor: testActor, Target: localdolt.TargetRepo}
		directStaged, err := direct.ProposeSupersede(ctx, proposal, "01J0000000000000000000SUPA",
			localdolt.Fact{Key: "supersede.direct", Value: "new direct"})
		if err != nil {
			t.Fatalf("stage direct supersede: %v", err)
		}
		routedStaged, err := routed.ProposeSupersede(ctx, proposal, "01J0000000000000000000SUPB",
			localdolt.Fact{Key: "supersede.routed", Value: "new routed"})
		if err != nil {
			t.Fatalf("stage routed supersede: %v", err)
		}
		if _, err := directAccept(ctx, directStaged.ID, "", testActor, false); err != nil {
			t.Fatalf("accept direct supersede: %v", err)
		}
		if _, err := routed.ReviewAccept(ctx, routedStaged.ID, testActor, false); err != nil {
			t.Fatalf("accept routed supersede: %v", err)
		}
		if got := probes.Load(); got != 0 {
			t.Fatalf("valid supersedes invoked %d contradiction callback(s), want the validated bypass", got)
		}
		if got := queryInt(t, direct, "SELECT COUNT(*) FROM facts WHERE superseded_by IS NOT NULL"); got != 2 {
			t.Fatalf("accepted supersedes linked %d old rows, want 2", got)
		}
		assertPendingParity(t, ctx, direct, routed)
	})
}

type serializedAcceptScorer struct{}

func (serializedAcceptScorer) Rerank(_, passage string) (float32, error) {
	if strings.Contains(passage, "serial candidate") {
		return 2, nil
	}
	return -100, nil
}

func (serializedAcceptScorer) Close() error { return nil }

// TestOwnerRoutedCommandRecordKeepsCommitAndReadbackTogether is deterministic
// against the old split route: the wrapper holds both commit responses until
// both writes have landed, so separate follow-up queries would both read the
// last writer. One typed owner operation returns each caller's own row instead.
func TestOwnerRoutedCommandRecordKeepsCommitAndReadbackTogether(t *testing.T) {
	ctx := context.Background()
	base, direct, _ := startOwnerConfigured(t, nil, interleaveSplitCommandRequests)
	if _, err := direct.Migrate(ctx); err != nil {
		t.Fatalf("migrate owner store: %v", err)
	}
	clients := make([]*storeipc.OwnerStore, 2)
	for i := range clients {
		var err error
		clients[i], err = storeipc.DialOwnerStore(base)
		if err != nil {
			t.Fatalf("dial owner store %d: %v", i, err)
		}
	}

	type result struct {
		command memory.Command
		commit  string
		err     error
	}
	cmdlines := []string{"go test ./first/...", "go test ./second/..."}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := range clients {
		go func(i int) {
			<-start
			command, commit, err := clients[i].RecordCommand(ctx,
				memory.Actor{Name: fmt.Sprintf("agent:client-%d", i), Raw: fmt.Sprintf("client-%d", i)},
				"test", cmdlines[i], 0)
			results <- result{command: command, commit: commit, err: err}
		}(i)
	}
	close(start)

	seen := map[string]memory.Command{}
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("record routed command: %v", got.err)
		}
		if got.commit == "" {
			t.Fatal("record routed command returned an empty commit")
		}
		seen[got.command.Cmdline] = got.command
	}
	for _, cmdline := range cmdlines {
		if got, ok := seen[cmdline]; !ok || got.Kind != "test" {
			t.Fatalf("routed command results = %+v, want the caller's %q row", seen, cmdline)
		}
	}
	first, second := seen[cmdlines[0]].SuccessCount, seen[cmdlines[1]].SuccessCount
	if first == second || first+second != 3 {
		t.Fatalf("routed command success counts = %d and %d, want serialized counts 1 and 2", first, second)
	}
}

func interleaveSplitCommandRequests(inner http.Handler) http.Handler {
	var mu sync.Mutex
	commits := 0
	bothCommitted := make(chan struct{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != storeipc.CommitPath {
			inner.ServeHTTP(w, r)
			return
		}

		recorded := httptest.NewRecorder()
		inner.ServeHTTP(recorded, r)
		mu.Lock()
		commits++
		if commits == 2 {
			close(bothCommitted)
		}
		mu.Unlock()
		<-bothCommitted
		for key, values := range recorded.Header() {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	})
}

func assertPendingParity(t *testing.T, ctx context.Context, direct *localdolt.Store, routed *storeipc.OwnerStore) {
	t.Helper()
	directPending, err := direct.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("direct pending proposals: %v", err)
	}
	routedPending, err := routed.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("routed pending proposals: %v", err)
	}
	assertJSONParity(t, "pending proposals", directPending, routedPending)
}

func assertJSONParity(t *testing.T, label string, direct, routed any) {
	t.Helper()
	directJSON, err := json.Marshal(direct)
	if err != nil {
		t.Fatalf("encode direct %s: %v", label, err)
	}
	routedJSON, err := json.Marshal(routed)
	if err != nil {
		t.Fatalf("encode routed %s: %v", label, err)
	}
	if !bytes.Equal(directJSON, routedJSON) {
		t.Fatalf("%s differ:\ndirect: %s\nrouted: %s", label, directJSON, routedJSON)
	}
}

func queryString(t *testing.T, st *localdolt.Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query string: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query string returned no row: %v", rows.Err())
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("scan string: %v", err)
	}
	return value
}

func queryInt(t *testing.T, st *localdolt.Store, query string, args ...any) int {
	t.Helper()
	value := queryString(t, st, query, args...)
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("parse query result %q as an integer: %v", value, err)
	}
	return parsed
}

func TestOwnerRoutedWritePreservesDenyListAndAtomicCommit(t *testing.T) {
	ctx := context.Background()
	base, direct, _ := startOwner(t)
	if _, err := direct.Migrate(ctx); err != nil {
		t.Fatalf("migrate owner store: %v", err)
	}
	routed, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatalf("dial owner store: %v", err)
	}

	before := queryInt(t, direct, "SELECT COUNT(*) FROM dolt_log")
	actor, err := memory.NormalizeActor("routed-writer")
	if err != nil {
		t.Fatalf("normalize actor: %v", err)
	}
	if _, _, err := memory.New(routed, actor).AddTask(ctx, "one routed commit", "one row"); err != nil {
		t.Fatalf("routed task write: %v", err)
	}
	after := queryInt(t, direct, "SELECT COUNT(*) FROM dolt_log")
	if after != before+1 {
		t.Fatalf("routed memory write added %d commits, want 1", after-before)
	}

	config := filepath.Join(base, ".memdolt", "config.toml")
	if err := os.WriteFile(config, []byte("[deny_list]\npatterns = ['blocked routed text']\n"), 0o600); err != nil {
		t.Fatalf("write deny-list: %v", err)
	}
	if err := routed.CheckWriteText(ctx, []string{"blocked routed text"}); err == nil || !strings.Contains(err.Error(), "blocked routed text") {
		t.Fatalf("routed deny-list preflight error = %v, want named rule", err)
	}
	before = queryInt(t, direct, "SELECT COUNT(*) FROM dolt_log")
	_, _, err = memory.New(routed, actor).AddTask(ctx, "blocked routed text", "must not land")
	if err == nil || !strings.Contains(err.Error(), "blocked routed text") {
		t.Fatalf("routed denied write error = %v, want named deny-list rule", err)
	}
	if afterDenied := queryInt(t, direct, "SELECT COUNT(*) FROM dolt_log"); afterDenied != before {
		t.Fatalf("denied routed write moved history from %d to %d", before, afterDenied)
	}
}

func TestUnknownRoutedProposalOutcomeIsNotRetried(t *testing.T) {
	ctx := context.Background()
	base, direct, _ := startOwnerWrapping(t, dropFirstAnswer)
	if _, err := direct.Migrate(ctx); err != nil {
		t.Fatalf("migrate owner store: %v", err)
	}
	routed, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatalf("dial owner store: %v", err)
	}
	_, err = routed.ProposeFact(ctx, localdolt.Proposal{
		Rationale: "drop the answer", Actor: testActor, Target: localdolt.TargetRepo,
	}, localdolt.Fact{Key: "routing.unknown", Value: "the proposal lands once"})
	if err == nil || storeipc.IsOwnerRefusal(err) {
		t.Fatalf("lost proposal answer error = %v, want unknown transport outcome", err)
	}
	pending, err := direct.PendingProposals(ctx)
	if err != nil {
		t.Fatalf("read proposals after lost answer: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("lost-answer proposal was applied %d times, want exactly once", len(pending))
	}
}

func TestQueryEndpointEnforcesTheStoreReadOnlyBoundary(t *testing.T) {
	ctx := context.Background()
	base, owner, _ := startOwner(t)
	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE query_probe (k VARCHAR(16) PRIMARY KEY, v TEXT)"},
			{SQL: "INSERT INTO query_probe (k, v) VALUES (?, ?)", Args: []any{"kept", "original"}},
		},
		NoText:  true,
		Message: "create the IPC query boundary probe",
		Author:  storeipc.Actor{Name: "user", Email: "user@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	for name, query := range map[string]string{
		"insert":           "/* not a SELECT */ INSERT INTO query_probe (k, v) VALUES ('added', 'write')",
		"update":           "UPDATE query_probe SET v = 'changed' WHERE k = 'kept'",
		"delete":           "DELETE FROM query_probe WHERE k = 'kept'",
		"ddl":              "CREATE TABLE query_created (k INT PRIMARY KEY)",
		"session setting":  "SET @query_probe = 1",
		"select into":      "SELECT v INTO @query_probe FROM query_probe",
		"stored procedure": "CALL DOLT_BRANCH('query-probe')",
		"multiple":         "SELECT v FROM query_probe; DELETE FROM query_probe",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Query(ctx, storeipc.QueryRequest{SQL: query})
			if err == nil {
				t.Fatalf("Query accepted %s", query)
			}
			if !storeipc.IsOwnerRefusal(err) {
				t.Fatalf("Query refusal was reported as a transport failure: %v", err)
			}
			if !strings.Contains(err.Error(), "exactly one read-only SELECT or SHOW") {
				t.Fatalf("Query refusal %q does not describe the enforced boundary", err)
			}
		})
	}

	grid, err := client.Query(ctx, storeipc.QueryRequest{
		SQL:  "/* bound */ SELECT v FROM query_probe WHERE k = ?",
		Args: []any{"kept"},
	})
	if err != nil {
		t.Fatalf("bound SELECT through endpoint: %v", err)
	}
	if len(grid.Rows) != 1 || grid.Rows[0][0] == nil || *grid.Rows[0][0] != "original" {
		t.Fatalf("bound SELECT result = %+v, want original", grid.Rows)
	}
	if _, err := client.Query(ctx, storeipc.QueryRequest{SQL: "SHOW CREATE TABLE query_probe"}); err != nil {
		t.Fatalf("SHOW through endpoint: %v", err)
	}

	rows, err := owner.Query(ctx, "SELECT COUNT(*), MIN(v) FROM query_probe")
	if err != nil {
		t.Fatalf("inspect probe: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("inspect probe returned no row: %v", rows.Err())
	}
	var count int
	var value string
	if err := rows.Scan(&count, &value); err != nil {
		t.Fatalf("scan probe: %v", err)
	}
	if count != 1 || value != "original" {
		t.Fatalf("probe after refusals = (%d, %q), want (1, %q)", count, value, "original")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("inspect probe: %v", err)
	}
}

// TestARefusedWriteIsDistinguishableFromALostAnswer covers the property the
// M0 rig's accounting rests on: a write the owner refused must be
// reportable as "did not happen", and must not look like a request whose
// outcome is unknown.
func TestARefusedWriteIsDistinguishableFromALostAnswer(t *testing.T) {
	base, _, _ := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, err = client.Commit(context.Background(), storeipc.CommitRequest{
		Statements: []storeipc.Statement{{SQL: "INSERT INTO no_such_table (k) VALUES (?)", Args: []any{"x"}}},
		NoText:     true,
		Message:    "write to a table that does not exist",
		Author:     storeipc.Actor{Name: "user", Email: "user@memdolt.invalid"},
	})
	if err == nil {
		t.Fatal("a write against a missing table was reported as committed")
	}
	if !storeipc.IsOwnerRefusal(err) {
		t.Fatalf("error = %v, want one the caller can read as an owner refusal", err)
	}

	var status *ipc.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v does not carry the owner's status", err)
	}
	if status.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", status.Code, http.StatusInternalServerError)
	}
	if !bytes.Contains([]byte(status.Body), []byte("no_such_table")) {
		t.Fatalf("owner's answer %q does not say why the write failed", status.Body)
	}
}

// dropFirstAnswer runs the owner's routes and then aborts the connection
// before the first answer is written, which is the failure rig 1 caught:
// the owner applies the write and the client never learns that it did
// (docs/spikes/m0-rig1.md, F3).
func dropFirstAnswer(inner http.Handler) http.Handler {
	var dropped atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !dropped.CompareAndSwap(false, true) {
			inner.ServeHTTP(w, r)
			return
		}
		// The answer is written to a recorder that goes nowhere, and then
		// the connection is closed without one; net/http closes it quietly
		// on ErrAbortHandler.
		inner.ServeHTTP(httptest.NewRecorder(), r)
		panic(http.ErrAbortHandler)
	})
}

// TestALostAnswerIsNotResubmitted covers the acceptance criterion that the
// client machinery never resubmits the operation that hit a transport
// failure. A write over IPC is at-least-once, so a resubmission is a
// duplicate write, and M0 has no way to make one harmless.
func TestALostAnswerIsNotResubmitted(t *testing.T) {
	ctx := context.Background()
	base, owner, _ := startOwnerWrapping(t, dropFirstAnswer)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, err = client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{
			{SQL: "CREATE TABLE probe (k VARCHAR(64) PRIMARY KEY)"},
			{SQL: "INSERT INTO probe (k) VALUES (?)", Args: []any{"lost-answer"}},
		},
		NoText:  true,
		Message: "a write whose answer never arrives",
		Author:  storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	})
	if err == nil {
		t.Fatal("a write whose answer was dropped was reported as committed")
	}
	// The owner never answered, so this is not a refusal: the outcome is
	// unknown to the caller, and that is the whole point.
	if storeipc.IsOwnerRefusal(err) {
		t.Fatalf("a lost answer was reported as an owner refusal: %v", err)
	}
	// The owner is still live, so the caller must not be told the store is
	// free to open.
	if errors.Is(err, ipc.ErrNoLiveOwner) {
		t.Fatalf("a live owner was reported as no live owner: %v", err)
	}

	// The same client, without being rebuilt, reaches the owner again — and
	// carries a second, distinct write rather than the one that failed.
	if _, err := client.Commit(ctx, storeipc.CommitRequest{
		Statements: []storeipc.Statement{{SQL: "INSERT INTO probe (k) VALUES (?)", Args: []any{"answered"}}},
		NoText:     true,
		Message:    "a write whose answer arrives",
		Author:     storeipc.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"},
	}); err != nil {
		t.Fatalf("commit after the lost answer: %v", err)
	}

	rows, err := owner.Query(ctx, "SELECT k, COUNT(*) FROM probe GROUP BY k ORDER BY k")
	if err != nil {
		t.Fatalf("query the owner's store directly: %v", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[string]int64{}
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			t.Fatalf("scan: %v", err)
		}
		applied[key] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	// The dropped answer's write did land — the table it created is being
	// read here — and it landed exactly once.
	if applied["lost-answer"] != 1 {
		t.Fatalf("the write whose answer was lost was applied %d times, want exactly 1 (rows %v)",
			applied["lost-answer"], applied)
	}
	if applied["answered"] != 1 {
		t.Fatalf("the write made after the failure was applied %d times, want exactly 1 (rows %v)",
			applied["answered"], applied)
	}
}

// TestAnOperationAfterTheOwnerStopsSaysTheStoreIsFree covers, at the layer a
// CLI uses, the acceptance criterion that a transport failure against a
// repository no live owner holds is reported as such: PRD §5.2's design
// response then has the caller open the store directly.
func TestAnOperationAfterTheOwnerStopsSaysTheStoreIsFree(t *testing.T) {
	base, _, srv := startOwner(t)

	client, err := storeipc.Dial(base)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close the owner's endpoint: %v", err)
	}

	_, err = client.Query(context.Background(), storeipc.QueryRequest{SQL: "SELECT 1"})
	if !errors.Is(err, ipc.ErrNoLiveOwner) {
		t.Fatalf("error = %v, want one matching ipc.ErrNoLiveOwner", err)
	}
	if storeipc.IsOwnerRefusal(err) {
		t.Fatalf("a transport failure was reported as an owner refusal: %v", err)
	}
}

// TestStoreRoutesAreBehindTheTokenCheck covers the acceptance criterion
// that routing store operations over the endpoint does not widen its
// security posture: the routes carry writes, so an untokened request must
// be refused before it reaches the store.
func TestStoreRoutesAreBehindTheTokenCheck(t *testing.T) {
	ctx := context.Background()
	_, owner, srv := startOwner(t)

	for _, path := range []string{storeipc.CommitPath, storeipc.QueryPath, storeipc.OperationPath} {
		body := `{"statements":[{"sql":"CREATE TABLE untokened (k VARCHAR(8) PRIMARY KEY)"}],` +
			`"message":"untokened","author":{"name":"user","email":"user@memdolt.invalid"},` +
			`"sql":"SELECT 1"}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+srv.Addr()+path,
			bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	// And the refused write never reached the store.
	rows, err := owner.Query(ctx, "SHOW TABLES LIKE 'untokened'")
	if err != nil {
		t.Fatalf("show tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("an untokened request created a table")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("show tables rows: %v", err)
	}
}
