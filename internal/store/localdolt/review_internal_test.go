package localdolt

import (
	"strings"
	"testing"
)

// TestClassifyViolationsRefusesWhatItCannotAccountFor covers the half of
// PRD §6.3's "refuses unknown or unattributed rows" that a live merge
// cannot be made to produce on demand: Dolt only reports a violation
// against a row that is really there, so a record naming a row neither side
// touched, or a violation class this lane cannot verify, is exercised
// against the decision itself rather than against a Dolt state faked to
// contain one.
func TestClassifyViolationsRefusesWhatItCannotAccountFor(t *testing.T) {
	const table = "facts"
	touched := map[string]struct{}{"01FACT": {}, "01REPL": {}}

	t.Run("clears the rows the merge touched", func(t *testing.T) {
		constraint, ids, err := classifyViolations(table, []violationRecord{
			{id: "01REPL", kind: clearableViolation, constraint: "uk_fact_live_key"},
			{id: "01FACT", kind: clearableViolation, constraint: "uk_fact_live_key"},
		}, touched)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if constraint != "uk_fact_live_key" {
			t.Fatalf("constraint = %q", constraint)
		}
		if strings.Join(ids, ",") != "01FACT,01REPL" {
			t.Fatalf("ids = %v, want both rows in a stable order", ids)
		}
	})

	for _, tc := range []struct {
		name    string
		records []violationRecord
		want    string
	}{
		{
			name: "a row neither side of the merge changed",
			records: []violationRecord{
				{id: "01FACT", kind: clearableViolation, constraint: "uk_fact_live_key"},
				{id: "01BYSTANDER", kind: clearableViolation, constraint: "uk_fact_live_key"},
			},
			want: "cannot attribute it",
		},
		{
			name: "a violation class PRD §6.3 does not clear",
			records: []violationRecord{
				{id: "01FACT", kind: "foreign key", constraint: "fk_fact_source"},
			},
			want: `"foreign key"`,
		},
		{
			name: "two constraints at once",
			records: []violationRecord{
				{id: "01FACT", kind: clearableViolation, constraint: "uk_fact_live_key"},
				{id: "01REPL", kind: clearableViolation, constraint: "uk_something_else"},
			},
			want: "two constraints",
		},
		{
			name:    "a summary with no rows behind it",
			records: nil,
			want:    "holds no rows describing them",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ids, err := classifyViolations(table, tc.records, touched)
			if err == nil {
				t.Fatalf("classify accepted %s and offered to clear %v", tc.name, ids)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestProposalBranchOfRefusesAnythingButAULID covers the boundary that lets
// a proposal branch be formatted into an AS OF clause at all: Dolt parses a
// revision as a literal and will not take it as a bound parameter, so an id
// that is not 26 characters of Crockford base32 must never get that far.
func TestProposalBranchOfRefusesAnythingButAULID(t *testing.T) {
	if branch, err := proposalBranchOf("01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != nil {
		t.Fatalf("a ULID was refused: %v", err)
	} else if branch != "proposal/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("branch = %q", branch)
	}

	for _, id := range []string{
		"",
		"01ARZ3NDEKTSV4RRFFQ69G5FA",   // one short
		"01ARZ3NDEKTSV4RRFFQ69G5FAVX", // one long
		"01ARZ3NDEKTSV4RRFFQ69G5FAU",  // U is not in the alphabet
		"01ARZ3NDEKTSV4RRFFQ69G5fav",  // lower case is not either
		"'; DROP TABLE facts; ------",
		"01ARZ3NDEKTSV4RRFF'69G5FAV",
	} {
		if branch, err := proposalBranchOf(id); err == nil {
			t.Fatalf("%q was accepted as the proposal id of branch %q", id, branch)
		}
	}
}
