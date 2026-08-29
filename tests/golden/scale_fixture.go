//go:build golden

package golden

import (
	"fmt"
	"strings"
	"testing"
)

// scaleCorpusSize is how many synthetic hard-negative rows scaleCorpus
// derives from the hermetic fixture — an order of magnitude past
// rerankPoolSize=20 and at the low end of PRD §17 R2's own "memory scale"
// (~10³ rows). With the fixture's 21 rows the scale store carries 1021.
const scaleCorpusSize = 1000

var (
	scaleSourceTypes   = [...]string{"fact", "decision", "task", "doc_chunk"}
	scaleTitlePrefixes = [...]string{"note", "memo", "record", "summary", "draft", "log entry", "follow-up", "archive item"}
	scaleBodyFillers   = [...]string{
		"kept for provenance only",
		"see the earlier thread for surrounding discussion",
		"context since moved to the project archive",
		"older entry retained so history stays searchable",
		"recorded as-is without further follow-up",
		"cross-linked from a previous planning session",
	}
)

// scaleDocTitle names the document every synthetic doc_chunk hangs off
// (insertRows attaches them to the first documents row) and leads both the
// chunk's breadcrumb and its hydrated title, mirroring recall.rs's
// load_source_row "{doc title} — {heading_path}" shape the fixture's own
// chunks carry.
const scaleDocTitle = "Synthetic Corpus"

// scaleCorpus derives scaleCorpusSize deterministic hard-negative rows from
// the hermetic fixture's own target rows (fixture.go's seedRows): each
// negative recombines a sliding wrap-around window over two targets'
// words — primary target i%len(targets), secondary at a fixed stride — so
// negatives share the golden queries' vocabulary (which is what makes them
// hard for the lexical step) while their scrambled phrasing keeps them
// semantically empty.
//
// Determinism contract: generated content comes only from these template
// constants and integer arithmetic cycling over the fixture slice — no RNG,
// no clock, no filesystem, and no map-iteration order anywhere in
// generation (the seen map is membership-only). The same binary produces
// byte-identical rows on every machine and run.
//
// Two mechanical screens keep the measurement honest:
//   - uniqueness: duplicate text would embed identically and hand exact
//     score ties to the fusion sort's tiebreak; a colliding candidate
//     rotates its window instead.
//   - matcher safety: a negative satisfying any match query's
//     title_contains/body_contains could steal that query's top-3 slot and
//     count as a recall pass with the real target absent; candidates are
//     checked against every match-kind matcher (eval.go's hitMatches) and
//     rotated until clear.
//
// Negatives are ordinary corpusRow values, so they get exactly the
// fixture's persist-side treatment (embedText/rerankText, insertRows'
// §6.1-shaped inserts). Generated facts reuse title as their key; that key
// is distinct in the current deterministic corpus, and a future collision
// fails loudly during insertion. The uniqueness screen above checks
// title/body pairs, not fact keys. Generated doc_chunks continue the shared
// document's ord sequence past the fixture's 0–2.
func scaleCorpus(t *testing.T, golden *goldenFile) []corpusRow {
	t.Helper()

	targets := seedRows()
	matchQueries := make([]goldenQuery, 0, len(golden.Queries))
	for _, q := range golden.Queries {
		if q.Kind == "match" {
			matchQueries = append(matchQueries, q)
		}
	}
	if len(matchQueries) == 0 {
		t.Fatalf("golden file has no match queries; nothing to derive negatives from")
	}

	rows := make([]corpusRow, 0, scaleCorpusSize)
	seen := make(map[string]bool, scaleCorpusSize)
	nextOrd := 3 // the fixture's three chunks hold ord 0–2 on the shared document
	for i := 0; len(rows) < scaleCorpusSize; i++ {
		primary := targets[i%len(targets)]
		secondary := targets[(i*13+5)%len(targets)]
		pool := bm25Tokenize(primary.title + " " + primary.body + " " +
			secondary.title + " " + secondary.body)

		sourceType := scaleSourceTypes[i%len(scaleSourceTypes)]
		prefix := scaleTitlePrefixes[i%len(scaleTitlePrefixes)]
		filler := scaleBodyFillers[i%len(scaleBodyFillers)]
		width := 10 + (i%3)*4 // window lengths cycle 10 / 14 / 18

		var cand corpusRow
		placed := false
		for shift := 0; shift < 16 && !placed; shift++ {
			off := (i*17 + shift) % len(pool)
			window := make([]string, width)
			for k := range window {
				window[k] = pool[(off+k)%len(pool)]
			}
			half := width / 2
			cand = corpusRow{
				sourceType: sourceType,
				id:         fmt.Sprintf("syn-%s-%04d", sourceType, len(rows)),
				scope:      "repo",
				title:      prefix + ": " + strings.Join(window[:half], " "),
				body:       strings.Join(window[half:], " ") + ". " + filler + ".",
			}
			if sourceType == "doc_chunk" {
				cand.headingPath = scaleDocTitle + " > " + cand.title
				cand.title = scaleDocTitle + " — " + cand.headingPath
				cand.ord = nextOrd
			}
			text := cand.title + "\x00" + cand.body
			if seen[text] || scaleMatchesAny(matchQueries, cand) {
				continue
			}
			seen[text] = true
			placed = true
		}
		if !placed {
			t.Fatalf("scaleCorpus: exhausted window rotations for row %d without a unique, matcher-safe candidate", i)
		}
		if cand.sourceType == "doc_chunk" {
			nextOrd++
		}
		rows = append(rows, cand)
	}
	return rows
}

// scaleMatchesAny reports whether r would satisfy any match-kind golden
// query's matcher — the one condition a negative row must never meet.
func scaleMatchesAny(queries []goldenQuery, r corpusRow) bool {
	hit := resultHit{sourceType: r.sourceType, id: r.id, title: r.title, body: r.body}
	for _, q := range queries {
		if hitMatches(q, hit) {
			return true
		}
	}
	return false
}
