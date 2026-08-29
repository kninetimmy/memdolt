package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
	"github.com/kninetimmy/memdolt/internal/storeipc"
)

// TestEveryStoreDependentCLIUsesTheLiveOwner runs every shipped command
// family while serveStore holds the advisory lock. Success therefore proves
// the command routed through the authenticated endpoint instead of attempting
// a second embedded open; init is the lifecycle exception and must give the
// explicit stop-owner remedy.
func TestEveryStoreDependentCLIUsesTheLiveOwner(t *testing.T) {
	base := initStore(t)
	serveStore(t, base)
	ctx := context.Background()

	// The derived side-store remains local, but both index verbs obtain their
	// durable source rows through the owner. Rebuild runs before rows exist so
	// this routing test never needs to provision an inference model.
	runMemdolt(t, "index", "rebuild", "--dir", base, "--json")
	runMemdolt(t, "index", "status", "--dir", base, "--json")

	first := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "routed task", "--notes", "through owner", "--actor", "routing-agent", "--dir", base, "--json"))
	second := decodeJSON[taskInfo](t, runMemdolt(t,
		"task", "add", "blocked routed task", "--dir", base, "--json"))
	runMemdolt(t, "task", "list", "--dir", base, "--json")
	runMemdolt(t, "task", "done", first.ID, "--dir", base, "--json")
	runMemdolt(t, "task", "block", second.ID, "--notes", "owner route", "--dir", base, "--json")
	runMemdolt(t, "note", "add", "routed note", "--dir", base, "--json")
	runMemdolt(t, "note", "list", "--dir", base, "--json")
	runMemdolt(t, "command", "record", "test", "go test ./...", "--dir", base, "--json")
	runMemdolt(t, "command", "get", "test", "--dir", base, "--json")
	runMemdolt(t, "state", "set", "routed status", "--dir", base, "--json")
	runMemdolt(t, "state", "show", "--dir", base, "--json")
	runMemdolt(t, "arch", "set", "routed architecture", "--dir", base, "--json")
	runMemdolt(t, "arch", "show", "--dir", base, "--json")

	owner, err := storeipc.DialOwnerStore(base)
	if err != nil {
		t.Fatalf("dial owner for proposal fixture: %v", err)
	}
	proposal := localdolt.Proposal{
		Rationale: "exercise routed review commands",
		Actor:     store.Actor{Name: "agent:routing-agent", Email: "routing-agent@memdolt.invalid"},
		Target:    localdolt.TargetRepo,
	}
	decision, err := owner.ProposeDecision(ctx, proposal, localdolt.Decision{
		Title: "Use the live owner", Rationale: "embedded Dolt has one owner",
	})
	if err != nil {
		t.Fatalf("stage decision through owner: %v", err)
	}
	runMemdolt(t, "review", "list", "--dir", base, "--json")
	runMemdolt(t, "review", "show", decision.ID, "--dir", base, "--json")
	runMemdolt(t, "review", "accept", decision.ID, "--dir", base, "--json")

	stale, err := owner.ProposeFact(ctx, proposal, localdolt.Fact{Key: "routing.stale", Value: "report then reject"})
	if err != nil {
		t.Fatalf("stage stale probe: %v", err)
	}
	runMemdolt(t, "review", "stale", "--older-than", "0s", "--dir", base, "--json")
	runMemdolt(t, "review", "reject", stale.ID, "--dir", base, "--json")
	if _, err := owner.ProposeFact(ctx, proposal, localdolt.Fact{Key: "routing.expire", Value: "expire me"}); err != nil {
		t.Fatalf("stage expiry probe: %v", err)
	}
	runMemdolt(t, "review", "expire", "--older-than", "0s", "--dir", base, "--json")

	runMemdolt(t, "recall", "owner", "--mode", "fts", "--dir", base, "--json")
	runMemdolt(t, "search", "owner", "--dir", base, "--json")
	runMemdolt(t, "doctor", "--dir", base, "--json")

	golden, err := filepath.Abs(filepath.Join("..", "..", "tests", "golden", "retrieval_golden.json"))
	if err != nil {
		t.Fatalf("resolve golden path: %v", err)
	}
	if got := runMemdoltErr(t, "eval", "retrieval", "--mode", "fts", "--golden", golden, "--dir", base, "--json"); !strings.Contains(got, "below the recorded baseline") {
		t.Fatalf("routed evaluator error = %q, want the expected fixture-quality failure", got)
	}

	if got := runMemdoltErr(t, "init", "--dir", base); !strings.Contains(got, "stop the owner and rerun `memdolt init`") {
		t.Fatalf("init with a live owner error = %q, want the stop-owner remedy", got)
	}

	// A routed read still attributes the earlier agent write exactly as the
	// direct lane does; this also keeps the imported memory type in the same
	// response contract exercised by existing CLI tests.
	tasks := decodeJSON[taskList](t, runMemdolt(t,
		"task", "list", "--status", memory.StatusAny, "--dir", base, "--json"))
	if len(tasks.Tasks) != 2 {
		t.Fatalf("routed final task list has %d rows, want 2", len(tasks.Tasks))
	}
}
