# M0 rig 3 — retrieval golden gate

PRD §16's third M0 rig: *"Retrieval rig: golden set on Dolt FULLTEXT +
brute-force cosine."* Its exit gate (§16, restated in §17's R2 row) is
**"Recall@K ≥ memhub baseline (or the BM25 contingency proves it)."** This
is also the MD5 decision input: *"Dolt FULLTEXT first; in-process BM25
contingency on golden-gate failure — decided by M0 measurement"* (§19).

## 1. Verdict

**PASS on the letter of the gate — Dolt FULLTEXT reproduces memhub's
baseline — but the measurement cannot discriminate the lexical step on this
golden set, and that is itself the headline finding, not a footnote.**

| Gate condition (PRD §16 / §17 R2) | Measured |
|---|---|
| Recall@3 ≥ memhub baseline (100%, 21/21) | **100%, 21/21** (Dolt FULLTEXT) |
| Zero safety failures | **0** (Dolt FULLTEXT) |
| Failing queries | **none**, in any configuration measured (§4) |

Three lexical-gather configurations were run through the identical
downstream pipeline (vector gather, fusion, rerank, floors, truncation —
unchanged), so a recall difference between them would isolate cleanly to
step 1:

| Lexical gather (§8.1 step 1) | Recall@3 | Match passes | Safety failures |
|---|---|---|---|
| Dolt FULLTEXT (`MATCH … AGAINST … IN NATURAL LANGUAGE MODE`) | 100% | 21/21 | 0 |
| In-process BM25 (§8.1 R2 contingency) | 100% | 21/21 | 0 |
| **Vector-only control (no lexical gather at all)** | **100%** | **21/21** | **0** |

**The third row is the finding.** A control with step 1 removed entirely —
every query's candidate pool built from cosine similarity alone — passes
the golden gate exactly as well as either lexical implementation. That is
not "FULLTEXT and BM25 happen to agree"; it is "this golden set, at this
corpus size, cannot tell any of the three apart," and the reason is
structural, not a property of either scoring formula: `seedRows()` seeds
21 rows total and `rerankPoolSize = 20` (`tests/golden/pipeline.go`), so on
most queries the fused candidate pool never exceeds the pool cap — nearly
every seeded row reaches the cross-encoder regardless of what step 1
contributed to its fusion score, and the cross-encoder alone decides the
top 3. Pool membership is essentially never contested at this scale, so
FULLTEXT, BM25, and no lexical gather at all cannot differ on outcome here.
(memhub's own hermetic fixture has the same property — comparably sized,
same `rerank_candidate_pool = 20` default — which is itself worth recording
as an M0 finding: memhub's own golden gate has never discriminated its
lexical step either.) §4 and §8 detail this further; §7's MD5
recommendation is reworded to rest on grounds this measurement actually
supports.

### What this PASS does and does not license

It says: PRD §8.1's pipeline, run against a disposable embedded Dolt store
carrying §6.1-shaped FULLTEXT tables and embeddings built through the
rig-2 inference path (`tests/inference`, shared with `tests/parity`),
reproduces memhub's own hermetic Recall@3 baseline on the identical
22-query golden set and a faithfully-ported fixture corpus (§3). Every
query kind the golden set exercises passed: keyword-style queries,
`semantic-`-prefixed paraphrase queries that lean on the vector+rerank path
(near-zero keyword overlap by design), the `doc-`-prefixed doc-chunk path
(gated by `doc_min_rerank_score`), the `global-`-prefixed cross-scope path
(seeded as scope-tagged rows in the same store per this issue's license —
see §3), and the one `kind: empty` safety probe.

