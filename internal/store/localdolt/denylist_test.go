package localdolt_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// theSecret stands in for whatever a deny-list exists to keep out of the
// store's history. Nothing in these tests lets it reach an error message.
const theSecret = "AKIAIOSFODNN7EXAMPLE"

// factWrite is the shape of a memory write the M1 lanes will produce: the
// statements that record a fact, and the same text declared for the
// deny-list to be matched against.
func factWrite(id, key, value string) store.CommitRequest {
	return store.CommitRequest{
		Statements: []store.Statement{{
			SQL:  "INSERT INTO facts (id, `key`, value, created_at) VALUES (?, ?, ?, NOW())",
			Args: []any{id, key, value},
		}},
		Text:    []string{key, value},
		Message: "propose fact " + key,
		Author:  store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"},
	}
}

// TestCommitRefusesTextMatchingTheDenyListAndLeavesNothingBehind covers
// PRD §11.3's `[deny_list]` on the write path: a write whose text matches
// a configured rule is refused with an error naming the rule, and the
// refusal is complete — no row, and nothing added to the Dolt history.
func TestCommitRefusesTextMatchingTheDenyListAndLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := migratedStoreAt(t, base)
	before := commitCount(t, st)

	writeDenyList(t, base, `AKIA[0-9A-Z]{16}`)

	_, err := st.Commit(ctx, factWrite("01J00000000000000000000001", "aws.access_key", theSecret))
	if !errors.Is(err, store.ErrDenied) {
		t.Fatalf("commit error = %v, want one matching store.ErrDenied", err)
	}
	if !strings.Contains(err.Error(), `AKIA[0-9A-Z]{16}`) {
		t.Fatalf("refusal %q does not name the rule that matched", err)
	}
	if strings.Contains(err.Error(), theSecret) {
		t.Fatalf("refusal %q repeats the matched text back to the caller", err)
	}

	// Refused before the transaction opened: nothing to roll back, and so
	// nothing left in the working set for the next commit to sweep up.
	if got := factCount(t, st); got != 0 {
		t.Fatalf("facts after a refused write = %d, want 0", got)
	}
	if got := commitCount(t, st); got != before {
		t.Fatalf("history after a refused write has %d commits, want %d", got, before)
	}

	// The rule bounds the write, not the store: text it does not match
	// still commits, and the store is not left in a state that prevents
	// it.
	if _, err := st.Commit(ctx, factWrite("01J00000000000000000000002", "build.tool", "go 1.26")); err != nil {
		t.Fatalf("commit of text the deny-list does not match: %v", err)
	}
	if got := commitCount(t, st); got != before+1 {
		t.Fatalf("history after an allowed write has %d commits, want %d", got, before+1)
	}
}

// TestStagedWritesAreScannedByTheDenyList runs the deny-list through the
// staged-write lane of PRD §6.2, the one an untrusted agent writes with.
//
// Its commits do not go through Store.Commit — they are made on a proposal
// branch, on a connection the lane holds — so a deny-list wired only into
// Commit would leave exactly these writes unscanned. Each case here puts
// the secret in a different agent-supplied column to check that the lane
// declares all of them and not just the one in the commit message.
func TestStagedWritesAreScannedByTheDenyList(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := migratedStoreAt(t, base)
	before := commitCount(t, st)

	writeDenyList(t, base, `AKIA[0-9A-Z]{16}`)

	good := localdolt.Proposal{Rationale: "why", Actor: stagingActor, Target: localdolt.TargetRepo}
	for name, stage := range map[string]func() (localdolt.StagedProposal, error){
		"a fact's value": func() (localdolt.StagedProposal, error) {
			return st.ProposeFact(ctx, good, localdolt.Fact{Key: "aws.access_key", Value: theSecret})
		},
		"a decision's rationale": func() (localdolt.StagedProposal, error) {
			return st.ProposeDecision(ctx, good, localdolt.Decision{
				Title:     "Rotate the deploy credentials",
				Rationale: "the old one was " + theSecret,
			})
		},
		"the note to the reviewer": func() (localdolt.StagedProposal, error) {
			return st.ProposeFact(ctx,
				localdolt.Proposal{Rationale: "found in the log: " + theSecret, Actor: stagingActor, Target: localdolt.TargetRepo},
				localdolt.Fact{Key: "build.tool", Value: "go 1.26"})
		},
	} {
		_, err := stage()
		if !errors.Is(err, store.ErrDenied) {
			t.Errorf("%s: staging error = %v, want one matching store.ErrDenied", name, err)
		}
		if err != nil && strings.Contains(err.Error(), theSecret) {
			t.Errorf("%s: refusal %q repeats the matched text back to the caller", name, err)
		}
	}

	// A refused proposal leaves no branch, no row and no commit: staging
	// deletes the branch it could not fill, the way it does for any other
	// refusal.
	requireProposalBranches(t, st)
	if got := factCount(t, st); got != 0 {
		t.Fatalf("facts after refused proposals = %d, want 0", got)
	}
	if got := commitCount(t, st); got != before {
		t.Fatalf("history after refused proposals has %d commits, want %d", got, before)
	}

	// Text the rule does not match still stages, on its own branch.
	staged, err := st.ProposeFact(ctx, good, localdolt.Fact{Key: "build.tool", Value: "go 1.26"})
	if err != nil {
		t.Fatalf("propose a fact the deny-list does not match: %v", err)
	}
	requireProposalBranches(t, st, staged.Branch)
}

