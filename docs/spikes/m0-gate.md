# M0 — the consolidated go/no-go decision

PRD §16's M0 row is the project's stop-or-continue gate: *"Recall@K ≥
memhub baseline (or the BM25 contingency proves it); zero data-loss events
in the concurrency soak; push/pull round trip clean. **Fail → project
stops; write up findings.**"* Four rigs were run against it. This document
records the decision, the measured evidence for each gate condition, and
the limits the decision carries.

It is a consolidation, not a new measurement. Every number below is quoted
from `docs/spikes/m0-rig1.md` through `m0-rig4.md`; nothing was re-run to
write it.

## 1. Decision

**GO**, recorded 2026-08-02.

All three of §16's exit-gate conditions are met. None of the three is met
as widely as its wording suggests, and §16's *scope* column carries one
item that did not complete (§5). §16 is explicit about the consequence of
a miss — "Fail → project stops" — and no condition was missed, so the
project continues into M1 under the limits in §4 and the obligations in
§6.

| Exit-gate condition | Rig | Measured | Met |
|---|---|---|---|
| Recall@K ≥ memhub baseline (or the BM25 contingency proves it) | 3 | Recall@3 = 1.0 (21/21), 0 safety failures, on memhub's own 22-query golden set — equal to the memhub baseline, on Dolt FULLTEXT, so the contingency branch was never needed | Yes, but the golden set cannot discriminate the lexical step it appears to test (§3.1) |
| Zero data-loss events in the concurrency soak | 1 | 0 lost, 0 corrupted, 0 phantom, 0 unledgered out of 121,180 acknowledged writes; 0 of 2,376,072 reads unable to see a committed row | Yes, for inserts, on Windows/amd64, against process death (§3.2) |
| push/pull round trip clean | 4 | Both directions between two physical machines through the Pi hub, each verified by a fresh re-clone rather than a push exit code | Yes, over linear history, one writer at a time (§3.3) |

The four rigs' verdicts, in the words each note used:

| Rig | Document | Verdict as written |
|---|---|---|
| 1 — embedded-driver concurrency soak | `docs/spikes/m0-rig1.md` | **PASS** |
| 2 — ONNX-in-Go parity harness | `docs/spikes/m0-rig2.md` | **"PASS — with one load-bearing caveat that changes what 'PASS' means."** |
| 3 — retrieval golden gate | `docs/spikes/m0-rig3.md` | **"PASS on the letter of the gate — Dolt FULLTEXT reproduces memhub's baseline — but the measurement cannot discriminate the lexical step on this golden set, and that is itself the headline finding, not a footnote."** |
| 4 — Dolt hub, two-client round trip | `docs/spikes/m0-rig4.md` | **PASS** |

Each of those four notes bounds its own verdict, though not all in the
same way: rigs 1 and 4 say in a sentence that their verdict is that rig
only and that M0's go/no-go needs the others, while rigs 2 and 3 do it
through a "What this PASS does and does not license" section instead.
Four passes do not make a milestone verdict by addition: what makes this a
GO is the three conditions in §3, and what bounds it is §4.

## 2. Which rig answers which gate condition

The gate has three conditions and there are four rigs, so the mapping is
not one to one. Two asymmetries matter enough to state before the
evidence.

**Rig 2 has no exit-gate condition of its own.** ONNX-in-Go parity appears
in §16's *scope* column, not its exit gate. It is nonetheless load-bearing
under condition 1: rig 3's embeddings and rerank scores are produced by
the rig-2 inference path (`tests/inference`, factored out of
`tests/parity` and shared by both harnesses), so rig 3's Recall@3 is only
as trustworthy as rig 2's parity result. Rig 2 measured 31/31 texts
tokenizing byte-identically to memhub's fastembed reference on both
models, a maximum embedding deviation of 1.79×10⁻⁷ against a 1×10⁻³
tolerance, and bit-identical rerank scores across all 14 probe pairs
against a 1×10⁻² tolerance — the last two despite the Go and Rust sides
linking two different onnxruntime minor releases (1.26.0 and 1.24.2) over
byte-identical model files.

That package path is historical. Before M2, `tests/inference` supplied both
rigs; after M2, both rigs consume production `internal/embedding` and the
test-only package is gone. The recorded parity measurements and their limits
still hold because the tokenizer and ONNX session logic moved without a second
implementation.

**Merge is not a gate condition.** §16's scope line reads
"clone/pull/push/**merge** round trip"; its exit gate reads only
"push/pull round trip clean". The gate is the operative reading, which is
the reading rig 4 was run under and the reading this GO uses. A
fast-forward pull never invokes the merge algorithm at all — it detects
that local HEAD is an ancestor and moves a pointer, with no cell
comparison, no second-parent commit and no conflict-table population — so
rig 4's transport passes do not transfer to merge, and this GO does not
claim merge works (`docs/spikes/m0-rig4.md` §6). Merge questions belong to
M1 and M4 and are being answered there on their own evidence; they are not
folded into this verdict in either direction.