It does **not** say Dolt FULLTEXT's lexical quality was validated against
BM25's, or against no lexical gather at all — the vector-only control above
means this golden set cannot support that claim at its current size. It
does not say Dolt FULLTEXT's natural-language relevancy formula is
numerically equivalent to memhub's SQLite `bm25()` — it isn't (§4). It does
not cover a larger or more adversarial corpus: everything here runs at
~21 rows, well under the low end of the PRD's own "memory scale" (10³–10⁴
rows, §17 R2's mitigation note) — the row count where pool contention, and
therefore actual lexical-step discrimination, would start to happen.

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
| Full pipeline run time | ~7.6s (schema + seed + embed 21 rows + 66 query runs [22 FULLTEXT + 22 BM25 + 22 vector-only control], each query reranking a pool of up to 20 candidates) |

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
hits), 1 task, **3 doc chunks**, and 2 global-scope rows (21 rows total).

The doc-chunk port went through a correction during PR #16 review: the
first version seeded one truncated, hand-shaped "chunk" instead of running
memhub's own chunking logic against the ingested markdown. memhub's
`doc::add` splits an ingested document through `chunk_markdown`
(`src/commands/doc.rs`, ~line 541): one chunk per ATX heading section, the
heading line kept in the chunk's body, `heading_path` carrying the full
ancestor breadcrumb (e.g. `Rust Code Style Guide > Error Handling`, not
just the leaf heading). Hand-tracing that function against
`seed_hermetic_corpus`'s markdown (`# Rust Code Style Guide` /
`## Error Handling` / `## Naming`) produces **three** chunks, not one:
a title-only chunk (`heading_path = "Rust Code Style Guide"`, body = just
that H1 line), the Error Handling chunk the golden queries actually target,
and a Naming chunk no golden query targets. All three are seeded now — a
faithful port means the rows a real ingest would *also* produce are present
in the candidate pool, not just the one row a matcher happens to check.
`doc_chunk_embed_text` (`src/retrieval/persist.rs`) embeds
`heading_path + "\n\n" + body` — the raw breadcrumb, not the hydrated
`"{doc title} — {heading_path}"` title `recall.rs`'s `load_source_row`
builds for matching/rerank — so `tests/golden/pipeline.go`'s `corpusRow`
now carries both fields separately (`title` for matching/rerank, distinct
from `headingPath` for the embed text and the FULLTEXT/BM25 lexical index).
Re-running the golden set against the corrected 3-chunk fixture produced
the same 21/21 result recorded above for both FULLTEXT and BM25 — the
correction did not change the outcome, but it does mean the doc-chunk
candidate pool a real query actually competes in (3 chunks of the same
document, not 1) is now represented.

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
21-row corpus where each golden query targets one specific row, this
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

