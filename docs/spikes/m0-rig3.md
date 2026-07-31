# M0 rig 3 — retrieval golden gate

PRD §16's third M0 rig: *"Retrieval rig: golden set on Dolt FULLTEXT +
brute-force cosine."* Its exit gate (§16, restated in §17's R2 row) is
**"Recall@K ≥ memhub baseline (or the BM25 contingency proves it)."** This
is also the MD5 decision input: *"Dolt FULLTEXT first; in-process BM25
contingency on golden-gate failure — decided by M0 measurement"* (§19).

## 1. Verdict

**PASS — Dolt FULLTEXT, no contingency needed.**

| Gate condition (PRD §16 / §17 R2) | Measured |
|---|---|
| Recall@3 ≥ memhub baseline (100%, 21/21) | **100%, 21/21** (Dolt FULLTEXT) |
| Zero safety failures | **0** (Dolt FULLTEXT) |
| Failing queries | **none**, in either configuration |

The §8.1 R2 in-process BM25 contingency was implemented and measured
alongside FULLTEXT anyway (not conditionally — "under the same harness"
is easiest to prove by literally running both every time), and it also
clears the gate:

| Lexical gather (§8.1 step 1) | Recall@3 | Match passes | Safety failures |
|---|---|---|---|
| Dolt FULLTEXT (`MATCH … AGAINST … IN NATURAL LANGUAGE MODE`) | 100% | 21/21 | 0 |
| In-process BM25 (§8.1 R2 contingency) | 100% | 21/21 | 0 |

Both configurations are byte-for-byte the same pipeline (vector gather,
fusion, rerank, floors, truncation — `tests/golden/pipeline.go`) with only
the lexical-gather step swapped, so a difference between them would
isolate cleanly to that step. There wasn't one, on this golden set.

### What this PASS does and does not license

