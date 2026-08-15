package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// The review verbs are the CLI over PRD §7's gate. There is no CLI verb
// that stages a proposal — propose_* is the MCP surface of §11.1 — so
// these tests stage through the store and then review from the command
// line, which is also the shape of the real workflow: an agent proposes,
// a human at a terminal promotes.

var cliStagingActor = store.Actor{Name: "agent:claude-code", Email: "agent-claude-code@memdolt.invalid"}

// TestReviewCLIAcceptRoundTrip covers the operator's path from a listing to
// a durable row: list, show the single-commit diff, accept, and see the
// proposal gone from the listing.
func TestReviewCLIAcceptRoundTrip(t *testing.T) {
	base := initStore(t)
	staged := stageFact(t, base, "the race lane is the one CI runs",
		localdolt.Fact{Key: "build.command", Value: "go test -race ./..."})

	listed := decodeJSON[proposalList](t, runMemdolt(t, "review", "list", "--dir", base, "--json"))
	if len(listed.Proposals) != 1 || listed.Proposals[0].ID != staged.ID {
		t.Fatalf("`review list` returned %+v, want the staged proposal %s", listed.Proposals, staged.ID)
	}
	if human := runMemdolt(t, "review", "list", "--dir", base); !strings.Contains(human, staged.ID) ||
		!strings.Contains(human, "the race lane is the one CI runs") {
		t.Fatalf("`review list` printed %q, want the id and the rationale", human)
	}

	// show renders the one commit that is the proposal's whole payload.
	shown := runMemdolt(t, "review", "show", staged.ID, "--dir", base)
	for _, want := range []string{staged.ID, staged.Branch, "added facts", "+ key: build.command", "+ value: go test -race ./..."} {
		if !strings.Contains(shown, want) {
			t.Fatalf("`review show` printed %q, want it to contain %q", shown, want)
		}
	}

	accepted := decodeJSON[localdolt.AcceptResult](t, runMemdolt(t,
		"review", "accept", staged.ID, "--dir", base, "--json"))
	if accepted.Commit == "" || accepted.Proposal.ID != staged.ID {
		t.Fatalf("`review accept` returned %+v", accepted)
	}

	// The row is durable, the branch is gone, and the merge commit records
	// the review under the person who ran the command (PRD §3.1).
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()
	if got := queryString(t, st, "SELECT value FROM facts WHERE live_key = ?", "build.command"); got != "go test -race ./..." {
		t.Fatalf("the accepted fact reads %q on main", got)
	}
	if got := queryString(t, st, "SELECT message FROM dolt_log ORDER BY date DESC LIMIT 1"); got != "review accept fact "+staged.ID {
		t.Fatalf("the merge commit message is %q", got)
	}
	if got := queryString(t, st, "SELECT committer FROM dolt_log ORDER BY date DESC LIMIT 1"); got != "user" {
		t.Fatalf("the merge commit was authored by %q, want the reviewer at the terminal", got)
	}
	if got := queryString(t, st, "SELECT COUNT(*) FROM dolt_branches WHERE name LIKE 'proposal/%'"); got != "0" {
		t.Fatalf("%s proposal branch(es) survived the accept", got)
	}
}

// TestReviewCLIRejectAndExpire covers the two verbs that remove a proposal
// without promoting it, and the audit that reports without touching
// anything.
func TestReviewCLIRejectAndExpire(t *testing.T) {
	base := initStore(t)
	rejected := stageFact(t, base, "not this one", localdolt.Fact{Key: "build.command", Value: "go build ./..."})
	swept := stageFact(t, base, "nor this one", localdolt.Fact{Key: "convention.style", Value: "gofmt -l ."})

	out := runMemdolt(t, "review", "reject", rejected.ID, "--dir", base)
	if !strings.Contains(out, rejected.ID) || !strings.Contains(out, "main is unchanged") {
		t.Fatalf("`review reject` printed %q", out)
	}

	// The stale audit reports and changes nothing: --older-than 0 makes
	// every pending proposal old enough to report.
	stale := decodeJSON[proposalList](t, runMemdolt(t,
		"review", "stale", "--older-than", "0s", "--dir", base, "--json"))
	if len(stale.Proposals) != 1 || stale.Proposals[0].ID != swept.ID {
		t.Fatalf("`review stale` returned %+v, want the one remaining proposal", stale.Proposals)
	}
	after := decodeJSON[proposalList](t, runMemdolt(t, "review", "list", "--dir", base, "--json"))
	if len(after.Proposals) != 1 {
		t.Fatalf("the stale audit changed the pending set to %+v", after.Proposals)
	}

	// The default expiry age leaves a proposal staged a moment ago alone.
	untouched := decodeJSON[proposalList](t, runMemdolt(t, "review", "expire", "--dir", base, "--json"))
	if len(untouched.Proposals) != 0 {
		t.Fatalf("the default expiry swept %+v", untouched.Proposals)
	}

	expired := decodeJSON[proposalList](t, runMemdolt(t,
		"review", "expire", "--older-than", "0s", "--dir", base, "--json"))
	if len(expired.Proposals) != 1 || expired.Proposals[0].ID != swept.ID {
		t.Fatalf("`review expire --older-than 0s` swept %+v", expired.Proposals)
	}
	if empty := decodeJSON[proposalList](t, runMemdolt(t, "review", "list", "--dir", base, "--json")); len(empty.Proposals) != 0 {
		t.Fatalf("%+v are still pending after expiry", empty.Proposals)
	}

	// Neither verb promoted anything.
	st := openInitializedStore(t, base)
	defer func() { _ = st.Close() }()
	if got := queryString(t, st, "SELECT COUNT(*) FROM facts"); got != "0" {
		t.Fatalf("main carries %s facts after a reject and an expiry, want 0", got)
	}
}