1. **Lexical gather** (`fulltext.go` / `bm25.go` / a `noLexicalGatherer`
   control), per source type (`fact`, `decision`, `task`, `doc_chunk`),
   `LIMIT 50` (post-dedupe — see the fan-out finding below).
   - **FULLTEXT:** `SELECT id, MAX(score) FROM (SELECT id, MATCH(cols)
     AGAINST (? IN NATURAL LANGUAGE MODE) AS score FROM t WHERE MATCH(cols)
     AGAINST (? IN NATURAL LANGUAGE MODE)) AS matches GROUP BY id ORDER BY
     score DESC LIMIT 50` against the §6.1 FULLTEXT-keyed columns
     (`facts(key, value)`, `decisions(title, rationale)`, `tasks(title,
     notes)`, `doc_chunks(heading_path, body)`). Dolt's NL-mode relevancy
     (`sql/expression/matchagainst.go` in `go-mysql-server`) is
     higher-is-better and is **not** the same formula as SQLite's `bm25()`
     memhub calls (which is lower-is-better and gets negated in
     `recall.rs`'s `score()`) — it is `log(doc_count) + 1` scaled by a
     row-length normalization factor and a global-count multiplier, closer
     to the classic MySQL/MyISAM full-text relevancy than to Okapi BM25.
     `normalize_fts` (below) makes the sign and scale irrelevant to the
     fusion formula either way; only the *ranking* the raw score induces
     matters, and it induced the same top-3 winners as memhub's on this
     corpus (with the caveat in §1: this golden set can't tell that apart
     from *no ranking at all*).
     
     **A previously-unknown Dolt FULLTEXT property, found during PR #16
     review:** a `MATCH ... AGAINST` filter in a `WHERE` clause does not
     return one row per matching document — it returns one row per matched
     *index term*, so a row whose FULLTEXT-keyed columns contain several
     query words comes back several times, once per matched word, every
     copy carrying the identical accumulated relevancy (measured directly:
     one decision-type query against this rig's own 13-decision fixture
     returned 31 rows for 14 distinct ids). A bare `LIMIT 50` therefore caps
     *result rows*, not distinct candidates — harmless at this rig's row
     count, where the fan-out never approached 50, but at a real project's
     corpus it would silently drop distinct candidate rows whose match
     happened to land past the row-not-candidate cutoff, and M1's retrieval
     code inherits this if it copies the naive query shape. The fix
     (`fulltext.go`'s `Gather`) wraps the match in a subquery and
     `GROUP BY id` before `LIMIT` applies, collapsing the fan-out to one row
     per id first.
   - **BM25 (contingency):** in-process Okapi BM25 (`k1=1.2, b=0.75`)
     over an inverted index built once per source type from the same
     in-memory corpus, no Dolt query involved at all.
   - **Vector-only control:** `noLexicalGatherer` — always returns zero
     hits, so the candidate pool is vector-gather-only. Exists purely to
     answer whether step 1 discriminates anything at this corpus size (§1);
     it does not.
   
   **Lexical semantics differ from memhub's in a way the relevancy formula
   above doesn't capture.** memhub's `build_fts_match`
   (`src/retrieval/util.rs:37-49`) joins query tokens with `AND`, so SQLite
   FTS5 requires *every* token to appear before a row matches at all — a
   strict, all-or-nothing lexical gate. Both of this rig's configurations
   are OR-of-terms: Dolt's NL-mode relevancy accumulates a score per
   matched word with no requirement that all of them match (a document
   containing just one query word still scores, just lower), and the BM25
   contingency sums per-term scores the same way. This is a materially
   larger behavioral difference than the relevancy-formula difference
   discussed above, and this golden set does not exercise it either — every
   `match`-kind query's target row contains enough of the query's tokens
   that AND-vs-OR never changed which rows were even eligible to match.
2. **Vector gather** — brute-force cosine (`util.rs`'s exact formula,
   reimplemented in Go) between the query's BGE embedding and every seeded
   row's embedding (built once at seed time via `inference.EmbedRunner`),
   restricted to the requested source types, `cosine < 0.0` excluded. This
   step runs identically in all three configurations, including the
   vector-only control — only step 1's contribution to the fused score
   changes between them.
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
**all three** configurations, including the vector-only control.
`tests/golden/golden_test.go`'s `logSummary` prints every failing query's
id, kind, and failure reason verbatim (`t.Logf("[%s] FAIL %s (%s): %s",
…)`); there was nothing to print on any of the three runs. The gate itself
(§19 MD5's shipped choice) binds to FULLTEXT only — a run where FULLTEXT
alone stops clearing it now fails the test (`t.Errorf`), independent of
whether BM25 or the control would have passed:

```
[FULLTEXT] queries=22 match=21/21 empty=1/1 recall@3=1.0000 safety_failures=0
[BM25] queries=22 match=21/21 empty=1/1 recall@3=1.0000 safety_failures=0
[VECTOR-ONLY (control, no lexical gather)] queries=22 match=21/21 empty=1/1 recall@3=1.0000 safety_failures=0
note: the vector-only control also cleared the gate — on this golden set's corpus size, the lexical step never had to discriminate the rerank pool (recall@3=1.0000)
--- PASS: TestRetrievalGolden (7.63s)
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
this rig's scope. This probe is not part of the committed harness and
cannot be re-run from `go test`; the two numbers above are a one-time
observation, not a reproducible measurement — treat them accordingly (§8
repeats this caveat where it's load-bearing for the PRD's wording).

## 7. MD5 recommendation

**Dolt FULLTEXT first, per PRD §8.1's default.** This is *not* a
recall-quality conclusion — §1's vector-only control means this golden set
cannot separate Dolt FULLTEXT from BM25 from no lexical gather at all, so
there is no measured evidence that FULLTEXT's lexical quality is better,
worse, or equal to the contingency's. The recommendation rests on two
grounds that don't need that comparison: (1) it is the PRD's own §8.1
default, so keeping it is the lower-friction choice absent a reason to
switch, and (2) it ships less code — no additional in-process index that
the shipped product would need to keep in sync with every write, on top of
the FULLTEXT tables §6.1 already commits to. **The golden set, at its
current ~21-row size, is not evidence for this choice — it is evidence
that the choice doesn't matter yet.** The §8.1 R2 in-process BM25
contingency stays implemented in this rig (`tests/golden/bm25.go`) and
behind the same harness, ready to flip on if a real project's query mix or
corpus shape someday actually separates the two — which this measurement,
honestly read, says has not yet been tested. A future re-run of this rig at
something closer to the PRD's own "memory scale" (10³–10⁴ rows, enough to
put real pressure on `rerank_candidate_pool = 20`) is the measurement that
would actually decide MD5 on lexical-quality grounds, not this one.

## 8. What could not be measured, and what was assumed

**The corpus is too small for this golden set to discriminate the lexical
step at all — the load-bearing finding of this rig, not a footnote.**
`rerankPoolSize = 20` (`pipeline.go`) and the seeded corpus is 21 rows;
almost every query's fused candidate pool never exceeds the pool cap
regardless of what step 1 contributes, so the cross-encoder alone decides
the top 3 in practice. The vector-only control (§1, §5) confirms this
directly: removing the lexical step entirely still clears the gate. Nothing
measured here says whether Dolt FULLTEXT's NL-mode ranking tracks BM25 (or
beats no lexical gather at all) once the corpus is large enough that pool
membership is actually contested — that is a genuinely open question this
rig's numbers do not answer, at the PRD's own "memory scale" (10³–10⁴ rows,
§17 R2's mitigation note) or beyond. **The AND-vs-OR lexical-matching
difference (§4)** between memhub's `build_fts_match` and both of this rig's
lexical gatherers is likewise unexercised by a golden set where every
target row already contains enough of the query's tokens for that
difference not to matter. **The global-store merge simplification (§3)**
is licensed by this issue's acceptance criteria but is a real
simplification, not a proof that a literal two-store merge will produce
identical numbers — that is M2/M9's question to answer when that plumbing
exists. **The write-cost probe (§6)** is a single, non-committed,
throwaway 200-row timing, not a proper benchmark (no warm-up, no
repetition, no isolation from Dolt's `NOW()`/commit overhead in the two
loops, not reproducible from `go test`) — treat the 1.7× figure as an
order-of-magnitude anecdote, not a load-bearing number.

## 9. What this resolves in the PRD

- **§8.1, "R2 contingency" `[V]`** — resolved to the extent this rig can:
  Dolt FULLTEXT reproduces memhub's Recall@3 baseline, and the BM25
  contingency is built and validated as a working fallback — but the
  measurement cannot show FULLTEXT is *better than* the contingency (or
  than no lexical gather at all) at this corpus size (§1, §8). Marker text
  updated in place with a pointer here, worded to that effect.
- **§17, R2 row** — updated in place: mitigation ("Golden gate in M0;
  in-process BM25 contingency") executed and the gate passed on Dolt
  FULLTEXT, but R2's actual lexical-quality question — does Dolt FULLTEXT
  hold up against BM25 at real corpus sizes — is explicitly **not**
  resolved by this measurement; the row is worded as still partially open.
  Pointer to this document added.
- **§19, MD5** — updated in place: "Dolt FULLTEXT first" adopted on the
  §7 grounds above (PRD default + less shipped code), not on a
  recall-quality comparison this measurement can't support. Pointer to
  this document added.
- **§6.1's FULLTEXT-write-cost note** — annotated with §6's one-off,
  non-reproducible observation and a pointer here; the "five internal
  index tables" figure itself remains sourced to Dolt's FULLTEXT blog
  post, not independently reproduced (§6).

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