## 3. The exit gate, condition by condition

### 3.1 "Recall@K ≥ memhub baseline (or the BM25 contingency proves it)"

**Met, as equality, on Dolt FULLTEXT.** K = 3 per PRD §8.4.

| | memhub baseline | memdolt (Dolt FULLTEXT) |
|---|---|---|
| Recall@3 | 1.0 — 21/21 match queries | **1.0 — 21/21** |
| Safety failures (failed `empty` queries) | 0 | **0** |
| Golden set | `tests/retrieval_golden.json`, 22 queries (21 `match` + 1 `empty`) | byte-identical copy, verified with `diff` at port time |

The baseline was re-measured rather than assumed: memhub v0.2.0's
`cargo test --test retrieval_harness retrieval_golden_hermetic` returned
3/3 pass, `recall_at_k == 1.0`, 0 safety failures on the same shipped file
(`docs/spikes/m0-rig3.md` §2). The gate's escape hatch — "or the BM25
contingency proves it" — was never needed: Dolt FULLTEXT cleared the gate
directly. The contingency was built and run anyway and scored identically,
which is why it stays implemented in reserve rather than being discarded.

**How narrowly this is met, and it is narrower than it reads.** Rig 3 ran
a third configuration — a vector-only control with the lexical gather
removed entirely — and it also scored 100%, 21/21, 0 safety failures. The
mechanism is measured, not guessed: the fused candidate pool is 21 rows
(every seeded row) on 22 of 22 queries in all three configurations against
a `rerank_candidate_pool` of 20, so exactly one row is evicted per query,
and the evicted row differed by lexical configuration on 4 of 22 queries
but was never one of that query's top-3 targets. So the gate condition is
satisfied on its own terms — memdolt reproduces memhub's baseline — while
the golden set at its current ~21-row size cannot distinguish a good
lexical gather from a bad one or from none at all. PRD §17's R2 row and
§19's MD5 both already record this in place. MD5's adoption of Dolt
FULLTEXT rests on it being the PRD's own §8.1 default and on shipping less
code, **not** on measured recall superiority, and the re-measurement that
would decide it on quality grounds needs a corpus at the PRD's own memory
scale (10³–10⁴ rows).

Also unexercised by this condition: the AND-vs-OR difference between
memhub's all-tokens-required FTS5 match and both of rig 3's OR-of-terms
gatherers; a literal two-store global-scope merge (rig 3 seeded scope as a
tagged column in one store, licensed by its own acceptance criteria); and
any platform other than Windows/amd64.

### 3.2 "Zero data-loss events in the concurrency soak"

**Met.** Totals over rig 1's eight scenario runs:

| Gate condition, as fixed in code | Measured |
|---|---|
| Acknowledged writes absent on re-open in a different process | **0** of 121,180 |
| Acknowledged writes whose stored value differed | **0** |
| Acknowledged writes unreadable after unclean-kill recovery | **0** of 33,109 acknowledged before a kill |
| Committed rows a concurrent reader could not see | **0** of 2,376,072 reads |
| Rows in the store no ledger intent accounts for | **0** |
| Stale-LOCK recovery after an unclean kill | recovered, 3 of 3 kill runs |

162,841 writes were attempted and 121,180 acknowledged; the 41,661
difference is entirely from the three kill runs, after the owner was
dead. Row counts never regressed in 2,376,072 reads, and across 121,180
concurrent commits not one engine-level failure of any class was observed.

**Why the zeros are checkable, which is the whole question with a zero.**
The data-loss definition was fixed in code (`tests/soak/summary.go`'s
`decide`) before anything was measured. The ledger each writer reconciles
against is independent of the store, flushed to the OS before the write is
attempted, with SHA-256 payload digests so a changed value is detected as
changed. There is no retry anywhere in the harness. A negative control
(`TestReconciliationDetectsWhatItClaimsTo`) feeds the reconciler a store
and a ledger that disagree in every way it claims to detect and requires
FAIL; during rig 1's review a further ten mutations were injected into the
reconciler — including one that always returns PASS — and the negative
control caught all ten (recorded in the project's memhub decision log,
not in the rig note).