func TestReviewAgeHelpUsesGoDurations(t *testing.T) {
	unsupportedDayDuration := regexp.MustCompile(`\b\d+d\b`)
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{
			name: "expire",
			cmd:  newReviewExpireCommand(),
		},
		{
			name: "stale",
			cmd:  newReviewStaleCommand(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			help := tc.cmd.UsageString()
			flag := tc.cmd.Flags().Lookup("older-than")
			if flag == nil {
				t.Fatal("--older-than flag is missing")
			}
			defaultAge, err := time.ParseDuration(flag.DefValue)
			if err != nil {
				t.Fatalf("--older-than default %q is not a Go duration: %v", flag.DefValue, err)
			}
			want := fmt.Sprintf("Go duration (for example %dh); default: %s (%d days)",
				int(defaultAge.Hours()), flag.DefValue, int(defaultAge.Hours()/24))
			if !strings.Contains(help, want) {
				t.Fatalf("help %q does not contain %q", help, want)
			}
			if unsupported := unsupportedDayDuration.FindString(help); unsupported != "" {
				t.Fatalf("help %q suggests unsupported day duration syntax %q", help, unsupported)
			}
		})
	}
}

// TestReviewCLIRefusalsAreLoud covers what the review verbs owe an operator
// who names something that is not there or not reviewable here.
func TestReviewCLIRefusalsAreLoud(t *testing.T) {
	base := initStore(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "an id that is not a ULID",
			args: []string{"review", "accept", "nope", "--dir", base},
			want: "is not a proposal id",
		},
		{
			name: "a proposal that is not pending",
			args: []string{"review", "show", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--dir", base},
			want: "there is no pending proposal",
		},
		{
			name: "rejecting a proposal that is not pending",
			args: []string{"review", "reject", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--dir", base},
			want: "there is no pending proposal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runMemdoltErr(t, tc.args...); !strings.Contains(err, tc.want) {
				t.Fatalf("error was %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// An empty listing is not an error: nothing to review is the ordinary
	// state of a reviewed store.
	if out := runMemdolt(t, "review", "list", "--dir", base); !strings.Contains(out, "no proposals") {
		t.Fatalf("`review list` on an empty store printed %q", out)
	}
}

// stageFact stages one fact proposal through the store, the way the MCP
// server's propose_fact will. The store is closed before it returns,
// because it holds the single-owner lock (PRD §5.2) that the CLI
// invocations under test need to take.
func stageFact(t *testing.T, base, rationale string, fact localdolt.Fact) localdolt.StagedProposal {
	t.Helper()
	st, err := localdolt.New(localdolt.Config{BaseDir: base, Actor: cliStagingActor})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.Open(context.Background()); err != nil {
		t.Fatalf("open the store to stage a proposal: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close the staging store: %v", err)
		}
	}()

	staged, err := st.ProposeFact(context.Background(), localdolt.Proposal{
		Rationale: rationale,
		Actor:     cliStagingActor,
		Target:    localdolt.TargetRepo,
	}, fact)
	if err != nil {
		t.Fatalf("stage a proposal: %v", err)
	}
	return staged
}

// queryString reads one value out of the store as text.
func queryString(t *testing.T, st *localdolt.Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("%s: no rows (err %v)", query, rows.Err())
	}
	var value string
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("%s: scan: %v", query, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}
