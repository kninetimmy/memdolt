//go:build soak

package soak

import (
	"fmt"
	"testing"
	"time"
)

// TestReconciliationDetectsWhatItClaimsTo is the rig's negative control.
//
// Every other number this package reports rests on reconcile, and a
// reconciler with a bug reports zero data-loss events on a store that lost
// everything. So this feeds it a ledger and a store that disagree in every
// way the rig claims to detect, and requires it to say so — and requires
// the verdict to come out FAIL. Without this, "0 lost" would mean only
// that nothing was looked for.
func TestReconciliationDetectsWhatItClaimsTo(t *testing.T) {
	const writer = "probe-w0"
	now := time.Now()

	payload := func(seq int) string { return fmt.Sprintf("nonce|%s|%d", writer, seq) }
	key := func(seq int) string { return fmt.Sprintf("%s/%06d", writer, seq) }

	var records []Record
	add := func(seq int, phase Phase, outcome Outcome) {
		records = append(records, Record{
			Phase:    phase,
			Writer:   writer,
			Seq:      seq,
			Key:      key(seq),
			Payload:  payload(seq),
			Digest:   digestOf(payload(seq)),
			Outcome:  outcome,
			AtUnixNS: now.UnixNano(),
		})
	}
	intentAndResult := func(seq int, outcome Outcome) {
		add(seq, PhaseIntent, "")
		add(seq, PhaseResult, outcome)
	}

	intentAndResult(0, OutcomeCommitted)     // present and intact
	intentAndResult(1, OutcomeCommitted)     // absent      -> lost
	intentAndResult(2, OutcomeCommitted)     // wrong value -> corrupted
	intentAndResult(3, OutcomeRefused)       // present     -> phantom
	intentAndResult(4, OutcomeRefused)       // absent      -> fail-closed, fine
	intentAndResult(5, OutcomeIndeterminate) // present
	intentAndResult(6, OutcomeIndeterminate) // absent
	add(7, PhaseIntent, "")                  // killed mid-write, present
	add(8, PhaseIntent, "")                  // killed mid-write, absent

	stored := map[string]storedRow{
		key(0):          {payload: payload(0), digest: digestOf(payload(0))},
		key(2):          {payload: "tampered", digest: digestOf(payload(2))},
		key(3):          {payload: payload(3), digest: digestOf(payload(3))},
		key(5):          {payload: payload(5), digest: digestOf(payload(5))},
		key(7):          {payload: payload(7), digest: digestOf(payload(7))},
		"nobody/000000": {payload: "a row no writer ever intended", digest: digestOf("x")},
	}

	summary := newSummary("negative-control", ScenarioConfig{})
	reconcile(map[string]*WriterLedger{writer: {Writer: writer, Records: records}}, stored, summary, time.Time{})
	summary.Reads.MissingCommittedRow = 1
	summary.decide()

	got := summary.Writes
	for _, want := range []struct {
		name string
		got  int
		want int
	}{
		{"attempted", got.Attempted, 9},
		{"committed", got.Committed, 3},
		{"refused", got.Refused, 2},
		{"indeterminate", got.Indeterminate, 2},
		{"noResult", got.NoResult, 2},
		{"lost", got.Lost, 1},
		{"corrupted", got.Corrupted, 1},
		{"phantom", got.Phantom, 1},
		{"indeterminatePresent", got.IndeterminatePresent, 1},
		{"indeterminateAbsent", got.IndeterminateAbsent, 1},
		{"noResultPresent", got.NoResultPresent, 1},
		{"noResultAbsent", got.NoResultAbsent, 1},
		{"unledgered", got.Unledgered, 1},
	} {
		if want.got != want.want {
			t.Errorf("%s = %d, want %d (full accounting: %+v)", want.name, want.got, want.want, got)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	if summary.Verdict != VerdictFail {
		t.Fatalf("verdict = %s, want %s: the input contains a lost write, a corrupted write, an unledgered row and an unseen commit",
			summary.Verdict, VerdictFail)
	}
	// One event per lost write, corrupted write, unseen read and unledgered
	// row; phantom and indeterminate writes are findings, not events.
	if summary.DataLossEvents != 4 {
		t.Fatalf("data-loss events = %d, want 4 (%v)", summary.DataLossEvents, summary.VerdictReasons)
	}
}

// TestReconciliationPassesAStoreThatKeptEverything is the other half of the
// control: the same shape of input, with the store holding exactly what the
// ledger says it was told to hold, must come out PASS. A reconciler that
// fails everything is as useless as one that passes everything.
func TestReconciliationPassesAStoreThatKeptEverything(t *testing.T) {
	const writer = "probe-w0"
	payload := func(seq int) string { return fmt.Sprintf("nonce|%s|%d", writer, seq) }
	key := func(seq int) string { return fmt.Sprintf("%s/%06d", writer, seq) }

	var records []Record
	stored := map[string]storedRow{}
	for seq := 0; seq < 5; seq++ {
		for _, phase := range []Phase{PhaseIntent, PhaseResult} {
			outcome := Outcome("")
			if phase == PhaseResult {
				outcome = OutcomeCommitted
			}
			records = append(records, Record{
				Phase: phase, Writer: writer, Seq: seq, Key: key(seq),
				Payload: payload(seq), Digest: digestOf(payload(seq)), Outcome: outcome,
			})
		}
		stored[key(seq)] = storedRow{payload: payload(seq), digest: digestOf(payload(seq))}
	}

	summary := newSummary("negative-control-clean", ScenarioConfig{})
	reconcile(map[string]*WriterLedger{writer: {Writer: writer, Records: records}}, stored, summary, time.Time{})
	summary.decide()

	if summary.Writes.Attempted != 5 || summary.Writes.Committed != 5 || summary.Writes.Lost != 0 {
		t.Fatalf("accounting = %+v, want 5 attempted, 5 committed, none lost", summary.Writes)
	}
	if summary.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want %s (%v)", summary.Verdict, VerdictPass, summary.VerdictReasons)
	}
}