// TestCommitRefusesADenyListItCannotEvaluate covers the fail-closed half
// of PRD §14: a configured deny-list memdolt cannot evaluate refuses the
// write rather than letting it through unscanned.
func TestCommitRefusesADenyListItCannotEvaluate(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := migratedStoreAt(t, base)
	before := commitCount(t, st)

	// A pattern that is not a valid regular expression: configured, and
	// impossible to decide with.
	writeDenyList(t, base, `a(b`)

	_, err := st.Commit(ctx, factWrite("01J00000000000000000000003", "build.tool", "go 1.26"))
	if err == nil {
		t.Fatal("commit succeeded against a deny-list that cannot be evaluated")
	}
	if !strings.Contains(err.Error(), "deny_list.patterns[0]") {
		t.Fatalf("refusal %q does not say which pattern could not be evaluated", err)
	}
	if got := commitCount(t, st); got != before {
		t.Fatalf("history after a refused write has %d commits, want %d", got, before)
	}
	if got := factCount(t, st); got != 0 {
		t.Fatalf("facts after a refused write = %d, want 0", got)
	}
}

// TestCommitIsUnaffectedWithoutADenyList covers the unconfigured case: no
// config file at all is not a deny-list that failed to load.
func TestCommitIsUnaffectedWithoutADenyList(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	st := migratedStoreAt(t, base)
	before := commitCount(t, st)

	if _, err := st.Commit(ctx, factWrite("01J00000000000000000000004", "aws.access_key", theSecret)); err != nil {
		t.Fatalf("commit with no deny-list configured: %v", err)
	}
	if got := commitCount(t, st); got != before+1 {
		t.Fatalf("history has %d commits, want %d", got, before+1)
	}
}

// TestMigrationsRunAgainstADenyListThatCannotBeEvaluated covers the
// boundary the check draws: it applies to writes that declare memory
// text, and the migration runner's commits declare none, so a broken
// deny-list cannot leave a repository unable to initialize.
func TestMigrationsRunAgainstADenyListThatCannotBeEvaluated(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	writeDenyList(t, base, `a(b`)

	st := openStore(t, base, discardLogger())
	result, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate against a broken deny-list: %v", err)
	}
	if result.Version != store.LatestSchemaVersion() {
		t.Fatalf("schema version after migrate = %d, want %d", result.Version, store.LatestSchemaVersion())
	}
}

// writeDenyList configures one deny-list rule for the repository at base.
func writeDenyList(t *testing.T, base string, patterns ...string) {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if err := os.MkdirAll(paths.Dir(), 0o755); err != nil {
		t.Fatalf("create %s: %v", paths.Dir(), err)
	}

	// Patterns are TOML literal strings: a regular expression's
	// backslashes are not valid escapes in a basic string.
	body := "[deny_list]\npatterns = [\n"
	for _, pattern := range patterns {
		if strings.Contains(pattern, "'") {
			t.Fatalf("test pattern %q cannot be written as a TOML literal string", pattern)
		}
		body += "  '" + pattern + "',\n"
	}
	body += "]\n"

	if err := os.WriteFile(paths.ConfigFile(), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", paths.ConfigFile(), err)
	}
}

// migratedStoreAt opens a store at base with PRD §6.1's schema applied,
// which is what a write path needs beneath it. These tests name the base
// directory because the deny-list they configure lives in it.
func migratedStoreAt(t *testing.T, base string) *localdolt.Store {
	t.Helper()
	st := openStore(t, base, discardLogger())
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func commitCount(t *testing.T, st *localdolt.Store) int {
	t.Helper()
	return scanCount(t, st, "SELECT COUNT(*) FROM dolt_log")
}

func factCount(t *testing.T, st *localdolt.Store) int {
	t.Helper()
	return scanCount(t, st, "SELECT COUNT(*) FROM facts")
}

func scanCount(t *testing.T, st *localdolt.Store, query string) int {
	t.Helper()
	rows, err := st.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("%s: no rows (err %v)", query, rows.Err())
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("%s: scan: %v", query, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}
