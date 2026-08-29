# memdolt — Product Requirements Document

**Document type:** PRD, written to be handed to a fresh agent as the starting authority for a new repository.
**Status:** design. Side project. memhub remains the operator's daily driver; nothing here changes memhub.
**Date:** 2026-07-29.
**Provenance:** synthesized from (a) the memhub repository at v0.2.0 (full feature inventory, the broad parity baseline), (b) the narrowly audited memhub v0.2.2 OpenCode 2 compatibility behavior (§11.4, §12; a supplement, not a full v0.2.1/v0.2.2 baseline), (c) live research against github.com/dolthub/dolt, docs.dolthub.com, and the DoltHub blog, and (d) the memhub r2 MCP spec (`memhub-mcp-implementation-spec-r2.md`, 2026-07-29) whose upgrades this PRD absorbs natively. Source URLs in §18.

---

## 0. How to read this document

### 0.1 Confidence markers

| Marker | Meaning |
|---|---|
| **[V]** | Verified against published Dolt/MCP/SDK docs or the memhub source tree during research (2026-07-29). |
| **[L]** | Likely — inferred from partial sources; confirm before building on it. |
| **[design]** | Proposal original to this PRD. Argue with it. |
| **[verify]** | Explicitly unverified. Resolve during M0 before dependent work. |

### 0.2 One-paragraph pitch

memdolt is a local-first CLI + MCP server, written in Go, that gives coding agents (Claude Code, Codex, OpenCode) durable per-repo project memory — facts, decisions, tasks, notes, reference docs — stored in **Dolt**, a MySQL-compatible database with git semantics. Every machine holds a full clone; a self-hosted **Dolt hub** (`dolt sql-server` on a Raspberry Pi 5 or Linux box, reached over Tailscale) is the shared remote you push/pull against. Version control is not a bolted-on sync layer: agent writes are **branches**, review is a **diff + merge**, cross-machine sync is **push/pull with real cell-level merges**, and history/provenance (`AS OF`, blame, log) are first-class product features. It reimplements memhub's proven feature surface — including its hybrid SQL+RAG recall pipeline and eval harness — on this foundation.

### 0.3 Relationship to memhub

- memhub (Rust + SQLite) is the **parity baseline and reference implementation**. Where this PRD says "as memhub does," the memhub source is the spec of record.
- v0.2.0 remains the broad parity baseline. The v0.2.2 material is only the named OpenCode 2 supplement in §11.4 and §12; it does not assert that all v0.2.1 or v0.2.2 behavior was audited.
- memdolt is a **separate product in a separate repo**. No shared code, no shared on-disk state, no requirement that the two interoperate live.
- One-way migration IS in scope: memdolt must import a `memhub export` JSON bundle (§15).
- The memhub r2 spec's upgrades (MCP `2026-07-28`, elicitation confirmations, hub topology, backup discipline, migration runbooks) are absorbed here as native requirements, not future work — with the parts Dolt makes obsolete explicitly retired (§13).

---

## 1. Product statement

**For** developers who run coding agents across multiple machines and want their agents to remember project state durably, **memdolt** is a self-hostable agent-memory system **that** treats memory as a versioned database: branch-staged agent writes, mergeable cross-machine sync against a hub you own, and queryable history of what was believed when. **Unlike** memhub's snapshot-based sync (where divergence is lossy and adopt is destructive) and unlike cloud memory products, memdolt's divergence resolves by merge, its audit trail is the commit graph itself, and the hub is a single ~100MB binary on hardware you control.

### 1.1 Goals

1. Feature parity with memhub v0.2.0 as inventoried in §12's matrix — same intent surface for agents, same operator workflows.
2. Retrieval quality parity: ported golden set must meet or beat memhub's Recall@K baseline (hard gate, §16).
3. Self-hosted hub: one `dolt sql-server` process serving both live SQL and push/pull remotes, documented for Pi 5 / Linux desktop over Tailscale.
4. Divergence is never lossy. There is no `sync_adopt`. Conflicts are surfaced, elicited, and merged.
5. History as a feature: "which session wrote this fact," "what did we believe last Tuesday," "what changed in this review" are queries, not forensics.
6. Everything the r2 spec proved out: MCP `2026-07-28` native, elicitation-gated destructive ops, CLI-only global promotion, format invariance across topologies, round-trip migration gates.

### 1.2 Non-goals (v1)