It says: PRD §8.1's pipeline, run against a disposable embedded Dolt store
carrying §6.1-shaped FULLTEXT tables and embeddings built through the
rig-2 inference path (`tests/inference`, shared with `tests/parity`),
reproduces memhub's own hermetic Recall@3 baseline on the identical
22-query golden set and an identical fixture corpus. Every query kind the
golden set exercises passed: keyword-style queries against Dolt FULLTEXT's
natural-language relevancy, `semantic-`-prefixed paraphrase queries that
lean on the vector+rerank path (near-zero keyword overlap by design), the
`doc-`-prefixed doc-chunk path (gated by `doc_min_rerank_score`), the
`global-`-prefixed cross-scope path (seeded as scope-tagged rows in the
same store per this issue's license — see §3), and the one `kind: empty`
safety probe.

It does not say Dolt FULLTEXT's natural-language relevancy formula is
numerically equivalent to memhub's SQLite `bm25()` — it isn't (§4) — only
that the two produce ranking behavior close enough, after min-max
normalization and a 50/50 blend with cosine, that neither pipeline drops a
target row out of the reranked top 3 on this 22-query, ~19-row corpus. It
also does not cover a larger or more adversarial corpus: everything here
runs at the same "memory scale" (10³–10⁴ rows) the PRD's risk register
already scopes this to (§17 R2's mitigation note), not at web-search scale
where NL-mode's word-frequency behavior might separate further from BM25.

## 2. What the numbers were measured on

| | |
|---|---|
| Date | 2026-07-30 |
| Worktree base | `8b6f6fa` (PR #15, M0 rig 2) on `orch/issue-14-build-the-m0-rig-3-retrieval-golden-gate` |
| memhub commit measured against | `github.com/kninetimmy/memhub` v0.2.0, local checkout at `C:\Users\Kninetimmy\memhub` |
| memhub baseline | `cargo test --test retrieval_harness retrieval_golden_hermetic` → 3/3 pass, `recall_at_k == 1.0` (21/21), 0 safety failures, on the same shipped `tests/retrieval_golden.json` (22 queries) — measured by the Architect 2026-07-30, re-confirmed read-only during this rig |
| OS | Windows 11 Pro, build 10.0.26200, amd64 |
| Go | go1.26.5 windows/amd64 |
| C toolchain | MinGW-W64 x86_64-ucrt-posix-seh (winlibs r3) GCC 16.1.0 |
| Go build settings | `CGO_ENABLED=1`, `-tags golden,gms_pure_go` |
| `github.com/dolthub/driver` | v1.88.1 |
| `github.com/dolthub/dolt/go` | v0.40.5-0.20260507221239-14b38e279fc6 |
| `github.com/dolthub/go-mysql-server` | v0.20.1-0.20260507202550-43d6daf5958b |
| Embedding / rerank models, tokenizer | same as rig 2 (docs/spikes/m0-rig2.md §2) — reused unmodified via `tests/inference` |
| Full pipeline run time | ~5.2s (schema + seed + embed 19 rows + 44 query runs [22 FULLTEXT + 22 BM25], each query reranking a pool of up to 20 candidates) |

**One platform only**, same caveat as rigs 1 and 2: Windows/amd64.

## 3. The fixture corpus and how it was ported

`tests/golden/retrieval_golden.json` is a byte-identical copy of memhub's
shipped `tests/retrieval_golden.json` (verified with `diff` at port time,
22 queries: 21 `match` + 1 `empty`) — not re-derived, not re-worded.

The seeded corpus (`tests/golden/fixture.go`'s `seedRows`) ports every row
memhub's `tests/retrieval/retrieval_golden_hermetic.rs` (`seed_hermetic_corpus`,
`seed_global_store`) creates to satisfy those 22 queries: 2 facts, 13
decisions (4 of them carrying the real backfilled `summary` text the
`semantic-decision-*` queries need to clear the rerank floor — without
those, per that file's own comments, the cross-encoder logit for the
paraphrase falls below `min_rerank_score` and the query returns zero
hits), 1 task, 1 doc chunk (ingested the same "Rust Code Style Guide /
Error Handling" content memhub's fixture uses, already empirically
calibrated against the doc floor per that file's comment), and 2
global-scope rows.

**One licensed deviation from a literal §6.1 store, per this issue's
acceptance criteria:** the two global-scope rows
(`global-fact-git-signing-key`, `semantic-global-decision-editor-indent`)
are seeded as ordinary rows in the same disposable store, tagged with a
`scope` column (`schema.go`'s `facts`/`decisions` DDL adds this one column
beyond §6.1) rather than standing up a second Dolt store and memdolt's
actual repo+global merge plumbing (PRD §8.1 step 6, not yet built — M2/M9
territory). `pipeline.go`'s fusion runs over the whole tagged pool at once
rather than per-scope-then-merge; the practical difference is that
`normalize_fts`'s min-max normalization is computed across both scopes'
FTS hits together instead of independently per scope before a merge. On a
19-row corpus where each golden query targets one specific row, this
rarely changes anything — most queries' FTS hit set is small enough that
`normalize_fts`'s degenerate-range branch (single hit ⇒ 1.0) applies
either way — and it didn't change the measured pass/fail outcome for
either `global-*` query here. **This rig gates the retrieval math, not
global-store plumbing** (the acceptance criteria's own words); a literal
two-store merge is real work for a later milestone, not a numerical
question this rig exists to answer.

## 4. The pipeline, as built

`tests/golden/pipeline.go` implements PRD §8.1 exactly as specified,
reusing the rig-2 inference path (`tests/inference`, factored out of
`tests/parity` so both rigs share one embedding/rerank/tokenizer
implementation instead of two — see that package's doc comment) for every
BGE-small-en-v1.5 embed call and every ms-marco-MiniLM-L-6-v2 rerank call:

1. **Lexical gather** (`fulltext.go` / `bm25.go`) — per source type
   (`fact`, `decision`, `task`, `doc_chunk`), `LIMIT 50`.
   - **FULLTEXT:** `SELECT id, MATCH(cols) AGAINST (? IN NATURAL LANGUAGE
     MODE) AS score FROM t WHERE MATCH(cols) AGAINST (? IN NATURAL
     LANGUAGE MODE) ORDER BY score DESC LIMIT 50` against the §6.1
     FULLTEXT-keyed columns (`facts(key, value)`, `decisions(title,
     rationale)`, `tasks(title, notes)`, `doc_chunks(heading_path,
     body)`). Dolt's NL-mode relevancy (`sql/expression/matchagainst.go`
     in `go-mysql-server`) is higher-is-better and is **not** the same
     formula as SQLite's `bm25()` memhub calls (which is lower-is-better
     and gets negated in `recall.rs`'s `score()`) — it is `log(doc_count)
     + 1` scaled by a row-length normalization factor and a global-count
     multiplier, closer to the classic MySQL/MyISAM full-text relevancy
     than to Okapi BM25. `normalize_fts` (below) makes the sign and scale
     irrelevant to the fusion formula either way; only the *ranking* the
     raw score induces matters, and Dolt's induced the same top-3 winners
     as memhub's on this corpus.
   - **BM25 (contingency):** in-process Okapi BM25 (`k1=1.2, b=0.75`)
     over an inverted index built once per source type from the same
     in-memory corpus, no Dolt query involved at all.
2. **Vector gather** — brute-force cosine (`util.rs`'s exact formula,
   reimplemented in Go) between the query's BGE embedding and every seeded
   row's embedding (built once at seed time via `inference.EmbedRunner`),
   restricted to the requested source types, `cosine < 0.0` excluded.
3. **Stale-embedding detection** — not implemented: the fixture seeds no
   stale or corrupt rows (§8.1 step 3 is a no-op on data that's never
   stale by construction, same as memhub's own hermetic fixture).
4. **Hydrate + filter** — trivial here: every seeded row is fully
   hydrated in memory at seed time (`corpusRow`); no fact in the fixture
   is stale (`fact_stale_after_days` horizon irrelevant to freshly-seeded
   rows) and `accepted_only` is not exercised by this golden set.
5. **Fusion** — memhub's exact formula and defaults, reimplemented
   verbatim from `src/retrieval/recall.rs`'s `score()` and
   `src/retrieval/util.rs`'s `normalize_fts`/`cosine_similarity`:
   `relevance = 0.5·norm(lexical) + 0.5·cosine`, `score = relevance·1.0 −
   0 − 0` (age half-life off ⇒ decay multiplier is always exactly `1.0`;
   the fixture has no stale or superseded rows, so both penalties are
   always exactly `0.0`).
6. **Scope merge** — see §3's licensed simplification.
7. **Rerank** — top-20 pool (`rerank_candidate_pool = 20`) reranked by
   `inference.RerankRunner` (ms-marco-MiniLM-L-6-v2), `min_rerank_score =
   2.0` / `doc_min_rerank_score = 0.0` floors applied to the reranked
   order, truncated to `max_results = 6` — mirrors `recall.rs`'s
   `finalize()` step for step.
8. Result shape carries `rank, source_type, scope, id, title, body,
   score` (`resultHit` in `pipeline.go`) — the subset of memhub's
   `RecallHit` this eval harness's matchers actually consult.

`tests/golden/eval.go` mirrors memhub's `src/commands/eval.rs`
(`evaluate_query`, `hit_matches`, `summarize`) field-for-field: `kind:
match` passes when some hit in the top `min(k, len(results))` has the
expected `source_type` (when given) and contains every
`title_contains`/`body_contains` substring case-insensitively; `kind:
empty` passes iff the result set is empty; `safety_failures` counts failed
`empty` queries.

## 5. Per-query results

All 21 `match` queries passed and the 1 `empty` safety probe passed, under
**both** lexical configurations. `tests/golden/golden_test.go`'s
`logSummary` prints every failing query's id, kind, and failure reason
verbatim (`t.Logf("[%s] FAIL %s (%s): %s", …)`); there was nothing to
print on either run:

```
[FULLTEXT] queries=22 match=21/21 empty=1/1 recall@3=1.0000 safety_failures=0
[BM25] queries=22 match=21/21 empty=1/1 recall@3=1.0000 safety_failures=0
gate config: FULLTEXT (primary)
--- PASS: TestRetrievalGolden (5.23s)
```

The `doc-`/`semantic-doc-` pair (doc-chunk ranking path) and the
`global-`/`semantic-global-` pair (cross-scope path) — the two
previously-zero-coverage paths this golden set added in memhub's Wave 4
R10 — passed on the first run with no tuning of the fixture or matchers,
which is some evidence that this rig's licensed global-scope simplification
(§3) did not paper over a real gap.

## 6. Measured FULLTEXT write-cost observation

A throwaway (not part of the shipped harness — not committed) timing probe
inserted 200 single-row facts into two otherwise-identical tables in a
fresh disposable store: one with no FULLTEXT key, one with `FULLTEXT KEY
(key, value)` matching §6.1's `ft_facts`. 200 inserts took **321ms**
without the FULLTEXT key and **554ms** with it — roughly **1.7×** the
per-insert cost at this tiny scale (single-row `INSERT`s, no batching).
This is directional evidence for PRD §6.1's existing note ("FULLTEXT costs
on write are real … acceptable at memory-table row counts") rather than a
precise measurement of it: `information_schema.tables` does not surface
Dolt's internal FULLTEXT index tables to a SQL client, so the specific
"five internal index tables per FULLTEXT key" figure that note cites
remains sourced to Dolt's own FULLTEXT blog post (§18's key sources), not
independently re-derived here. At the row counts this project actually
writes at (§17 R4's ~10³–10⁴-row memory corpora), a sub-millisecond-per-row
overhead is not a concern for M1/M2; it would matter more for a bulk
backfill or `index rebuild` over a large existing corpus, which is out of
this rig's scope.

## 7. MD5 recommendation

**Dolt FULLTEXT first, per PRD §8.1's default — no contingency needed on
the measured evidence.** Both configurations clear the gate identically on
this golden set, so there is no recall-quality reason to prefer BM25; the
tie-break falls to the option requiring less code in the shipped product
(§8.1's step 1 as designed, versus an additional in-process index the
in-repo tables would need to stay in sync with on every write). The §8.1 R2
in-process BM25 contingency stays implemented in this rig
(`tests/golden/bm25.go`) and behind the same harness, ready to flip on if a
real project's query mix or corpus shape someday separates the two —
deciding on measurement, not vibes, per §8.1's own instruction, applies
just as much to *not* switching as it would to switching.

## 8. What could not be measured, and what was assumed

**One platform, one corpus size.** Everything here is Windows/amd64 at
~19 seeded rows — the PRD's own "memory scale" (10³–10⁴ rows) is not
exercised; nothing here speaks to whether Dolt FULLTEXT's NL-mode ranking
still tracks BM25 closely at that scale, only that it does at golden-set
scale. **The global-store merge simplification (§3)** is licensed by this
issue's acceptance criteria but is a real simplification, not a proof that
a literal two-store merge will produce identical numbers — that is M2/M9's
question to answer when that plumbing exists. **The write-cost probe (§6)**
is a single throwaway 200-row timing, not a proper benchmark (no
warm-up, no repetition, no isolation from Dolt's `NOW()`/commit
overhead in the two loops) — treat the 1.7× figure as an order-of-magnitude
signal, not a load-bearing number.

## 9. What this resolves in the PRD

- **§8.1, "R2 contingency" `[V]`** — resolved: Dolt FULLTEXT clears the
  golden gate as measured; the BM25 contingency is built and validated
  but not needed. Marker text updated in place with a pointer here.
- **§17, R2 row** — updated in place: mitigation ("Golden gate in M0;
  in-process BM25 contingency") executed; gate passed on Dolt FULLTEXT.
  Pointer to this document added.
- **§19, MD5** — updated in place: "Dolt FULLTEXT first" adopted, decided
  by this rig's measurement; BM25 contingency retained, unused. Pointer to
  this document added.
- **§6.1's FULLTEXT-write-cost note** — annotated with §6's directional
  measurement and a pointer here; the "five internal index tables" figure
  itself remains sourced to Dolt's FULLTEXT blog post, not independently
  reproduced (§6).

## 10. Reproducing this

```sh
export CGO_ENABLED=1
export GOFLAGS=-tags=gms_pure_go

# Same staged models + onnxruntime shared library as rig 2
# (docs/spikes/m0-rig2.md §9):
export MEMDOLT_PARITY_MODEL_DIR=<staged model dir>
export ONNXRUNTIME_SHARED_LIBRARY_PATH=<path to an onnxruntime 1.26.0 shared library>

go test -tags golden,gms_pure_go ./tests/golden/... -v -timeout 30m
```

Omitting either environment variable fails loudly with the same
fetch/stage instructions rig 2 documents (`tests/inference.RequireEnv`,
shared code — see docs/spikes/m0-rig2.md §9). `retrieval_golden.json`'s
query count is drift-guarded (`TestRetrievalGolden` fails loudly if it
isn't exactly 22) the same way memhub's own hermetic test guards its copy.