**How narrowly this is met.** The soak **inserts only**. It does not
update the same row from two writers, delete, branch, or merge, so PRD
§6.3's conflict machinery is entirely unexercised and M1 must not read
this PASS as covering merges. **Cross-process read visibility is checked
at aggregate granularity only**, so the 2,376,072-read figure above is
narrower than it looks: each reader verifies key by key the writes *its
own process* was told were committed, and never another process's keys,
because knowing which of those are committed would need coordination the
rig deliberately does not have. What does span processes is the row count,
which every reader samples and which never decreased. It is Windows/amd64
only — the harshest of
the three targets for this rig, but Linux and macOS behaviour of `flock`,
`SIGKILL` and port exhaustion is inferred from code, not measured. It
kills processes, not machines: nothing here speaks to power loss or a
kernel panic. There were three kill events in total, which is thin
evidence for a rare-window phenomenon. And six of the eight runs are
preserved as table rows rather than verbatim summaries, so only two runs
can be re-read at full fidelity.

The soak also produced findings that are not gate failures but are now
load-bearing for M1, all folded into PRD §17: the silent read-only
downgrade of a foreign opener (R1 — worse than the "database is locked"
the register first anticipated, and not removable by memdolt's own lock,
which binds only processes that take it); writes over IPC being
at-least-once, caught happening (R9, which makes §6.1's client-minted ULID
PKs load-bearing rather than tidy); write throughput decaying with history
size rather than concurrency (R8); and ~16.8 KB of history per single-row
commit, about 4× §13.3's rule of thumb (R4).

### 3.3 "push/pull round trip clean"

**Met, in both directions, verified by re-clone.**

- Windows → Pi hub → macOS: the macOS client re-cloned into a clean
  directory and received the Windows commit.
- macOS → Pi hub → Windows: a fresh Windows clone received the macOS
  commit.
- `dolt pull` was exercised as a distinct operation, forced into a real
  fast-forward, rather than letting clone stand in for it.

No step was accepted on a push exit code; each clone's contents were
checked against what should be there. The hub is `dolt sql-server` with
remotesapi on port 50051, running as a systemd unit on a Pi 5 (aarch64)
over Tailscale, with SQL-grant (`CLONE_ADMIN`) auth. Hub and both clients
run Dolt v1.88.1 — deliberately pinned to match the client's embedded
driver so that a failed round trip could not be ambiguous between "remotesapi
doesn't work" and "a v1 client cannot talk to a v2 hub". Version skew
against Dolt v2.x is therefore *not* measured here; it is M4's
version-skew-guards question.

**How narrowly this is met.** All history in rig 4 is linear. Divergent
history, conflicts, concurrent writers, and non-fast-forward push
rejection were never exercised. Each client wrote exactly one row, and no
two clients wrote at the same time. Merge is out of scope by the gate's
own wording (§2).

Two properties of the rig-4 hub are asserted rather than measured, and
both belong with any statement that the hub is "secured": remotesapi in
v1.88.1 cannot be bound to a single address, so the tailnet-only property
rests on an nftables drop rule **that has never been probed from an
off-tailnet host**, and the rig4 database credential was pasted into
session transcripts three times and has not been rotated. The deferral was
accepted for the duration of the spike only, on the strength of that same
unprobed tailnet premise, and it guards only the scratch database
`rig4smoke`. Both are carried in §6.

**No hub performance figure exists.** Rig 4 ran on the Pi's SD card, not
an SSD, and took no timing or throughput measurements at all, so nothing
from it may be read as a deployment estimate. The SSD is a recommendation
for real deployment, not a requirement — memdolt must not refuse to run
without one.

## 4. What this GO does and does not license

It says: the four foundations M0 existed to test are sound enough to build
M1 on. The embedded driver under a single-owner process does not lose
acknowledged writes under sustained concurrent load or across an unclean
kill of the owner; ONNX inference in Go reproduces memhub's embeddings and
rerank scores to float32 noise; the ported retrieval pipeline reaches
memhub's own Recall@3 baseline on memhub's own golden set; and two real
machines can push and pull through a self-hosted Dolt hub over Tailscale.

It does not say:

- **That merges work.** Nothing in M0 exercised a merge, a conflict, or
  divergent history — not rig 1 (inserts only), not rig 4 (linear history,
  fast-forward pull). PRD §6.3's conflict machinery is unexercised by this
  gate.
- **That the store is safe against a second writer.** §5.2's single-owner
  rule is doing real work. A process that never takes memdolt's lock is
  silently downgraded to read-only and finds out only at its first write;
  memdolt's own lock cannot bind it.
- **That Dolt FULLTEXT is the right lexical gather on quality grounds.**
  It is the adopted choice on other grounds (§3.1). The golden set could
  not tell it apart from BM25 or from no lexical gather at all.
- **That `sugarme/tokenizer` is safe as shipped.** Rig 2's byte-identical
  result holds only with caller-side NFD normalization compensating for a
  real bug in that library's `BertNormalizer.StripAccents`. Without it, any
  text carrying precomposed accented Latin characters mis-tokenizes
  silently. M1's `internal/embedding` inherits that obligation.