- Replacing memhub. The operator keeps using memhub; memdolt earns adoption or doesn't.
- DoltHub/DoltLab dependency. DoltLab needs 16GB RAM **[L]** and a web UI memdolt doesn't want; the hub is plain `dolt sql-server`.
- MCP Apps / web review panels (r2 §8 logic stands: terminal-first operator).
- MCP Tasks extension delivery (parked — Claude Code doesn't implement it **[V]**; design behind an interface, ship nothing).
- Dolt vector indexes (§8.3 — explicitly not production-ready per DoltHub **[V]**; brute-force cosine instead, revisit later).
- Real-time multi-writer collaboration on one branch. The concurrency model is per-machine clones + merge, not Google Docs.
- Windows as a **hub** host (client yes, hub no — same reasoning as r2 §13.5).

---

## 2. Personas & usage model

- **P1 — the operator (primary):** full-stack dev, 2–3 machines (Windows + macOS today), runs Claude Code/Codex/OpenCode CLIs, owns a Pi 5 and a Linux desktop, uses Tailscale. Wants memory that follows them across machines without a cloud account and without lossy sync.
- **P2 — the self-hoster (secondary):** anyone who clones the repo. Same shape as P1; may not have Tailscale (any private network works; auth guidance in §14.4). Docs must never assume P1's exact fleet.
- **P3 — the agent (machine user):** untrusted writer. Interacts only through the MCP surface; can propose durable memory but never promote it; can write scratch (notes, tasks) directly within defined lanes.

Daily loop (topology A, the default): session start → `pull` (fast-forward or auto-merge; conflicts elicited) → agent works, recall/locate/propose throughout → wrap-up → review proposals (diff + accept/reject) → `push`.

---

## 3. Core concept: memory as a versioned repository

Each project's memory is a Dolt database. The commit graph is the write-ahead log, the audit trail, and the sync unit all at once.

### 3.1 Branch model **[design]**

| Branch | Written by | Contents | Lifecycle |
|---|---|---|---|
| `main` | operator (via review/CLI) + direct-lane writes | durable truth | permanent |
| `proposal/<ulid>` | agent, via `propose_*` | exactly one staged fact/decision/supersede, one commit | merged on accept, deleted on reject |

- **One branch per proposal, not per session.** Branches are cheap in Dolt; per-proposal branches preserve memhub's per-proposal accept/reject granularity exactly, keep merges tiny, and make `review show <id>` a single-commit diff. **[design]**
- **Direct lanes** (no review gate, matching memhub's semantics): `tasks`, `session_notes`, `commands`, `project_state`/`project_arch` narratives, doc ingestion. These commit straight to `main`, **batched**: one commit per MCP tool call for tasks/narratives; notes accumulate in the working set and commit on session end or a 5-minute timer, whichever first (Dolt commits are prolly-tree updates — materially heavier than a SQLite INSERT — so note-per-commit is prohibited). **[design]**
- Commit metadata is load-bearing: author = normalized actor (`agent:claude-code`, `agent:codex`, `user`), message = structured one-liner (`propose fact msrv=1.24`, `note batch (3)`, `review accept #42`). `dolt_log` + `dolt_blame` then answer provenance queries with zero app code. **[V]** (system tables exist as documented.)
- Agents never `ALTER TABLE`. Schema changes ship only in migrations run by the binary on `main` (§6.4). This keeps Dolt schema-conflict machinery **[V]** out of the daily path entirely.

### 3.2 What the commit graph replaces from memhub

| memhub mechanism | memdolt equivalent |
|---|---|
| `writes_log` append-only audit table | `dolt_log` / `dolt_diff_<table>` / `dolt_blame_<table>` **[V]** |
| `pending_writes` staging table + status lifecycle | `proposal/*` branches; status = branch existence + merge state |
| snapshot manifest + logical-version digest + five verdicts | commit ancestry: ahead / behind / diverged-mergeable / conflicted |
| `sync_adopt` (destructive) | merge; conflicts elicited row-by-row; **no destructive path exists** |
| `.memhub/backups/` pre-adopt copies | any commit is a restore point; `dolt backup` for whole-instance copies **[V]** |

What the commit graph does **not** replace: `superseded_by` links (supersession is a semantic relationship between two rows that both stay in the table, not a storage-history fact — keep the columns; §6.1 scopes fact-key uniqueness so that both can), staleness/verification timestamps, and the review gate itself (branches are the *mechanism*; the gate — human accepts before durable — is policy and stays).

**Narrative history is a product feature, not incidental plumbing** **[design]**: because `project_state`/`project_arch` commit straight to `main` (§3.1), `dolt_log` over those two tables is a full timeline of how the project's own self-description changed over time. "How did our architecture evolve" is a `history` query (§11.1) against `project_arch`, not a manual diff of old PROJECT.md snapshots someone happened to keep.

---

## 4. Architecture overview

```
┌────────────── each client machine ──────────────┐      ┌────── hub (Pi 5 / Linux box) ──────┐
│ agent CLIs (Claude Code / Codex / OpenCode)     │      │  dolt sql-server                    │
│    │  stdio MCP                                 │      │    ├─ SQL :3306        (topology B) │
│ memdolt binary (Go)                             │      │    └─ remotesapi :50051 (push/pull) │
│    ├─ MCP server (go-sdk, 2026-07-28)           │◄────►│  databases: one per project + global│
│    ├─ CLI (cobra)                               │ tail │  systemd unit, dedicated user       │
│    ├─ Store: local Dolt clone (.memdolt/dolt/)  │ scale│  nightly `dolt backup` to 2nd target│
│    ├─ Embedding side-store (derived, local)     │      └─────────────────────────────────────┘
│    ├─ Code index (SQLite, derived, local)       │
│    └─ ONNX runtime (BGE-small + ms-marco int8)  │
└─────────────────────────────────────────────────┘
```

Key placements, each argued later:

1. **Compute stays on clients.** Recall (embedding + rerank) runs against the local clone. The hub never loads ONNX models. This deletes the r2 spec's Q3 risk (Pi inference latency) outright. **[design]**
2. **Embeddings are derived, machine-local, and NOT in the Dolt repo** (§8.2). The versioned repo holds only source-of-truth text.
3. **The code index is derived, machine-local SQLite** — identical reasoning to memhub/r2 §6.7: it describes the *local working tree*, which the hub cannot know. Never synced, never versioned.
4. **One long-lived process owns the local clone** (§5.2) — the embedded driver's cross-process locking is real **[V]** and the design must respect it rather than fight it.

### 4.1 Topologies

| Topology | What it is | Status |
|---|---|---|
| **A — clone + push/pull** (default) | Full local clone per machine; hub is the remote via remotesapi. Offline = fully functional (read AND write); reconcile by merge on reconnect. | v1 core |
| **B — live SQL to hub** | No local memory clone; Store speaks MySQL wire to `sql-server`, always-fresh, no pull step. Code index still local. | v1 stretch — the `Store` interface (§5.1) must support it from day one; shipping the remote impl may land in M4 |
| **C — solo local** | No remote configured. Everything works; `push`/`pull` report "no remote." | free — A minus a remote |

Topology is **per-project config** (r2 D7 amendment carried over). The r2 D3 trilemma (`fail`/`readonly`/`local`) is dissolved by design in A: hub-down means you keep working on the local clone and push later — that is the normal workflow, not a fallback mode. Topology B degrades to read-only-against-last-clone only if the operator opted into B; the PRD recommends A precisely to avoid that class of failure.

---

## 5. Storage layer

### 5.1 The `Store` interface **[design]**

The r2 spec's D1 (thin client vs shared command layer) resolves here as: **one Go interface, two implementations.**

```go
type Store interface {
    // memory CRUD, proposals, review, recall gathering, doc ops, render reads…
}
// LocalStore  — embedded Dolt driver over .memdolt/dolt/ (topologies A, C)
// RemoteStore — database/sql MySQL client to the hub    (topology B)
```

CLI and MCP server both route through `Store` — the r2 §7.5 lesson (CLI silently reading a stale local DB is a data-correctness bug) is a birth requirement here. `locate`/code-index bypass `Store` and are always local.

### 5.2 Embedded-driver concurrency — the #1 design constraint

Research findings **[V]**: `github.com/dolthub/driver` opens a Dolt data dir in-process ("akin to SQLite") with full version-control via SQL procedures (`dolt_commit`, `dolt_checkout`, `dolt_merge`, `dolt_push`, `dolt_pull` — all usable embedded). But the storage layer takes a filesystem `LOCK` file; stale LOCK files can survive unclean shutdown; DDL is not concurrency-atomic. The driver ships retry-with-backoff (~30s max default). Field reports from other embedders (beads/gastown) confirm these are production sharp edges, not theory.

**Corrected in M0 [V]:** this section first said two OS processes on the same repo dir "hit `database is locked`". They do not. The second process opens successfully, answers a ping, and reads normally; it has been silently downgraded to read-only, and finds out only at its first write, which fails with `cannot update manifest: database is read only`. That is a worse failure than the one anticipated — no health check can see it, and a writer believes it is writing until it commits. Dolt's own `LOCK` file also records no pid, so §5.2.3's recovery cannot be implemented against it. Both are why memdolt takes a lock of its own before the driver touches the data directory. See §17 R1 and `docs/spikes/m0-rig1.md`.

**Design response [design]:**

1. **Single-owner rule:** when the MCP server is running, it is the sole process holding the embedded store. CLI invocations detect a live server (pidfile + liveness probe in `.memdolt/`) and route through it over a local IPC endpoint (localhost HTTP on an ephemeral port with a per-run token — same pattern as memhub's viz auth). No live server → CLI opens the store directly.
2. **[V]** memdolt's single-owner advisory lock resolves the residual inter-process races (two CLIs at once): the second process is refused fast with a distinct error (~106 ms measured), before the driver's own retry path is ever reached. The driver's `BackOff` is deliberately left disabled — waiting is the opposite of what the single-owner rule wants when acceptance criteria call for the second process to fail fast. See `docs/spikes/m0-rig1.md`. Keep every transaction short.
3. Startup recovery: on open, if a lock file carries an ownership record and its **advisory lock is free**, the process that wrote it is gone: clear the record in place and log loudly with the stale pid. **[V]** (M0 rig 1) — the ergonomics marked [verify] here resolve as *the lock decides, the pid describes*. The kernel releases an advisory lock however a process exits, including a kill, so a free lock is proof; a pid cannot tell a live owner from an unrelated process that inherited a recycled pid, and rig 1 measured a correct recovery against a record naming a pid that was demonstrably alive. The record is cleared in place rather than unlinked, because unlinking a file while holding a lock on it lets two processes lock two inodes under one name. Numbers and method: `docs/spikes/m0-rig1.md`.
4. `doctor` checks: stale LOCK, orphaned pidfile, IPC reachability.

This is more machinery than memhub needed (SQLite WAL handles multi-process natively). It is the honest price of Dolt embedded, paid once, in one module.

### 5.3 On-disk layout

```
<repo>/.memdolt/            gitignored except config.example.toml
  dolt/                     Dolt data dir; database name = "memory"
  config.toml               per-machine (mirrors memhub key structure, §11.3)
  embeddings.sqlite         derived side-store (§8.2)
  code_index.sqlite         derived code index (§9)
  rendered/                 PROJECT.md, PROJECT_LEDGER.md
  server.pid / server.sock  single-owner machinery (§5.2)
~/.memdolt/
  models/                   ONNX models, SHA-256-pinned fetch (§11.2)
  global/                   clone of the hub's global database (§10)
  config.toml               machine defaults, hub URL(s), registry of known projects
```

`project_id` derives from the git remote, exactly as memhub — hub database names are `proj_<project_id>` (sanitized). **[design]**

---

## 6. Data model

### 6.1 Schema (Dolt / MySQL dialect) **[design]**

Faithful port of memhub's final schema (23 migrations collapsed to one initial DDL), with three deliberate *structural* changes: ULID string PKs replace AUTO_INCREMENT ids on agent-writable tables (merge-safe: concurrent inserts on two machines can never collide), no FTS shadow tables (Dolt FULLTEXT indexes replace them), and no `writes_log`/`pending_writes` (replaced per §3.2). Column-level departures (added/removed columns on ported tables) are called out in the notes below, not enumerated here.

```sql
-- durable memory (reviewed lane)
facts(id CHAR(26) PK, key VARCHAR(255), value TEXT,
      source VARCHAR(64) DEFAULT 'user', kind VARCHAR(64) NULL,
      evidence VARCHAR(1024) NULL,
      verified_at DATETIME NULL, created_at DATETIME,
      superseded_by CHAR(26) NULL,
      live_key VARCHAR(255) GENERATED ALWAYS AS
        (IF(superseded_by IS NULL, key, NULL)) STORED,
      KEY idx_fact_key (key),
      UNIQUE KEY uk_fact_live_key (live_key),
      FULLTEXT KEY ft_facts (value, key))
decisions(id CHAR(26) PK, title VARCHAR(512), rationale TEXT, summary TEXT NULL,
      alternatives_rejected TEXT NULL, evidence VARCHAR(1024) NULL,
      status ENUM('active','superseded','draft'), source VARCHAR(64) NULL,
      decided_at DATETIME, superseded_by CHAR(26) NULL,
      FULLTEXT KEY ft_decisions (title, rationale))
-- direct lanes
tasks(id CHAR(26) PK, title VARCHAR(512), status ENUM('open','done','blocked'),
      notes TEXT NULL, created_at DATETIME, updated_at DATETIME,
      FULLTEXT KEY ft_tasks (title, notes))
session_notes(id CHAR(26) PK, actor VARCHAR(64), actor_raw VARCHAR(255),
       text TEXT, session_id VARCHAR(255) NULL, agent_id VARCHAR(255) NULL,
       provider_id VARCHAR(255) NULL, model_id VARCHAR(255) NULL, variant VARCHAR(255) NULL,
       created_at DATETIME, FULLTEXT KEY ft_notes (text))
commands(kind ENUM('build','test','run','lint','other') PK, cmdline TEXT,
      last_exit_code INT, last_run_at DATETIME, success_count INT, fail_count INT)
project_state(id CHAR(26) PK, body TEXT, actor VARCHAR(64), actor_raw VARCHAR(255), created_at DATETIME)
project_arch (same shape)
-- reference docs
documents(id CHAR(26) PK, path VARCHAR(1024), title VARCHAR(512), content_hash CHAR(64),
      byte_len BIGINT, source VARCHAR(64), ingested_at DATETIME, UNIQUE KEY uk_doc_path (path))
doc_chunks(id CHAR(26) PK, doc_id CHAR(26) FK CASCADE, ord INT, heading_path VARCHAR(1024),
      body TEXT, UNIQUE KEY uk_chunk (doc_id, ord), FULLTEXT KEY ft_chunks (heading_path, body))
-- plumbing
meta(k VARCHAR(64) PK, v TEXT)          -- schema_version, project_id, created_at
```

Notes:
- FULLTEXT costs on write are real (five internal index tables per FULLTEXT key **[V]**) — acceptable at memory-table row counts; a one-off, non-reproducible timing probe during M0 rig 3 (docs/spikes/m0-rig3.md §6, not part of the committed harness) observed ~1.7× per-insert overhead at a 200-row scale, consistent with but not a precise measurement of this note. **Before, this note said Dolt did not surface those tables to a SQL client; after, the repository's proposal regression has observed their `dolt_<table>_<key>_fts_*` names in `DOLT_DIFF_STAT`, so that claim is removed.** This observation establishes visibility through that table function only; it neither reproduces the count of five nor promises ordinary direct queries against the internal tables, and the five-table figure remains sourced to Dolt's own FULLTEXT blog post (§18). `MATCH ... AGAINST` in a `WHERE` clause also returns one row per matched index term, not one per matching document — a real fan-out property, not a write-cost one, that a naive per-source-type gather query must dedupe before applying `LIMIT` (docs/spikes/m0-rig3.md §4).
- `decisions.summary` deliberately not in the FULLTEXT key (memhub parity — summaries are rerank food, not match targets).
- Git-ingest tables (`commits`/`files`/`commit_files`) are **derived local data** and live in the code-index SQLite, not the versioned repo (each machine's git clone can differ; same §4 principle). This is a placement change from memhub. **[design]**
- Metrics and transcript-pointer tables also stay out of the versioned repo (machine-local; §12 matrix).
- `session_notes` carries nullable session, agent, provider, model, and variant provenance. These are opaque metadata attached to the note, not free-form note text or inputs to actor/source derivation; ordinary notes may omit them, while verified OpenCode wrap-up notes use them (§11.4).
- `facts.evidence` / `decisions.evidence`: a nullable free-form pointer (file path, `file:line`, commit hash, PR number, or URL) an agent or reviewer can attach when proposing or promoting a row. Its purpose is content-based re-verification — checking whether the pointed-at file/commit/PR still says what the fact or decision claims — extending the same `path`+`content_hash` pattern `documents` already uses for ingested reference docs down to individual facts/decisions. **[design]**
- `decisions.alternatives_rejected`: nullable TEXT recording what was considered and passed over. `propose_decision` (§11.1) names it directly so the tool schema prompts agents to fill it in, not just the choice made. **[design]**
- `facts.confidence` is **removed** (memhub carries it). It is asserted once at write time and read by nothing downstream — no query, ranking, or filter consults it. `commands.success_count`/`fail_count` remain the system's only *observed* confidence mechanism (§6.1 `commands` table); an unread, asserted number is vestigial and the review gate (§7) is the trust mechanism this schema actually relies on. **[design]**
- **What `facts` uniqueness guarantees is one *live* row per key, not one row per key.** `uk_fact_live_key` indexes `live_key`, a generated column equal to `key` while `superseded_by IS NULL` and NULL once the row is superseded; NULLs are distinct to a MySQL unique index, so a key accumulates superseded rows without limit while exactly one row under it stays live. The plain `UNIQUE KEY uk_fact_key (key)` this PRD first carried guaranteed something the rest of the document contradicts: measured on Dolt v1.88.1 **[V]** (`docs/spikes/m1-fact-key-uniqueness.md`) it rejected a second row under an existing key on one machine with no merge involved, which forbade supersession itself — §6.2's link-plus-replacement transaction was rejected on its `INSERT` — along with superseded-row scoring (§8.1 step 5 penalizes rows that must therefore still be present) and keep-both (§11.1). Three consequences the client owns **[design]**: the `superseded_by` link is written **before** the replacement row, never after, because the uniqueness check is per statement and a transaction does not relax it; "the current fact under this key" is `WHERE live_key = ?` or `key = ? AND superseded_by IS NULL`, since a bare `key` match returns the whole supersession chain; and no database constraint makes that chain well-formed — two rows may share a superseder, and cycles or dangling ids are application invariants now. `STORED` is correctness machinery: a `VIRTUAL` generated column enforces the same uniqueness but silently makes `MATCH … AGAINST` return nothing on the same table. `KEY idx_fact_key (key)` remains the ordinary lookup index in the planned schema **[design]**, but under the current `ft_facts(value, key)` order both exact and dotted-prefix key filters still work after that B-tree is dropped, so it is not a measured correctness requirement **[V]**.
- Fact keys follow a dotted namespace convention — `build.*`, `convention.*`, `env.*`, `gotcha.*`, and similarly-scoped prefixes — so human skimming and the supported `list_facts`/`recall` filter use a literal dotted prefix. The Dolt v1.88.1 column-order tradeoff is explicit: **before**, with `ft_facts(key, value)`, standalone `value LIKE 'go %'` worked because `value` was non-leading, but the supported `key LIKE 'build.%'` failed with Error 1105 even when `idx_fact_key` existed; **after**, with the current `ft_facts(value, key)`, exact and bound dotted-prefix key filters work over live and superseded facts, while standalone `value = ?` and `value LIKE ?` now fail with Error 1105 because `value` is leading. This deliberately removes the old successful value-prefix filter; fact content search remains `MATCH(value, key)` **[V]** (`docs/spikes/m1-fact-key-uniqueness.md` §§7–8). The supported key-prefix shape is ``WHERE `key` LIKE ?`` with a bound pattern such as `build.%`: one terminal `%`, no leading wildcard or general substring-search promise. The filter leaves §8.1's penalty — not the filter — to demote old facts. Referenced from the server-instructions content in §11.1. **[design]**
- The measured v1.88.1 restriction belongs to the leading column of **every** FULLTEXT key, not to `facts.value` alone: without a separate B-tree, standalone equality on a leading column fails with Error 1105 (`*expression.Equals`), and a standalone `LIKE 'prefix%'` filter on a leading column fails with Error 1105 (`*expression.GreaterThanOrEqual`) even when that column also has a B-tree. Under the current schema this affects `facts.value`, `decisions.title`, `tasks.title`, `session_notes.text`, and `doc_chunks.heading_path`; the default embedded-Dolt regression exercises equality and prefix filters on all five. Non-leading and non-FULLTEXT controls do not inherit that restriction in the measured cases: fact `key` equality/prefix work without `idx_fact_key`, `decisions.rationale` equality works, and `decisions.status` equality works. `MATCH` over each complete FULLTEXT key remains the content-query path **[V]** (`docs/spikes/m1-fact-key-uniqueness.md` §§7–8).

### 6.2 Proposal payloads

A proposal branch's single commit inserts the actual row (fact/decision) with `source='agent:<name>'`, plus one row in a small `proposals` table (`id, kind, rationale, actor, created_at, target ENUM('repo','global')`) carrying review metadata. Accept = merge to `main` (+ optional supersede link executed in the same merge commit); reject = `DELETE` branch. Supersede proposals stage the `superseded_by` update the same way, in §6.1's order: the link first, the replacement row second. Merging such a branch into a `main` whose `facts` table has also moved reports a constraint violation that is already satisfied — §6.3 says what the client does with it. **[design]**

### 6.3 Conflict semantics

Dolt v1.88.1 merges one row's cells three-way and independently: two branches that modify disjoint columns of the same `tasks` row (`status` on one, `title` on the other, with neither writing `updated_at`) merge to a row carrying both edits — exit 0, automatic merge commit, no row in `dolt_conflicts_tasks`, no review opportunity. A conflict is raised per cell whose two modifications disagree, not per row two branches touched (reproduction and captured output: `docs/spikes/m1-conflict-surfaces.md` §5) **[V]**.

Where the merge does fail, v1.88.1 exposes two distinct failure surfaces in the measured M1 cases **[V]**:

- **Data conflicts** expose base/ours/theirs rows in `dolt_conflicts_<table>` and are resolved through `dolt conflicts resolve`.
- **Merge-created constraint violations** expose offending rows in `dolt_constraint_violations_<table>` and are not resolved by `dolt conflicts resolve`, even when that command exits 0.

Expected conflict classes and fail-closed policy **[design]**:

| Conflict | When | Resolution path |
|---|---|---|
| same fact `key`, distinct ULID rows | two machines inserted different live values for one fact key between syncs | constraint violation: show every offending row + blame; operator selects or supplies the durable value; atomically restore the UNIQUE invariant — by superseding the rows not chosen, which keeps them (§6.1), rather than deleting them — and remove only the reviewed violation rows |
| supersede-and-replace, merged | an accepted supersede proposal (§6.2) merged into a `facts` table that has commits on both sides | already-satisfied constraint violation: the merged rows are correct and constraint verification passes, yet both rows are reported and block the commit; re-verify, clear the reviewed records, change no row |
| same-row cell edits, including a fact `value` | two machines edited one row between syncs | data conflict: show base/ours/theirs + blame; operator picks ours/theirs/manual |
| task done-versus-edit | one machine completed a task while another edited the same row, both writing `updated_at` (without that shared cell the two edits auto-merge, above) | data conflict: never choose a whole row by newest `updated_at`; show base/ours/theirs and require an operator-approved final row, preserving `done` unless the operator explicitly reopens it |
| notes / inserts whose only key is a fresh ULID | no same-row conflict by construction | still require both failure surfaces to be empty before commit |
| schema conflicts | impossible in normal operation (agents can't ALTER; migrations converge before pull, §6.4) | refuse + instruct upgrade |

After any nonzero merge, the client stops before commit and queries both per-table surfaces. It refuses unknown or unattributed rows. For a data conflict it writes only the operator-approved row through the conflict-resolution path. For a constraint violation it first runs constraint verification on the affected table, because v1.88.1 also records violations for rows that no longer violate anything — any branch that hands a unique value from one row to another, which is exactly what an accepted supersede proposal does **[V]** (`docs/spikes/m1-fact-key-uniqueness.md`). A violation the table verifies clean against is cleared on that evidence and logged, never raised to the operator as a conflict. Where verification fails, it repairs the underlying rows and removes the matching reviewed entries from `dolt_constraint_violations_<table>` in one transaction; `dolt conflicts resolve` is not used as a substitute. Before concluding the merge, it re-queries both surfaces and runs constraint verification for every affected table. Any remaining row or failed verification keeps the merge blocked. **[design]**

Because the merge is per-cell, which same-row `tasks` races reach that review at all is decided by the write path, not by Dolt: stamping `updated_at` on every task mutation makes every such race a conflict, while writing only the columns a caller changed lets Dolt combine disjoint task edits unreviewed. `tasks` is a direct lane (§3.1), so either is admissible — but the client picks one deliberately rather than leaving it to whichever `UPDATE` a call site happens to emit. **[design]**

### 6.4 Migrations & version skew

- Idempotent runner keyed on `meta.schema_version`, migrations applied on `main` only, each migration = one commit tagged `migration/<n>`. **[design]**
- Client refuses to operate on a clone/hub whose `schema_version` is newer than the binary ("run memdolt upgrade") — memhub's sync guard, generalized. Pull that would fast-forward past a newer migration commit: same refusal.
- Hub upgrade runbook (r2 §7.2 discipline): stop server → `dolt backup` every database → new binary → start → migrations on open → healthz → then clients.

---

## 7. Review & guardrails

The untrusted-writer invariant is memhub's soul; memdolt keeps it whole.

1. Agents propose (branch), humans promote (merge). `review accept` is the only path to durable truth in the reviewed lane.
2. **Global promotion is CLI-only, permanently** (r2 D19, adopted verbatim). `global`-targeted proposals are excluded from MCP/elicitation review; MCP `review_pending` reports them as "N global proposals pending — run `memdolt review` in a terminal."
3. Elicitation-gated review over MCP for **repo-scope** proposals: the go-sdk's multi-round-trip machinery with its legacy `elicitation/create` downgrade shim **[V]** means confirmation dialogs work in today's Claude Code and upgrade transparently when MRTR delivery ships. The r2 D16 token discipline (single-use, short-expiry, server-minted, pending-row-backed `requestState`) applies unchanged — an auto-responding client hook must not be able to forge an approval.
4. Accept-time contradiction probe: port memhub's cross-encoder check (score incoming payload vs existing durable rows; ≥2.0 blocks with `--supersede`/`--force` escapes).
5. Never auto-approve on absent elicitation response. Fail closed on missing actor attribution (`agent:unknown` = untrusted).
6. Doc ingestion stays a direct, user-attributed lane (docs are user-pointed artifacts, not agent claims) with the same path confinement: MCP `doc_add` restricted to repo root ∪ `allowed_dirs`, deny-list on top; CLI unconfined.

---

## 8. Retrieval (hybrid SQL + RAG)

Port memhub's pipeline with the storage swapped underneath. Same models, same fusion, same knobs, same eval harness. Quality parity is a hard gate, not an aspiration.

### 8.1 Pipeline **[design, porting memhub [V] behavior]**

1. **Lexical gather (M0 FULLTEXT-first path; not selected for M2 recall candidate assembly):** per-source-type `MATCH … AGAINST (? IN NATURAL LANGUAGE MODE)`, limit 50/type (after deduping — a `MATCH...AGAINST` filter returns one row per matched index term, not per matching document, so a naive query must `GROUP BY` id before `LIMIT` applies or it silently caps result rows rather than distinct candidates; found and fixed in M0 rig 3, docs/spikes/m0-rig3.md §4). Facts use `MATCH(value, key)` to match `ft_facts(value, key)`; that order is the Dolt v1.88.1 compatibility contract that leaves dotted `key` prefix filters usable (§6.1), not a relevance preference, and means standalone equality/prefix filters on `value` are unsupported. Dolt FULLTEXT is tf-idf-ish, not BM25, natural-language mode only **[V]**. **M0 rig 3 ran this against memhub's golden set (docs/spikes/m0-rig3.md) and reproduced its Recall@3 baseline — but measured, the ~21-row corpus exceeds `rerank_candidate_pool = 20` on every single query, evicting exactly one row each time; which row differed by lexical configuration on 4 of 22 queries, yet the evicted row was never a top-3 target, so FULLTEXT, BM25, and a vector-only control (lexical step removed entirely) all returned identical results. R2's actual lexical-quality question — does the choice matter once the excluded row is one a query wants — was therefore still open after M0.** The M2 scale follow-up below resolves that open question; see risk R2 and its contingency below.
2. **Vector gather (hybrid mode in M0; sole candidate gather in the selected M2 path):** brute-force cosine over the embedding side-store for requested source types. This is architecturally identical to memhub (which brute-forces cosine over BLOB columns — it never used sqlite-vec), so nothing is lost versus baseline.
3. **Stale-embedding detection:** side-store rows missing / content-hash mismatched / wrong byte length → `stale_embeddings` warning with fix hint (`memdolt index rebuild`). Recall stays usable meanwhile.
4. **Hydrate + filter:** staleness (facts only, `verified_at` vs `fact_stale_after_days`), `accepted_only`.
5. **Fusion:** weighted linear blend, memhub's exact formula and defaults — `relevance = 0.5·norm(lexical) + 0.5·cosine`, `score = relevance·age_decay − stale_penalty(0.3) − superseded_penalty(0.4)`, half-life off by default. In the selected M2 path the lexical input is absent, so candidate ordering is cosine-only (the constant `0.5` multiplier does not change that order) before penalties and rerank. Superseded facts are retained and gathered like any other row — §6.1's uniqueness is scoped to live rows precisely so they can be — and this penalty is what demotes them; recall never drops a hit for being superseded.
6. **Scope merge:** repo + global corpora, tagged, one unified pool.
7. **Rerank:** ms-marco-MiniLM-L-6-v2 int8 cross-encoder over top-20 pool, floors `min_rerank_score = 2.0` / `doc_min_rerank_score = 0.0`, truncate to `max_results = 6`.
8. Response shape mirrors memhub's (`results[], warnings[], candidate_count, elapsed_ms, available_docs`), plus a memdolt addition: each hit may carry `last_changed` commit metadata (hash, author, date) from `dolt_blame` when `--provenance` is requested. **[design]**
9. **Empty-recall observability** **[design]**: every `recall` call whose result set is empty above the rerank floor (`min_rerank_score`/`doc_min_rerank_score`, step 7) increments a counter. The dominant long-term failure mode for this system is an agent that never triggers recall at all — a call that runs and comes back empty is the one signal that would be visible instead, and `doctor` surfaces the running empty-recall count/rate as a named check.

**R2 contingency (lexical quality):** if the golden gate shows Dolt FULLTEXT dragging Recall@K below baseline, replace step 1 with in-process BM25 over candidate rows — memory corpora are small enough (10³–10⁴ rows) that full-scan lexical scoring in Go is trivially fast, and it removes the FULLTEXT write penalty as a bonus. Decide on measurement, not vibes. **[design]** — **M0 rig 3 built and measured both (docs/spikes/m0-rig3.md): both reproduce the golden-set baseline (Recall@3 = 100%, 21/21, 0 safety failures), but so does a vector-only control with no lexical gather at all — the golden set's ~21-row corpus never put real pressure on `rerank_candidate_pool = 20`, so M0 did not decide the comparison on lexical-quality measurement, only on which option shipped less code (§19 MD5). The BM25 contingency remained built and validated for the real-scale re-measurement.**

**M2 candidate selection (2026-08-29, issue #87): vector-only gather with the existing 20-candidate rerank pool.** The checked-in rig re-ran the unchanged 22-query set against its deterministic 1,021-row adversarial fixture at candidate pools 20/40/80 (`docs/spikes/m0-rig3.md` §12). Every run had zero safety failures. FULLTEXT improved 18/21 → 19/21 → 20/21 with 3 → 2 → 1 wanted-target evictions and query-loop times 4.135 s → 5.229 s → 7.493 s; it never cleared the gate. BM25 improved 18/21 → 19/21 → 21/21 with 3 → 2 → 0 evictions and times 1.581 s → 2.678 s → 4.848 s; it required the largest measured pool to clear. Vector-only held 21/21 with zero evictions at all three sizes and took 1.557 s → 2.699 s → 4.705 s. Therefore the smallest measured passing strategy is vector-only at 20: it also remains 21/21 with zero safety failures on the original hermetic fixture, reranks one quarter as many candidates as passing BM25-at-80, and was the fastest measured scale query loop. Larger vector pools add cost with no measured quality gain; BM25-at-80 adds an in-process index plus 3.1× query-loop time; measured FULLTEXT pools do not pass. This supersedes FULLTEXT-first for **recall candidate assembly** while preserving the M0 decision and evidence as the explicit before-state in §19 MD5. The FULLTEXT schema and its independently documented SQL behavior are unchanged.

### 8.2 Embeddings are derived and local — NOT in the Dolt repo **[design]**

`embeddings.sqlite` side-store: `(source_type, source_id, model_name) → vector BLOB, content_hash, dimension`. Rationale:

- fp32 blobs in a versioned store defeat prolly-tree structural sharing: every edited fact would append ~1.5KB of history forever (Dolt's own sizing guidance: ≈4KB × update × indexed column **[V]**). Text stays versioned; derived float arrays don't deserve history.
- Pushes/pulls stay small and merges never see vector churn.
- Cost: a fresh clone needs one index build (batch-16 embedding, seconds-to-minutes at memory scale) — the `stale_embeddings` machinery already handles the interim gracefully. Same lifecycle as the code index.
- Dolt vector indexes are explicitly not production-ready (alpha; "too slow for us to recommend using it in production" **[V]**, custom Proximity-Map ANN). When they mature, revisit as an optimization for topology B only.

**M2 side-store handoff (2026-08-29).** Before this change, this section
specified the derived store while production inference stopped at
`internal/embedding.Engine`; the golden rig held vectors in a Go map and no
production rebuild or status lifecycle existed. After it,
`.memdolt/embeddings.sqlite` is the production side-store, backed by
`modernc.org/sqlite`, and `memdolt index rebuild|status` owns its lifecycle.
No prior runtime behavior is removed. The structural blast radius is explicit:

- `internal/layout.Paths.EmbeddingsFile` is the shared path for every
  repository embedding-side-store caller; the existing base, Dolt, lock,
  pidfile, and config paths behave as before. This rule binds that embedding
  path, not every derived store: the code index retains its distinct §5.3 path.
- `store.EmbeddingSource` carries rendered source text, and the concrete
  `localdolt.Store.EmbeddingSources` reads committed `main` `HEAD` only. It
  returns every row in `facts`, `decisions`, `tasks`, and `doc_chunks`, including
  superseded facts/decisions and done/blocked tasks; proposal-branch and
  uncommitted rows remain ineligible. That four-table eligibility belongs to
  this named reader alone, not to every durable table or every future
  `Store` implementation. The `Store` interface, its one-SELECT-or-SHOW
  `Query` boundary, the Dolt schema/migrations, source rows, working set, and
  commit graph all retain their prior behavior.
- `internal/embedding.EmbeddingModelName` exports the same BGE model identity
  `Open` already used; checksum provisioning, tokenizer normalization,
  `Engine.Embed`, reranking, and client-only ONNX placement behave as before.
  `Rebuild` alone writes the SQLite side-store: it inserts missing vectors,
  refreshes only hash/dimension/byte-length-stale rows, skips current rows
  without inference or SQL updates, and removes rows whose source disappeared.
  `Status` never writes or creates the file; it classifies current, missing,
  content-hash-mismatched, wrong-byte-length, and orphaned rows and points to
  `memdolt index rebuild`. The write restriction belongs to `Rebuild`, not to
  every symbol in `internal/embedding`.
- The `embeddings` SQLite table has primary key
  `(source_type, source_id, model_name)` and stores `vector`, `content_hash`,
  and `dimension`; fp32 bytes are little-endian and remain outside Dolt. The
  CLI root help now lists the new `index` command; existing subcommands'
  execution, flags, and JSON payloads behave as before. Lifecycle tests use
  disposable repositories
  and assert exact Dolt source rows, working-set status, and commit hashes
  before and after every rebuild/status call. `modernc.org/sqlite` is the sole
  new direct dependency. Selecting v1.57.0 also raises the existing
  `golang.org/x/text`, `x/crypto`, `x/mod`, `x/net`, `x/sync`, `x/sys`,
  `x/telemetry`, `x/term`, and `x/tools` selections plus
  `github.com/mattn/go-isatty`, and adds modernc's indirect libc/memory/math
  runtime graph and checksums; the tokenizer normalization, networking,
  terminal, and tooling behavior those existing modules supplied still holds,
  as exercised by the unchanged full test, vet, and lint surfaces. Those
  transitive pure-Go dependencies do not introduce a second storage
  implementation. Dolt vector indexes and hub-side inference remain out of
  scope exactly as before.

**M2 recall handoff (2026-08-29, issue #90).** Before this change,
`internal/embedding.Rebuild` alone wrote `embeddings.sqlite`, the shipped CLI
had no recall command, doctor had no empty-recall signal, and the original
golden gate's selected-strategy assertion still consumed the rig's Go map of
vectors even though production indexing existed. After it, production recall
reads committed Dolt text plus current side-store vectors, and the original
fixture's selected assertion traverses that same path. The old FULLTEXT/BM25
rig remains for its historical and scale comparisons; it is no longer the
production assertion. No Dolt schema, source row, working set, or commit-graph
behavior changes. The structural blast radius is:

- `internal/retrieval.Config` and `LoadConfig` read only `[retrieval]`, with
  the established defaults and range checks. This removes the old behavior
  that `[deny_list]` was the sole config table with a reader: before, every
  other table was ignored; after, retrieval reads its own table while
  `denylist.Load` still independently and fail-closed reads only
  `[deny_list]`. The restriction belongs to these two named loaders, not every
  config-reading symbol or every future table.
- `store.RecallSource`, `store.LexicalHit`, and `store.CommitProvenance` are
  read models used by `localdolt.Store.RecallSources`, `RecallFTS`, and
  `LastChanged`. Those concrete readers are committed-`main`-`HEAD` only;
  that restriction belongs to the named LocalStore methods, not to every
  `Store` implementation. Existing `Store.Open`, `Commit`, `Query`, and
  `Close`, review/proposal paths, and deny-list behavior remain unchanged.
  FULLTEXT still gathers at most 50 distinct rows per source type by grouping
  before the limit, and optional provenance comes from each table's
  `dolt_blame` row.
- `internal/retrieval.Recall` adds FTS and hybrid modes. FTS uses the existing
  natural-language FULLTEXT keys. A fully current hybrid corpus uses only
  cosine to order the pre-rerank pool; no lexical weight is silently restored.
  Missing, content-hash-mismatched, and wrong-byte-length vectors warn and
  only matching stale rows may use lexical fallback. Configured fusion,
  fact/done-task age behavior, stale and superseded penalties, accepted/source
  filters, the top-20 cross-encoder pass, score floors, and result truncation
  then apply. Superseded rows remain present and tagged; no row is deleted.
  This behavior belongs to recall candidate assembly, not the separate FTS
  schema contract or M3/M5 retrieval surfaces.
- `internal/embedding.CurrentVectors` is read-only and returns only vectors
  that `Status` classifies current. `RecordRecall` and `ReadObservability` own
  one local `recall_observability` row in the same SQLite file. Thus the prior
  sentence above that "`Rebuild` alone writes the SQLite side-store" has an
  explicit before-and-after: before, it was literally the only writer; after,
  `Rebuild` remains the only writer of `embeddings` vector rows, while
  `RecordRecall` writes only local call counters. `Status`, `CurrentVectors`,
  and `ReadObservability` remain read-only and do not create a missing file.
  None of these restrictions applies to every symbol in `internal/embedding`.
- `memdolt recall` adds human and JSON response rendering for results,
  warnings, candidate/returned counts, elapsed time, available document
  chunks, and optional last-changed provenance. Existing root commands,
  flags, and payloads behave as before. `doctor` adds the named
  `empty-recall-rate` check; its lock, IPC, and schema checks and exit rules
  behave as before, and an absent repository still is not created.
- `tests/golden.TestRetrievalGolden` keeps the original 22-query fixture and
  the FULLTEXT/BM25 evidence, but its selected vector-only 20-candidate
  assertion now uses the production migration schema, side-store, source
  readers, scoring, and reranker. The scale sweep's three-strategy measurement
  remains unchanged. Global-store merging, MCP, document ingestion, search,
  and code indexing remain later-milestone work exactly as before.

### 8.3 Models & inference

- BGE-small-en-v1.5 fp32 ONNX (384-dim, CLS pooling, ~127MB) + ms-marco-MiniLM-L-6-v2 int8 (~22MB) — memhub's exact pair, same pinned upstream revisions.
- Runtime: `yalue/onnxruntime_go` **[V]** (bundled shared libs for Windows AMD64, Linux ARM64, macOS ARM64; version-coupled to onnxruntime 1.26.0; CPU-only). Linux AMD64 bundling resolved by M0 rig 2 (docs/spikes/m0-rig2.md §6): the module bundles no runtime for any platform, so Linux AMD64 needs the same SHA-256-pinned fetch-and-stage treatment as the other three, using Microsoft's official `onnxruntime-linux-x64-<version>.tgz` release asset and `ONNXRUNTIME_SHARED_LIBRARY_PATH`.
- Tokenization is NOT bundled with the runtime — a Go WordPiece/HF-tokenizers implementation is required (both models share the same BERT WordPiece vocab). Tokenizer library resolved by M0 rig 2 (docs/spikes/m0-rig2.md §4, §8): `sugarme/tokenizer`, confirmed to produce byte-identical token ids vs memhub's fastembed on a 31-text probe corpus (both models) — but only when the caller NFD-normalizes input before encoding, compensating for a bug in that library's own `BertNormalizer.StripAccents` (it never Unicode-NFD-decomposes text, so it is a near no-op on precomposed accented Latin characters). Any M1 code adopting this library must apply that compensation.
- Distribution: **first-run fetch** into `~/.memdolt/models/`, every file SHA-256-pinned and verified, offline escape hatch = drop files in place manually. (Unlike memhub's `include_bytes!`: a 150MB `go:embed` would bloat every build and Go tooling handles it poorly.) **[design]**

**M2 production handoff (2026-08-29).** Before M2, the parity and golden rigs
required callers to stage model files and set an ONNX Runtime path; test-only
code verified model hashes, then the harness initialized ONNX itself. After M2,
`internal/embedding.Open` is the one production entry point: it reads the
embedded `models/manifest.json`, selects the committed runtime pin for Windows
AMD64, Linux AMD64, Linux ARM64, or macOS ARM64, verifies every existing or
downloaded model, verifies both the official runtime archive and extracted
shared library, and only then initializes ONNX. A mismatched existing file is
never replaced implicitly; a mismatched download stays temporary and is
removed. `Options.Offline` turns missing files into exact pre-positioning
instructions, while still verifying every file already present. The
`ONNXRUNTIME_SHARED_LIBRARY_PATH` name remains only an optional rig override,
not a production requirement. The caller-side NFD restriction now binds every
text entry point on `embedding.Engine` because all of them route through the
package's `encodeSingle`/`encodePair`; it does not bind arbitrary direct uses
of `sugarme/tokenizer` elsewhere.

### 8.4 Eval harness

Port `eval retrieval` + `eval locate` and the golden JSON format verbatim (substring matchers, `match`/`empty` kinds, Recall@K, K=3). Seed `tests/golden/retrieval_golden.json` from memhub's file. Hermetic fixture runner in CI. **The M0/M2 gate: memdolt Recall@K ≥ memhub's baseline on the same golden set, same fixture data.**

---

## 9. Code index & `locate`

Direct port of memhub's design; storage = local SQLite via `modernc.org/sqlite` (pure Go, no cgo, FTS5 available). Never versioned, never synced, never read by recall.

- File set from `git ls-files -z` through the deny-list; per-file staleness = (mtime,size) fast path then content hash; `HEAD` stamped for reporting only.
- Chunkers: tree-sitter (Go bindings) for the same 7 grammars — rust, c#, java, ts, js, python, go — with memhub's chunking rules (top-level items, `Type::method`, container header-chunks with excised bodies, doc-comment folding, LF normalization); 50-line/4000-byte window fallback.
- Fusion knobs `[code_index]`: fts 0.5 / vector 0.5 / `test_path_penalty` 0.90; reranker off by default (memhub decisions 122/123 carry over); lazy refresh before every query.
- Returns ranked `{path, start_line, end_line, symbol, kind, score, snippet≤6 lines}` — breadcrumbs, never full files.
- Schema-version-mismatch = drop + rebuild (index is regenerable; `upgrade` is a no-op here).
- Git-ingest history tables (`commits/files/commit_files` + `search file:<path>`) live here too (§6.1 note).

---

## 10. Global store

A `global` Dolt database on the hub, cloned to `~/.memdolt/global/` on each machine — **genuinely global from day one**, which memhub only achieves via the r2 hub proposal.

- Same schema as a project DB; scope governed by the active repo's retrieval config (memhub parity).
- Enablement per-repo (`[global] enabled`). Off ⇒ recall byte-identical to repo-only.
- Born-global (`fact add --global`) and promotion (`fact promote <ID> --global` — copy, not move; repo row wins locally) — both **CLI-only** (§7.2).
- Recall merges scopes into one pool, one rerank pass, provenance tags only — never drops a hit for being global.
- Sync: plain push/pull like any project; ULID keys make cross-machine global writes merge-clean.
- memhub's `global_accept_markers` replay machinery is unnecessary — merge idempotency does the job.

---

## 11. Surfaces

### 11.1 MCP server

`modelcontextprotocol/go-sdk` ≥ v1.7.0 **[V]** (2026-07-28 support; MRTR↔legacy elicitation shims both directions). stdio transport, spawned per session, registered through host-specific committed files: Claude's `.mcp.json` and OpenCode V2's `opencode.json`; neither replaces the other.

**Native `2026-07-28` behaviors (r2 §3 absorbed):** per-request `_meta` clientInfo attribution with legacy-`initialize` fallback, fail-closed to `agent:unknown`; `server/discover`; `ttlMs` on `tools/list` (static tool set, long TTL). OpenCode V2's raw MCP client identity `cli` normalizes to canonical `opencode` for actors and rollups, while provenance retains the raw `cli` value.

**Tool surface (parity + replacements):**

| Group | Tools |
|---|---|
| Read | `status`, `recall`, `search`, `locate`, `list_tasks`, `list_decisions`, `list_facts`, `list_proposals` (nee list_pending_writes), `get_command` |
| Direct-lane write | `task_add`, `task_done`, `log_session_note`, `record_command`, `doc_add`, `render` |
| Staged write | `propose_fact`, `propose_decision` (title, rationale, alternatives_rejected, evidence?), `propose_supersede` |
| Review (elicited) | `review_pending` — walks repo-scope proposals via elicitation dialogs (batch approve allowed for repo scope; global excluded per §7.2); fact-key conflict elicitation on `propose_fact` against an existing live key (overwrite/supersede/keep-both/cancel, existing value shown inline — r2 §4.4), where keep-both re-files the new fact under a distinct key collected in the same dialog, since §6.1 admits only one live row per key |
| Repo ops | `repo_status` (ahead/behind/diverged + working-set state), `repo_pull` (merge; conflicts elicited per §6.3 policy), `repo_push`. **No `sync_adopt` — no destructive counterpart exists.** |
| History (new) | `history` — blame/log/AS-OF lookups for a fact/decision ("when did this change and who changed it") or the `project_state`/`project_arch` narrative tables ("how did our status/architecture evolve", §3.2) |
| Gated | `archive_transcript` (confirm=true; unredacted warning — memhub parity) |

Server instructions embed memhub's routing rules (recall-before-ledger, locate-before-grep, turn-1 PROJECT.md, never-write-durable-directly) adapted to memdolt names, plus two memdolt-native additions **[design]**: the fact-key namespace convention (§6.1 — `build.*`, `convention.*`, `env.*`, `gotcha.*`, and similar dotted prefixes) so agents file under an existing prefix instead of inventing ad hoc keys, and the filing rule that decides fact vs. decision — **facts state what is true; decisions record what we chose and why — if there's a "because," it's a decision.**

The server instructions text is itself a versioned, first-class artifact: checked in, and its changes are reviewed as deliberately as a schema migration, not tweaked ad hoc. It encodes the agent's recall-decision policy — recall-before-ledger, when to file a fact vs. a decision, what prefix a new fact key gets — and an undiscussed edit to that policy is exactly as load-bearing as an undiscussed column change. **[design]**

### 11.2 CLI

Cobra; every memhub subcommand maps (full disposition in §12). New/renamed: `memdolt pull|push|repo status` (replaces `sync *`), `memdolt review` (same verbs; diffs rendered from proposal branches), `memdolt history <fact|decision|state|arch> <ident>` (`<ident>` names the fact/decision; `state`/`arch` take none — the narrative table itself is the subject), `memdolt hub init|status` (hub bootstrap + doctor), `memdolt import --from-memhub <export.json>`. Dropped: `sync adopt`, `export`/`import` JSON as the sync path (kept only for interop/migration), `wrapup-policy`-style multi-binary — single binary.

### 11.3 Config

`.memdolt/config.toml` mirrors memhub's structure where semantics survive: `[deny_list]`, `[render]`, `[retrieval]` + `[retrieval.scoring]` (identical knobs/defaults), `[code_index]`, `[doc] allowed_dirs`, `[global]`, `[audit]`, `[wrap_up]`. Replaced: `[sync]` → `[repo] remote_url, topology = "clone" | "live" | "local", auto_pull_on_session_start (bool)`. Machine config `~/.memdolt/config.toml` holds hub defaults + known-projects registry (upgrade enumeration — never a filesystem scan; memhub parity).

### 11.4 OpenCode 2 compatibility supplement (memhub v0.2.2)

This is the narrow v0.2.2 supplement to the v0.2.0 parity baseline, not a claim of general v0.2.1/v0.2.2 parity.

**Registration, doctor, and attribution.** OpenCode uses its native V2 configuration shape in the tracked repo `opencode.json` and, where an operator chooses user registration, in user `opencode.json` or `opencode.jsonc`:

```json
{
  "mcp": {
    "servers": {
      "memdolt": {
        "type": "local",
        "command": ["memdolt", "serve"],
        "disabled": false
      }
    }
  },
  "commands": {
    "wrap-up": {
      "description": "Wrap up this memdolt session",
      "template": "Use the memdolt wrap-up skill. Arguments: $ARGUMENTS"
    }
  },
  "skills": ["templates/skills/opencode"]
}
```

`commands` is the plural command-object map and `skills` is a path array. Doctor recognizes a registration only after parsing a repo or user `opencode.json`/`opencode.jsonc`: native `mcp.servers.memdolt` and officially supported V1 `mcp.memdolt` count; JSONC comments and trailing commas are accepted before a real JSON parse; malformed files and similarly named unsupported paths do not count.

**Wrap-up provenance.** An OpenCode wrap-up takes its current session id only from host context — never a session list, title, active-session heuristic, or filesystem — then independently verifies it with `opencode2 api get "/api/session/<id>"`. It parses `data` as `Session.Info` and requires the returned `data.id` to exactly equal the host-supplied id. An unavailable id, failed API call, malformed response, missing returned id, or mismatch stops before every durable memory write, render, or sync. On success the session note stores `session_id` plus nullable `data.agent` (`agent_id`) and, when present, `data.model.providerID` (`provider_id`), `data.model.id` (`model_id`), and `data.model.variant` (`variant`); they never enter free-form text or derive actor/source values.

**Transcript archive.** Transcript mode requires a separate explicit approval that warns the archive is unredacted. It reuses the already verified current id and never discovers or guesses another one. Before any process invocation, validate an OpenCode id as `ses` followed by a nonempty ASCII alphanumeric, underscore, or hyphen suffix. Invoke `opencode2 api v2.session.export --param sessionID=<id> --param sanitize=false` through argument-based process APIs (on Windows, fall back to `opencode2.cmd` only when the bare program is unavailable). Before opening the project or writing, require a JSON object `data`, object `data.info`, exact `data.info.id`, and array `data.messages`. Archive the exact complete, unsanitized response bytes as `.json.zst` and retain one replaceable local pointer per session. Archive and pointer data are excluded from recall, embeddings, and every export; `[wrap_up].transcript_retention_days` governs retention, and expiry removes the local archive and its pointer.

**Skills and upgrade.** Hibernated `metrics` and `viz` may remain in source for feature builds but are absent from normal OpenCode `skills` and command discovery. Additive wrapper sync never creates an absent agent directory, overwrites an unowned file, or automatically deletes stale or hibernated wrappers. It reports stale or unowned wrappers as actionable orphans for manual review/removal and leaves user files unchanged.

---

## 12. Feature parity matrix (memhub v0.2.0 baseline + v0.2.2 OpenCode 2 supplement → memdolt)

Every ordinary row uses v0.2.0 as its baseline. Rows explicitly labeled v0.2.2 are the narrow OpenCode 2 supplement only, not a claim that all intervening memhub behavior was audited.

| memhub feature | memdolt disposition |
|---|---|
| facts/decisions/tasks/notes/commands/narratives CRUD | port (§6) |
| pending_writes + review accept/reject/expire/stale | proposal branches + review verbs; expiry = branch age sweep; `review stale` lifecycle audit ports as-is |
| contradiction probe on accept | port (§7.4) |
| supersede links + penalties | port (columns + scoring) |
| writes_log audit | replaced by commit graph (§3.2) |
| hybrid recall + warnings + scope merge | port (§8), quality-gated |
| eval retrieval/locate + golden sets | port verbatim (§8.4) |
| code index + locate (7 grammars) | port (§9) |
| doc ingestion (heading chunker, hash no-op, auto-flip include_docs) | port (§6.1, §8) |
| global store + promotion | port, genuinely-global via hub (§10) |
| sync enable/status/snapshot/check/commit/adopt + five verdicts + manifest/digest | **replaced** by push/pull/merge (§3.2); `check --diff` becomes `repo status --diff` over `dolt_diff` |
| render PROJECT.md / PROJECT_LEDGER.md (atomic two-phase write) | port; ledger's "Recent activity" sourced from `dolt_log` |
| doctor (19 checks) | port + memdolt-specific checks (LOCK/pidfile/IPC §5.2, remote reachability, schema skew, model presence, empty-recall rate §8.1) |
| audit md (CLAUDE.md/AGENTS.md linter) | port (pure text tool) |
| export/import JSON v1 | import kept (migration §15); export kept for interop; neither is the sync path |
| ingest-git + `search file:` | port into code-index store (§6.1 note) |
| upgrade (multi-instance registry, skill resync, install manifest, Windows self-replace) | port; Go single-binary + no 250MB embed makes Windows self-replace simpler; skill wrappers for 3 agent CLIs from templates |
| gc (target/ artifacts) | replaced by `memdolt gc` = Dolt-focused: `dolt gc` scheduling, old-notes retention sweep + periodic `gc --full` (§13.3), model-cache pruning |
| token accounting (recall proxy, session scraper, tiktoken, calibrate) | port behind config gate, M6 — off by default |
| viz dashboard | port behind build tag, post-v1 backlog |
| session transcripts archive (zstd, fail-closed) | port (klauspost/compress zstd), M6 |
| wrapup-policy text renderer | port (one source of truth for 3 skill flavors) |
| skills (14 × 3 CLIs) | port the memhub-analogous set; catch-up skill becomes trivial (`pull`) |
| OpenCode V2 registration, doctor, and raw `cli` identity (v0.2.2 supplement) | native config, parsed repo/user JSON or JSONC recognition, and canonical `opencode` attribution (§11.4) |
| OpenCode session-note provenance and memhub export-v1 import (v0.2.2 supplement) | port nullable verified session metadata without changing note text or actor/source semantics (§6.1, §11.4, §15) |
| OpenCode complete unredacted transcript export (v0.2.2 supplement) | port the separately approved, validated, local-only `.json.zst` archive; never recall, embed, or export it (§11.4) |
| OpenCode hibernated discovery and wrapper resync (v0.2.2 supplement) | keep hibernated surfaces undiscovered; report-only actionable orphans, never overwrite or delete user files (§11.4) |

---

## 13. Hub deployment & operations

### 13.1 The hub is one process

`dolt sql-server` with `remotesapi` enabled **[V]**:

```yaml
# /etc/memdolt-hub/config.yaml
listener: { host: "100.x.y.z", port: 3306 }   # tailnet IP — never 0.0.0.0
remotesapi: { port: 50051 }
data_dir: /mnt/ssd/memdolt-hub
```

- Clone/fetch/pull AND push through remotesapi are supported (Dolt ≥ v1.30 both sides **[V]**). Pushes must be fast-forward unless forced **[V]** — memdolt clients always merge locally then push, so this is the natural flow.
- Auth = SQL users/grants (`clone_admin` read; push requires elevated grants **[V]**; DoltHub itself calls this auth weak **[V]**) → **the tailnet is the perimeter** (r2 §6.2-6.3 posture: bind tailnet IP, systemd system unit under a dedicated user, `After=tailscaled`, bind-retry for the boot race). Non-Tailscale self-hosters: private network or SSH tunnel; never expose remotesapi publicly. **[design]**
- Hardware: Pi 5 (8GB) comfortably exceeds Dolt's 2GB production minimum **[V]**; the hub does no inference (§4.1), so r2's Q3 concern does not exist here. Linux desktop per r2 §13.5 checklist (mask suspend, system unit, tailscaled at boot) equally fine. ARM64 Linux release binaries: **[L]** — confirm the asset on github.com/dolthub/dolt/releases during M0. `dolt version` pin: hub and clients within a documented compatible range; `doctor` checks skew.
- SSD-primary storage recommended (r2 D8 carried over, downgraded from requirement to recommendation): live databases on USB SSD for endurance and random-write performance under commit churn. This is a documented recommendation, not a requirement — memdolt must not refuse to run without an SSD.

### 13.2 Backups (r2 §12, mostly dissolved, residue kept)

Every client clone is a full-history replica — the 3-2-1 baseline exists by construction. Residual hub-side apparatus **[design]**:
- Nightly `dolt backup sync-url` per database to a second local target (SD card or second disk) — `dolt backup` captures working set + all refs, more than push does **[V]**.
- Optional third leg: `rclone` the backup dir to Drive (crypt per r2 D11 remains operator's call).
- `doctor --hub`: last-backup age, disk headroom, per-database size trend. GFS retention is **not** ported — commit history already provides point-in-time recovery; backup rotation is simple age-based pruning.

### 13.3 History growth & retention

Dolt storage grows with history (~4KB/update-transaction/indexed-column rule of thumb **[V]**); at memory-scale write volume this is years of headroom, but notes need policy:
- Retention sweep deletes `session_notes` older than `transcript_retention_days`-style config; deleted rows persist in history until GC.
- Transcript archives are unredacted, machine-local, and governed by `[wrap_up].transcript_retention_days`: expiry removes the archive and its one-per-session pointer without making either retrievable or exportable (§11.4).
- `memdolt gc --deep` runs Dolt's full collection (blocks writes; scheduled, never in a request path). Exact `dolt gc --full`/`--shallow` semantics **[L/verify]** — pin during M0.
- Hard-forget (a secret accidentally committed) = history rewrite, documented as the exceptional, manual, git-filter-branch-class operation it is. PRD stance: keep secrets out via deny-list (port memhub's) rather than promising deletion. **[design]**

### 13.4 Format invariance (r2 D13, adapted)

Topology is a routing decision, never a data-format decision: the same Dolt database must serve topology A, B, and C without conversion — no hub-only tables, no host-embedded paths, `project_id` stays git-remote-derived. The §16 round-trip gate enforces it.

---

## 14. Tech stack (normative for a new agent)

| Concern | Choice | Notes |
|---|---|---|
| Language | **Go**, latest stable; module `go` directive **1.26.2**, modules | Not a preference: `github.com/dolthub/driver` v1.88.1's own `go.mod` forces this floor, so the "≥1.24" this table first carried was never achievable **[V]** (M0). gofmt + golangci-lint gate in CI; idiomatic Go, no framework soup |
| Storage | Dolt via `github.com/dolthub/driver` (embedded) + `database/sql` MySQL driver (topology B) | Apache-2.0 **[V]** |
| Build settings (embedded driver) | **`CGO_ENABLED=1`** and a C compiler on every build host, **plus the `gms_pure_go` build tag** | Both are required, not tuning **[V]** (M0). `dolt/go/store/nbs` imports `github.com/dolthub/gozstd` unconditionally, so there is no pure-Go build. Without the tag, `go-mysql-server/internal/regex` needs cgo *and* system ICU development headers; with `CGO_ENABLED=0` neither implementation file is selected and the build fails outright. The tag swaps ICU-backed `REGEXP`/`RLIKE` for Go's `regexp` (RE2) — a real semantic change in a corner of the SQL dialect, accepted because M0–M2 use neither **[design]**. A command-line `-tags` replaces the one in `GOFLAGS` rather than adding to it, so any invocation that passes its own tags must repeat `gms_pure_go` **[V]** (M0) |
| MCP | `github.com/modelcontextprotocol/go-sdk` ≥ v1.7.0 | 2026-07-28 + elicitation shim **[V]** |
| CLI | `spf13/cobra` | `--json` on everything, memhub convention |
| Inference | `yalue/onnxruntime_go` (pinned to its supported onnxruntime version) + Go WordPiece tokenizer (`sugarme/tokenizer`, confirmed byte-identical to memhub's fastembed with an NFD-normalization compensation the caller must apply — M0 rig 2, docs/spikes/m0-rig2.md §4) | CPU-only by design |
| Derived stores | `modernc.org/sqlite` (pure Go, FTS5) | code index + embedding side-store |
| Chunking | tree-sitter Go bindings, 7 grammars | port memhub chunker rules |
| Compression | `klauspost/compress` (zstd) | transcript archives |
| IDs | ULID (`oklog/ulid`) | merge-safe PKs |
| CI | GitHub Actions: lint, test (linux/windows/macos), ARM64 cross-build lane | branch protection once green; model files cached, never committed. The ARM64 lane needs a **cross C toolchain**, not just `GOARCH=arm64`: cgo is mandatory (row above), so a cgo-less cross-build is not available **[V]** (M0) |
| License | Apache-2.0 | matches Dolt |

**Repo layout:**

```
memdolt/
  cmd/memdolt/            main
  internal/layout/        per-repository path resolution: .memdolt/ and everything under it (§5.3)
  internal/singleowner/   the advisory lock files behind the single-owner rule (§5.2)
  internal/store/         Store interface; localdolt/, remotesql/
  internal/storeipc/      store operations carried over the owner's loopback endpoint (§5.2.1)
  internal/schema/        DDL + migration runner
  internal/review/        proposal branches, accept/reject, contradiction probe
  internal/retrieval/     gather, fuse, rerank, eval
  internal/embedding/     onnx session mgmt, tokenizer, side-store
  internal/codeindex/     walker, chunkers, locate
  internal/mcpserver/     tools, elicitation flows, instructions
  internal/render/  internal/docs/  internal/hub/  internal/cli/
  models/manifest.json    SHA-256 pins + upstream URLs (no binaries in git)
  tests/golden/           retrieval_golden.json, code_locate_golden.json (ported)
  tests/soak/             §16 rig-1 concurrency soak, behind the `soak` build tag
  docs/prd/memdolt-prd.md this document, checked in verbatim as product authority
  docs/spikes/            M0 rig findings, one file per rig
```

**Project conventions for agents** (seed CLAUDE.md from these): PRD is authority, don't silently diverge; agents are untrusted writers — the review gate is non-negotiable; fail loudly; no scope creep beyond the parity matrix; feature-branch + PR always; flag new deps before adding.

---

## 15. Migration from memhub (one-way, optional)

`memdolt import --from-memhub <export.json>`: consume memhub's export v1 JSON (facts, decisions, tasks, commands, session_notes while preserving nullable `session_id`, `agent_id`, `provider_id`, `model_id`, and `variant`, project_state/arch, pending_writes → recreated as proposal branches; writes_log → imported as a single annotated genesis note, not fake history). Docs re-ingested from source files (export excludes them by design). Embeddings and code index rebuild locally. Follow with `eval retrieval` against the ported golden set before trusting recall. The operator's own migration, if ever, follows the r2 §13.1 discipline: converge memhub first, lowest-stakes project first, one week soak, quarantine (`.memhub` renamed, not deleted), old state retained a month.

---

## 16. Milestones & gates

| M | Scope | Exit gate |
|---|---|---|
| **M0 — Spike (go/no-go)** | Embedded-driver soak: MCP-server-owns-store + CLI-routes-through-IPC under concurrent load, incl. unclean-kill/stale-LOCK recovery. ONNX-in-Go: embeddings + rerank scores match memhub within tolerance on a probe corpus (tokenizer ids byte-identical). Retrieval rig: golden set on Dolt FULLTEXT + brute-force cosine. Hub rig: sql-server + remotesapi on the Pi or Linux box, clone/pull/push/merge round trip over Tailscale from two machines. Resolve every **[verify]**: ARM64 release asset, gc flag semantics, shallow clones, Linux-AMD64 onnxruntime bundling, tokenizer lib, current vector-index status. | Recall@K ≥ memhub baseline (or the BM25 contingency proves it); zero data-loss events in the concurrency soak; push/pull round trip clean. **Fail → project stops; write up findings.** **Decided 2026-08-02: GO** — all three conditions met, each in a narrower scope than its wording suggests, and two of the scope column's six `[verify]` items (gc flag semantics, shallow clones) did not complete. Condition-by-condition evidence, the limits the GO carries, and the obligations it leaves open: `docs/spikes/m0-gate.md` |
| **M1 — Core** | init, schema+migrations, CRUD all lanes, proposal branches, review CLI (accept/reject/expire/stale, contradiction probe), commit conventions, doctor basics, deny-list. | Lifecycle test suite green; a full propose→review→merge cycle audited via `dolt_log`. |
| **M2 — Retrieval** | Embedding side-store, vector-only recall candidate gather + warnings, rerank, eval harness, staleness machinery, `search`; FULLTEXT remains available to the text-search surface. | Golden gate green in CI (hermetic fixture). |
| **M3 — MCP** | Full tool surface, 2026-07-28 behaviors, elicitation review loop + fact-key conflict flow, server instructions, `.mcp.json`, native OpenCode V2 registration/doctor/identity, verified OpenCode wrap-up provenance, skills for 3 agent CLIs. | End-to-end session from real Claude Code: recall, propose, elicited review, task ops. |
| **M4 — Hub & repo ops** | remotes config, pull/push/repo-status + conflict elicitation, hub init/systemd docs, auth setup, version-skew guards, topology config; `Store` remote impl (topology B) if time allows. | Two-machine round-trip acceptance test (r2 §13.6 analogue): fixture data, write from both machines, merge, verify counts/hashes; re-open with plain local memdolt — no conversion (D13 gate). |
| **M5 — Parity long tail** | docs ingestion, code index + locate + eval, render, global store, import-from-memhub including session-note provenance, audit md, ingest-git. | Parity matrix (§12) fully dispositioned; locate golden gate green. |
| **M6 — Ops polish** | backups + doctor --hub, gc/retention, upgrade machinery with safe wrapper resync, token accounting (gated), validated unredacted OpenCode transcripts, README/status discipline. | Quarterly-drill-style restore test documented and executed once. |

---

## 17. Risk register

| # | Risk | Sev | Mitigation |
|---|---|---|---|
| R1 | Embedded-driver cross-process access. **Measured in M0 [V]:** a second OS process is not refused — it opens, pings and reads normally, having been **silently downgraded to read-only**, and fails only at its first write with `cannot update manifest: database is read only`. Materially worse than the "database is locked" this register first anticipated: no health check detects it, and a writer believes it is writing right up to its first commit. Stale LOCK files survive unclean shutdown | **High** | Single-owner rule + CLI→IPC routing (§5.2), enforced by memdolt's own advisory lock, taken before the driver touches the data dir: a second **memdolt** process is refused in ~0.1 s with a distinct error rather than downgraded. Residual and not removable by that lock: it binds only processes that take it, so a foreign `dolt` CLI or `dolt sql-server` on the same directory still gets the silent downgrade — `doctor` must report it and the docs must forbid it. M0 rig 1 was the gate; see `docs/spikes/m0-rig1.md` |
| R2 | Dolt FULLTEXT (tf-idf, NL-mode-only) drags Recall@K below baseline. **Before — M0 rig 3 [V]:** Recall@3 = 100% (21/21), 0 safety failures, matching BM25 on the same 22-query golden set, but the ~21-row corpus could not discriminate either from vector-only; the real-scale question remained open. **After — M2 scale sweep [V], 2026-08-29:** at 1,021 adversarial rows and pool 20, FULLTEXT and BM25 each fell to 18/21 through three wanted-target evictions while vector-only stayed 21/21 with none. Pools 40/80 brought FULLTEXT only to 19/21 and 20/21; BM25 reached 21/21 at 80; every configuration had zero safety failures. | High | Resolved for M2 recall candidate assembly: vector-only gather with the existing pool 20 is the smallest measured passing strategy and the fastest scale query loop (1.557 s versus 4.848 s for passing BM25-at-80). Keep the unchanged hermetic gate and the selected-strategy assertion at scale; retain FULLTEXT for text search rather than fusing it into recall candidates. See `docs/spikes/m0-rig3.md` §12. |
| R3 | Go tokenizer mismatch vs fastembed → silently different embeddings | High | M0 byte-identical token-id check; probe-corpus score comparison |
| R4 | History growth from high-churn rows. **Measured in M0 [V]:** a single-row insert into a five-column table costs ~16.8 KB of history (801.6 MB for 47,835 commits) — roughly 4× §13.3's ~4 KB rule of thumb, which is therefore optimistic for small commits | Med | Embeddings out of repo (§8.2); note batching (§3.1) is load-bearing, not a nicety; retention + `gc --deep` (§13.3). See `docs/spikes/m0-rig1.md` |
| R5 | remotesapi auth is weak **[V]** | Med | Tailnet-perimeter posture; docs forbid public exposure; SQL grants as second layer |
| R6 | Dolt/driver version coupling (client↔hub skew, onnxruntime pin) | Med | doctor skew checks; documented compatible ranges; renovate-style dep discipline |
| R7 | Second-system scope creep | Med | §12 matrix is the contract; PRD non-goals enforced in review |
| R8 | Write-path throughput. **Measured in M0 [V]:** ~300 commits/s on an empty store, unchanged from 6 to 32 concurrent writers, falling to ~126/s once history reaches 48k commits / 800 MB — the ceiling is a function of history size, not of concurrency. The ~4-branch plateau remains **[L]**: this rig used one branch | Low | Memory-scale write volume is orders below this — a whole 10³–10⁴-row corpus is about thirty seconds of it; note-batching helps, and §13.3's retention sweep now has a throughput justification as well as a disk one. Re-measure for topology B. See `docs/spikes/m0-rig1.md` |
| R9 | Writes routed over the single-owner IPC endpoint are **at-least-once**: an owner that dies after committing and before answering leaves its caller unable to tell whether the write landed. **Measured in M0 [V]** — one such write per unclean-kill run was in the store while its caller had been told nothing | Med | ULID primary keys (§6.1) make a retry idempotent by construction, so M1's IPC write path must carry client-minted ids and must not replace them with server-minted ones; a caller whose answer was lost re-reads rather than re-writes. See `docs/spikes/m0-rig1.md` |

---

## 18. Key sources

Dolt: embedded driver (github.com/dolthub/driver; dolthub.com/blog/2022-07-25-embedded), remotesapi push (blog/2023-12-29-sql-server-push-support), remotes docs (docs.dolthub.com/sql-reference/version-control/remotes), vector indexes (blog/2025-01-16, blog/2025-06-23 deep-dive, blog/2025-09-03), FULLTEXT (blog/2023-08-14), system tables & merges (docs.dolthub.com/sql-reference/version-control/dolt-system-tables, /merges), conflicts (blog/2026-03-23-programmatic-conflict-resolution), sizing (blog/2023-12-06), perf (blog/2025-12-12), concurrency (blog/2021-03-12), backups (blog/2021-10-08). Driver field reports: steveyegge/beads#1719, gastownhall/beads#1925, #1401. MCP: blog.modelcontextprotocol.io/posts/sdk-betas-2026-07-28, github.com/modelcontextprotocol/go-sdk (docs/protocol.md). ONNX: github.com/yalue/onnxruntime_go. memhub: local repo v0.2.0 (broad parity inventory, 2026-07-29); narrow v0.2.2 OpenCode 2 supplement: https://github.com/kninetimmy/memhub/releases/tag/v0.2.2; r2 spec: memhub-mcp-implementation-spec-r2.md.

---

## 19. Decisions register (proposed; log outcomes as the project decides them)

| ID | Decision | Status |
|---|---|---|
| MD1 | Go + embedded Dolt driver; single-owner process model with CLI→IPC routing | Proposed (M0 validates) |
| MD2 | One branch per proposal (not per session) | Proposed |
| MD3 | Embeddings/code index derived + machine-local; never in the versioned repo | Proposed — load-bearing for history size |
| MD4 | Brute-force cosine; no Dolt vector indexes in v1 | Proposed (revisit on Dolt maturity) |
| MD5 | Dolt FULLTEXT first; in-process BM25 contingency on golden-gate failure | **Before — decided at M0 rig 3 (`docs/spikes/m0-rig3.md` §§1–10):** Dolt FULLTEXT on PRD-default + less-shipped-code grounds, not a recall-quality comparison. The hermetic gate passed (Recall@3 = 100%, 21/21, 0 safety failures) but could not discriminate FULLTEXT from BM25 or vector-only at ~21 rows, so the contingency stayed in reserve for a real-scale re-measurement. **After — superseded for M2 recall candidate assembly by the 1,021-row sweep (`docs/spikes/m0-rig3.md` §12):** vector-only gather with `rerank_candidate_pool = 20` is selected. It produced 21/21, zero safety failures, and zero wanted-target evictions on both fixtures; BM25 required pool 80 (4.848 s query loop versus 1.557 s), FULLTEXT still missed at pool 80, and larger vector pools added cost without quality. FULLTEXT remains for text search; the original decision and evidence remain recorded here as the before-state. |
| MD6 | Hub = dolt sql-server + remotesapi, tailnet-perimeter auth | Proposed |
| MD7 | Topology per-project: clone (default) / live / local; no destructive sync op exists anywhere | Proposed |
| MD8 | Global promotion CLI-only, permanently (r2 D19 adopted) | Adopted from r2 (operator-accepted there) |
| MD9 | Format invariance across topologies (r2 D13 adopted); round-trip test gates M4 | Proposed |
| MD10 | Models fetched at first run, SHA-256-pinned, not embedded in the binary | **Implemented (M2): `models/manifest.json` pins immutable model revisions and official ONNX Runtime 1.26.0 assets for all four client platforms; `internal/embedding.Open` verifies cache/download bytes before ONNX initialization and supports verified offline pre-positioning** |
| MD11 | ULID PKs on all agent-writable tables | Proposed |
| MD12 | Direct lanes (tasks/notes/commands/narratives/docs) commit to main without review; notes batched | Proposed |
| MD13 | Ambient recall: inject `recall` results automatically via agent-CLI prompt-submit hooks, config-gated | Backlog — post-v1 |
| MD14 | No pinned/importance flag on facts | Deliberate non-decision (rejected) — revisit only on eval evidence of critical facts losing rerank races |
| MD15 | `facts` uniqueness is scoped to live rows — one live fact per key, superseded rows unbounded — through a unique index over a `STORED` generated column | **Decided (M1 spike, docs/spikes/m1-fact-key-uniqueness.md): a plain `UNIQUE (key)` forbids supersession, superseded-row scoring and keep-both, all three of which this document describes; dropping uniqueness altogether would instead silence the cross-machine same-key race that §6.3 relies on surfacing as a constraint violation** |