- **Anything about Linux or macOS for rigs 1–3.** All three are
  Windows/amd64 measurements. Rig 4 is the exception: it used real Windows
  and macOS clients against an aarch64 Linux hub.
- **Anything about durability across power loss**, non-Latin-script
  retrieval quality, long-input truncation, int8 quantization under
  adversarial inputs, or a corpus at the PRD's own 10³–10⁴-row memory
  scale.

## 5. §16's scope column: the `[verify]` sweep did not complete

Separately from the three exit-gate conditions, §16's scope column
requires M0 to *"Resolve every [verify]: ARM64 release asset, gc flag
semantics, shallow clones, Linux-AMD64 onnxruntime bundling, tokenizer
lib, current vector-index status."* Four of the six are disposed of; two
are not.

| §16 item | State |
|---|---|
| ARM64 release asset | **Satisfied in substance, marker not updated.** Rig 4 §5 downloaded `dolt-linux-arm64.tar.gz` from the dolthub/dolt v1.88.1 release, recorded its SHA-256, installed it on the Pi, and ran the hub on it. PRD §13.1 still reads **[L]** — "confirm the asset … during M0". Note also that the release publishes no checksums asset, so rig 4's hashes are cross-machine consistency references, not verification against a publisher-supplied hash |
| gc flag semantics (`dolt gc --full` / `--shallow`) | **Not resolved.** No rig covered it. PRD §13.3's marker is byte-identical to the PRD's first commit: **[L/verify]** — "pin during M0" |
| shallow clones | **Not resolved.** The PRD's only text on it is the same §13.3 line |
| Linux-AMD64 onnxruntime bundling | **Resolved** — rig 2 §6. There is no Linux-AMD64-specific gap because there is no bundling at all, on any platform; Linux AMD64 needs the same SHA-256-pinned fetch-and-stage treatment as the other three. PRD §8.3 updated in place |
| tokenizer lib | **Resolved** — rig 2 §4, conditionally: `sugarme/tokenizer` v0.3.0 *with* caller-side NFD normalization. PRD §8.3 and §14 updated in place |
| current vector-index status | **Not re-checked during M0.** The PRD has carried this at **[V]** since its first commit, sourced to DoltHub's own published statement (§18 cites blog posts from 2025-01-16, 2025-06-23 and 2025-09-03). No rig re-derived it, and the word "current" in §16's item is the part nothing addressed |

One further `[verify]` outside that list was resolved: §5.2.3's stale-lock
detection ergonomics, by rig 1 §7 — *the lock decides, the pid describes*
— now recorded at [V] in PRD §5.2 item 3.

**This does not fail the gate.** The exit-gate column names three
conditions and the `[verify]` sweep is not among them. It is unfinished
scope, recorded here rather than quietly rolled forward, and it becomes
blocking at the point §13.3's `memdolt gc --deep` is designed or any
document promises specific `--full`/`--shallow` semantics — M6 by §16's
own milestone table, not M1.

## 6. Obligations carried out of M0

1. **Rotate the rig4 hub credential.** Deferred for the spike only; it has
   been in session transcripts three times. It must be rotated before it
   guards anything beyond the scratch database (`docs/spikes/m0-rig4.md`
   §6).
2. **Probe the nftables drop rule from off the tailnet.** The
   tailnet-perimeter posture (PRD §13.1, §17 R5) is asserted, not measured,
   and it is the premise under obligation 1's deferral. Tracked as an open
   memhub task.
3. **Pin `dolt gc --full`/`--shallow` semantics** before M6's gc/retention
   work, and update PRD §13.3's marker (§5).
4. **Update PRD §13.1's ARM64 marker** to record what rig 4 already
   measured (§5).
5. **Do not read rig 1 as covering merges.** M1 lands proposal branches and
   merges; its evidence has to come from M1's own work.
6. **Carry rig 2's NFD compensation into `internal/embedding`.** It is a
   correctness requirement of the library choice, not a footnote.
7. **Keep client-minted ULIDs on the IPC write path.** Rig 1's F3 showed a
   real at-least-once write; §6.1's ULID PKs are what makes a retry
   idempotent, and M1 must not replace them with server-minted ids.
8. **Re-run the retrieval golden gate at 10³–10⁴ rows** before treating
   MD5's choice of Dolt FULLTEXT as a quality conclusion (§3.1).
9. **Run rigs 1–3 on Linux and macOS** before either is claimed for those
   platforms.
10. **No hub deployment estimate exists.** Any future timing figure taken
    on the SD-backed Pi must be annotated as SD-bound rather than carried
    forward as a deployment number.
