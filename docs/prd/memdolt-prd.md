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
- **Direct lanes** (no review gate, matching memhub's semantics): `tasks`, `session_notes`, `commands`, `project_state`/`project_arch` narratives, doc ingestion. **Before issue #104, this paragraph said:** “These commit straight to `main`, **batched**: one commit per MCP tool call for tasks/narratives; notes accumulate in the working set and commit on session end or a 5-minute timer, whichever first (Dolt commits are prolly-tree updates — materially heavier than a SQLite INSERT — so note-per-commit is prohibited).” **After issue #104:** direct writes still commit to `main`, but only the long-lived MCP `log_session_note` path batches notes. It keeps prepared rows in `mcpserver.Toolset` process memory, grouped by actor, until the five-minute deadline or orderly session end; the Dolt working set is not the queue. Each successful actor group becomes one clean-working-set-guarded commit and is removed immediately, while a group that fails at the deadline remains in memory for one orderly-shutdown retry without blocking other groups. A final shutdown failure is returned and that failed group is discarded when the toolset closes; a crash or forced termination likewise loses every not-yet-committed in-memory note. The short-lived CLI note lane remains one note per commit. These batching and loss boundaries bind the MCP `Toolset` accumulator alone, not every `session_notes` writer or every direct lane. **[design]**
- Commit metadata is load-bearing: author = normalized actor (`agent:claude-code`, `agent:codex`, `user`), message = structured one-liner (`propose fact msrv=1.24`, `note batch (3)`, `review accept #42`). `dolt_log` + `dolt_blame` then answer provenance queries with zero app code. **[V]** (system tables exist as documented.)
- Agents never `ALTER TABLE`. Schema changes ship only in migrations run by the binary on `main` (§6.4). This keeps Dolt schema-conflict machinery **[V]** out of the daily path entirely.

**Confirmed outcomes (issue #145).** Before this delivery, the #104 accumulator
above retained every failed group for retry, while #139's native hash correction
had not reached the older direct-lane/document/raw-owner consumers. After it,
task add/done/block, notes/batches, command recording, narratives and document
add/remove retain confirmed identity/hash with late native, read-back, config,
close or output errors. A nonempty hash confirms durability; an unobserved native
result or owner reply means unknown outcome, not rollback. Inspect the named row
and Dolt history before any retry. Successful output fields remain compatible;
CLI JSON adds an optional error. Failed command read-back returns only the certain
kind plus hash/error; zero-value command totals in that failed result are unknown.
Pre-commit validation, deny/config, clean-guard and statement refusals claim no
durable effect and preserve their prior boundaries.

`Toolset.flushLocked` attempts every actor group. It removes confirmed groups
immediately even with a late error and reports confirmed hashes if any group
fails. Only known uncommitted groups retain the orderly-shutdown retry; explicit
render may also retry them. Unknown groups remain inspection-only and are never
submitted again at timer, render or close; end that session and inspect the named
notes before starting a fresh one. Unknown groups prevent session publication.
New notes still refuse after a flush error. Timer failures are shown by the next
render and retained for shutdown; errors from an explicit render are returned
there and also retained for shutdown. A later render may succeed once eligible
groups commit, without repeating any already-confirmed group. `Close` retains
earlier/final failures, makes its existing bounded final attempt on eligible
groups and discards every remaining group, including unknown groups. The existing
five-minute timer, one-minute flush/close limit and crash-loss boundary remain.
These rules bind Toolset's process-local accumulator alone, not all note writers.

`Toolset.Render` holds the queue mutex through flush and the committed-main
snapshot/file operation. A flush error produces `notes-failed` without publishing
files, preserving committed effects and the remaining groups. Stale timer callbacks
cannot consume a new timer or replay a flushed group. The production `runServe`
owner installs this same render callback on authenticated IPC before listening;
MCP render and live-owner CLI render therefore flush the same queue. Standalone
`Store.Render` has no session queue and retains its existing read/file behavior.
Proposal exclusion, per-file publication, actor attribution, deny-list checks,
clean-working-set guards, serialization and global exclusion remain unchanged.
No new batching service, schema, dependency or full M5/M6 claim is introduced.

Before #145's review-cycle correction, successful flushes discarded their note
IDs/hashes before calling Store.Render. A later config/snapshot/file error could
report only file effects although notes committed. After the correction, the
existing flush loop returns those effects and Toolset.Render retains them in
optional `render.Result.NoteCommits` (`noteCommits`: note ID to commit hash).
The map describes this invocation's confirmed flush only; it is independent of
SourceCommit, which stays empty if snapshot capture fails. `NoteCommitError`
adds note/history inspection evidence through later render, CLI close and output
failures. Existing MCP/owner result envelopes carry the map without resubmission;
a lost reply names possible note effects but claims no identities or hashes.
Timer/shutdown errors reuse the same formatter. Queue policy, pure Store.Render,
per-file publication and every prior guard remain unchanged. The AGENTS #145
record inventories the reached methods, workflow updates and real-store tests.

The complete symbol/file consumer inventory is in AGENTS.md's #145 record:
`Store.Commit`/`commitConnFinalize`/`commitTx`/`nativeCommitResult`, every direct
`Lanes.write` and `CommitNotes` caller, document mutations, existing human/interop/
proposal/migration consumers, raw and typed owner handlers/clients, CLI direct/
OpenCode/document/render paths, Toolset handlers/queue/render and `runServe`,
soak result classifiers, matching tests and host workflows. In particular,
ordinary failed staging retains its earlier residue/cleanup rules; a populated
late hash does not authorize branch deletion. Migration/tag recovery still needs
inspection and `memdolt init` rather than a claim that a late commit rolled back.

Before issue #139, those direct lanes and reviewed proposals were the shipped
writers; ordinary trusted human fact/decision commands remained deferred. After
it, the repository CLI in §11.2 also lets humans assert, verify, edit summaries
and link supersessions directly, one guarded human commit per actual change.
The reviewed agent lane and its human promotion gate remain unchanged. These
human commands are absent from MCP; a source label or SQL username cannot
convert an agent into a trusted human.

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

**Completed for the shipped CLI surface in M3:** before this routing pass,
the authenticated endpoint carried only the M0 `Commit`/`Query` pair and
`doctor` assembled its schema read from those raw queries. The CLI still
constructed `LocalStore` for task/note/command/narrative, review,
embedding-source, recall/evaluation and search operations, and proposal methods
had no routed counterpart; the live owner's advisory lock therefore refused
those CLI calls. After this pass, all use one owner-client store surface:
the owner executes the same memory, proposal, review, retrieval, search and
schema methods as direct mode, while the CLI retains rendering and the local
derived embedding side-store. The raw M0 pair remains for the concurrency soak
and for `internal/memory`'s shared statements; its read-only restriction is a
`Store.Query` restriction and therefore binds every implementation, not only
the IPC route. The typed operation allow-list is narrower: it binds that route
alone, so adding another method to a store does not expose it without naming it
on both handler and client.

Before issue #133, retaining rendering in the CLI above meant formatting
command results, and generated-memory render was deferred. After it, result
formatting still belongs to each surface, while `Store.Render` performs the
complete snapshot/file operation in the owning process. CLI and MCP call that
same method once; authenticated IPC carries the operation and confirmed
result-plus-error, with no caller-selected destination. Unknown replies require
inspection before retry. Its committed-read and file boundaries are detailed
in §11.2; the existing query and owner contracts above remain unchanged.
Before #145 these entry points had no note flush. After it, `runServe` supplies
the optional session render callback to the existing owner operation, while
owners without a queue default to Store.Render. Submission remains single-shot.

Review acceptance crosses that typed route as the complete application gate,
not as a raw storage merge. The first version of this routing pass called the
old `localdolt.AcceptProposal` signature directly, carried no `--force` intent,
and therefore omitted the contradiction guard that landed concurrently on
`main`. After integration, `ReviewAccept` carries the proposal id, reviewer and
force bit in one request; the owner handler requires a `ReviewAcceptFunc`, whose
production binding is `internal/review.Accept`, and that function constructs
`AcceptOptions` and enters `localdolt.Store.AcceptProposal` on the owning
process. Thus configuration validation and model use stay with the owner. At
that delivery, `acceptMu` serialized direct and routed accepts on the same
Store; issue #106's before-and-after below replaces that accept-only boundary
with the proposal-mutation boundary. The contradiction probe and validated
supersede-shape bypass remain in force, and the existing one-commit, deny-list,
conflict, constraint, attribution, merge and branch-deletion behavior still
holds. This restriction binds the named
`ReviewAccept` route alone: list, show, reject, expire and stale retain their
existing routed methods, and neither model callbacks nor a client-selected
configuration path cross IPC. The complete storage symbols and their scopes
remain enumerated in §7's contradiction-guard handoff.

Command recording has one additional concurrency boundary. Before the typed
operation, `internal/memory.RecordCommand` protected its incrementing upsert
plus read-back with process-local `commandMu`; the first routed version sent
that commit and query as two owner HTTP requests, so two client processes could
interleave and one could return the other's command line and counters. After,
the `record_command` operation runs the entire existing `RecordCommand` method
inside the owner, where the same mutex covers every routed client; direct mode
keeps its previous critical section, SQL increment, actor attribution,
deny-list scan and one-commit result. The route is still submitted once: if the
owner commits and loses the response, the outcome remains unknown and is not
retried. This owner-side multi-step restriction binds `RecordCommand` alone,
not every memory method, every pair of IPC requests, or every `Store` method.

`memdolt init` is the lifecycle exception. Before, it attempted a direct open
and surfaced the generic ownership-lock refusal. After, a verified live owner
causes a specific “stop the owner and rerun `memdolt init`” refusal before any
embedded open; with no live owner its creation, migration, output and
idempotence are unchanged. This stop-owner restriction binds `init` alone, not
every lifecycle or store-independent command: `doctor` continues to inspect a
live owner, and `version` remains store-independent.

Before issue #125, that exception described `init` alone. After it, `clone`
also requires the same exclusive ownership lock and refuses a live owner
before remote contact. It does not route a bootstrap through IPC. The new
restriction binds `localdolt.Clone`; `init` keeps its earlier stop-owner
refusal and migration behavior, ordinary commands retain owner routing, and
`doctor` and `version` retain their prior behavior.

Before issue #127, that bootstrap was the only shipped transfer. After it,
`push` and fast-forward-only `pull` operate on initialized stores through the
same direct/authenticated-owner choice as ordinary commands. Each sends one
complete typed operation; the owner runs the network work with its own
environment and cancellation context. Probe/authentication failures never
fall back to an embedded open, and a lost transfer response is not resubmitted.
The unknown-outcome remedy is inspection of local and remote main before a
retry. Confirmed promotion plus a later error survives the application/IPC
boundary. Clone and init retain the exclusive bootstrap behavior above.

Before issue #137, that pull path refused divergence. After it, compatible
divergence and complete explicit conflict resolution use the same single-owner,
one-submit boundary. Capture/preview and final promotion each finish before
human interaction; no durable transaction spans a dialog. Participating writes
share the owning Store mutex, while foreign Dolt processes remain outside it.
The pinned `DOLT_COMMIT` can advance main before SQL transaction finalization;
all validations precede that promotion point and confirmed hashes survive later
failures. Lost replies still require inspection, without replay (§11.2).

Before issue #129, remote configuration still required native Dolt with the
owner stopped. After it, `repo remote list` and `repo remote add` use that same
direct/authenticated-owner choice. Each configuration action is one typed
operation; no password crosses IPC and no remote is contacted. Add never
replays a lost response: inspect `memdolt repo remote list` before retrying.
Both configuration methods join the Store's transfer/mutation mutex; dirty
memory is preserved without commit/discard. Other reads, migrations and
foreign Dolt processes remain outside that mutex. Existing bootstrap,
transfer, attribution and review behavior remains.

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

Before #163 that identity was design-only. After it, explicit initialization
records `meta.project_id` and credential-free canonical `meta.project_origin`
in one user-attributed commit. The short ID matches tagged memhub; full-origin
comparison distinguishes hash collisions. Hub names replace ID hyphens with
underscores; the embedded database stays `memory`. Missing origin allows local
use without a host-path identity; Git/configuration failures refuse. Existing
stores require explicit `init --adopt-identity`; reopen never adopts, and
nonempty identities never auto-reassign. Global identity remains separate.
[The topology guide](../repository-topology.md) specifies supported origins,
explicit adoption, native transfer transitions and the full structural inventory.

Before #163's first review correction, branch-qualified identity reads could
mistake dirty native rows for committed identity. After it, Open and explicit
initialization capture main's immutable hash; readProjectIdentity requires that
hash for all its callers. Dirty rows remain preserved and cannot satisfy
adoption. The same correction rejects drive-relative Git paths and non-ASCII before folding, so
Windows paths and Unicode case folds cannot create shared project aliases.
Supported ASCII identity, full-origin collision checks and attribution remain.

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

  Before issue #116, this schema specified the provenance columns but shipped
  migrations stopped at version 3 without them. After it, append-only migration
  4 adds all five nullable columns; existing note text, actor, raw actor, and
  timestamps remain intact, and repeating `memdolt init` adds no migration or
  commit. Ordinary notes still store NULL metadata. Both the one-note CLI lane
  and `Lanes.CommitNotes` preserve supplied metadata and declare all five new
  strings for deny-list enforcement before committing: metadata can now refuse
  an otherwise allowed note. This declaration binds `LogNoteWithProvenance`
  (including `LogNote`) and `CommitNotes`, not arbitrary SQL writers or every
  string column. The MCP accumulator's per-actor batching, clean-working-set
  guard, retry, and shutdown rules in §3.1 remain unchanged.
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

**Divergent pull delivery (issue #137).** Before it, the policy above existed
in proposal review's fail-closed subset and status's rolled-back preview, but
pull refused every divergence. After it, divergent pull implements the policy:
compatible committed changes create one two-parent merge; actual data conflicts
show exact nullable base/ours/theirs rows and native blame plus reachable commit
author, email, date and message. A complete operator-selected final row is
required for each conflict. Same-live-key distinct facts retain every row:
choose an existing winner (optionally its manual durable value) and supersede
the other rows. Already-satisfied attributed UNIQUE records are verified and
cleared without row changes. Task completion survives unless an operator
explicitly chooses reopening; deletion cannot erase a done task. Final data
and constraint surfaces must be empty, maintained UNIQUE/FK checks must pass,
and fact/decision supersession links must be non-dangling and acyclic.

Supported same-primary-key data conflicts cover facts, decisions, tasks,
session notes, commands, both narratives, documents, document chunks and
proposal metadata. A chosen document deletion that would cascade into other
chunks refuses. Actual distinct-row uniqueness repair is confined to
`facts.live_key`; unresolved document-path/chunk uniqueness, foreign-key or
unknown constraint records, metadata/schema conflicts and unattributed records
fail closed with a Dolt/upgrade remedy. All three immutable DDL snapshots must
match the maintained schema. These extra checks bind divergent `Pull`, not
every commit, proposal acceptance or fast-forward transfer. Their established
validation and contradiction gates are unchanged.

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
4. Accept-time contradiction probe: the shipped cross-encoder scores incoming fact or decision prose against current same-kind durable rows; ≥2.0 blocks, and only an already-staged supersede proposal or explicit operator `--force` bypasses the probe.
5. Never auto-approve on absent elicitation response. Fail closed on missing actor attribution (`agent:unknown` = untrusted).
6. Doc ingestion stays a direct, user-attributed lane (docs are user-pointed artifacts, not agent claims) with the same path confinement: MCP `doc_add` restricted to repo root ∪ `allowed_dirs`, deny-list on top; CLI unconfined.

Before issue #132, item 6 described a deferred ingestion lane. After it,
repository ingestion ships as bounded in §11.2: the document's source is
`user`, but its Dolt commit is authored by the actual normalized caller.
This exception to proposal staging grants no review authority; all MCP
identities, including raw `user`, remain agent-class. Global ingestion stays
deferred.

Before #132's owner-credential fix, item 6's path freedom also admitted the
repository's `.memdolt/server.pid` with absent/empty deny rules. After the fix,
CLI root freedom and MCP confinement remain, but both reject that known owner
file and its aliases before reading content, as bounded in §11.2. Optional
deny-list configuration cannot disable this credential protection.

**M3 contradiction-guard handoff (2026-08-29, issue #102).** Before this
change, item 4 was a required but unshipped port and
`internal/store/localdolt/review.go` explicitly said the accept-time probe was
not there; `review accept` proceeded from one-commit and deny-list checks to
the merge protocol without comparing the claim with durable prose. After it,
ordinary repository fact and decision acceptance runs the fixed 2.0 probe
before `DOLT_MERGE`, while supersede proposals and an operator's `--force`
remain the only bypasses. The structural blast radius is explicit:

- `internal/review.Accept` is the shared application entry point for CLI and
  MCP promotion. It validates the repository's existing `[retrieval]` model
  configuration and supplies `embedding.Open` to the storage gate. It does
  not honor `retrieval.use_reranker = false`: that setting still controls
  recall, while the acceptance guard is mandatory. `retrieval.LoadConfig`,
  its defaults and validation, and all recall behavior otherwise remain as
  before. This mandatory-probe rule belongs to repository promotion through
  `internal/review.Accept`, not to every retrieval-config caller.
- `localdolt.Store.AcceptProposal`, `AcceptOptions`,
  `ContradictionScorer`, `ErrContradiction`, and `ContradictionError` own the
  storage boundary. An ordinary fact or decision accept requires readable
  model configuration; when current same-kind rows exist, it opens, runs and
  closes the scorer and refuses on any failure or any finite score ≥2.0.
  Facts compare `key: value`; decisions compare the optional summary, title
  and rationale in the same shape as production embedding input. Current
  facts are `superseded_by IS NULL`; current decisions are active and
  unsuperseded. Every such row is scored, without a recency cap. These rules
  bind `AcceptProposal` alone, not `PendingProposals`, `ProposalDiff`,
  `RejectProposal`, `ExpireProposals`, or every `Store` write.
- Before issue #106, `localdolt.Store.acceptMu` serialized only
  `AcceptProposal` calls on the one repository-owning Store, so a second
  promotion probed the first promotion's durable result rather than the same
  old `main`; concurrent accepts had previously been left to Dolt transaction
  timing. After issue #106, `proposalMu` retains that accept ordering and also
  serializes memdolt's proposal staging, rejection, and expiry mutations on
  that Store. At that delivery the boundary bound those four localdolt mutation
  paths alone, not `PendingProposals`, `ProposalDiff`, direct-lane commits,
  every Store implementation, or a foreign Dolt session editing the data directory.
  Issue #127 adds direct commits and transfers to that same Store mutex (§11.2);
  the read and foreign-session exclusions remain unchanged.
- `localdolt.requireSupersedeShape` and `validateSupersedeChanges` bind the
  model bypass to the shape `ProposeSupersede` actually stages: one live fact
  is changed only by its `superseded_by`/generated `live_key` handoff, one
  live replacement takes the same key, and no decision row changes. Before
  this guard, accept trusted a proposal metadata row whose kind said
  `supersede`; after it, a hand-crafted commit carrying that label without the
  link-and-replacement shape is refused rather than gaining the bypass. This
  shape restriction belongs to `KindSupersede` acceptance, not to ordinary
  fact/decision proposals or every two-row commit.
- `localdolt.acceptInTx` now runs the read-only probe before `merge` in the
  same rollback-capable transaction. A refusal, model-open failure, inference
  failure, non-finite score, or model-close failure therefore leaves `main`
  unmoved and the proposal branch present. A score below 2.0 reaches the
  existing `merge`, `resolveOrBlock`, surface re-check, constraint
  verification, and reviewer-authored `DOLT_COMMIT`. Before issue #106,
  post-commit cleanup attempted branch deletion directly. Issue #106 first
  added observed-head revalidation, then removed automatic deletion entirely
  for expected-commit/MCP accepts when review found that Dolt has no atomic
  expected-head delete; §11.1 records the exact CLI/MCP boundary. CLI cleanup's
  established populated-result-plus-error contract remains. The
  one-commit guard and accept-time deny-list scan still run before that
  transaction and retain their prior behavior.
- `localdolt.factSemanticText` and `decisionSemanticText` are the shared text
  renderers now used by both `EmbeddingSources` and the contradiction probe.
  Their output is byte-for-byte the prior embedding text, so embedding hashes,
  side-store identity, recall candidates, and reranker inputs retain their
  old behavior. This sharing applies to those two named renderers, not to
  task or document text.
- `memdolt review accept --force` is the sole new operator surface. It skips
  only contradiction configuration and inference; target validation,
  one-commit enforcement, accept-time deny-list scanning, conflict and
  constraint verification, reviewer attribution, the no-fast-forward merge
  commit, and the CLI post-commit head-check/deletion attempt all still hold. A
  supersede proposal takes the same bypass because its reviewed payload
  already declares which durable row it replaces. Force and supersede do not
  create a direct-write path, and accepted results remain the same auditable
  proposal-commit plus reviewer-merge graph. Existing review JSON result
  fields and the list/show/reject/expire/stale CLI surfaces are unchanged.
- The `facts`, `decisions`, and `proposals` tables, their columns and indexes,
  migration history, source rows, and commit message format do not change.
  The probe reads current `facts`/`decisions` from the transaction's durable
  `main` state and reads incoming prose from the immutable staging commit;
  it writes neither table. Tests inject deterministic scorers at the
  `ContradictionScorer` seam, while production promotion always supplies the
  checksum-verified shipped engine through `internal/review.Accept`; no new
  model, dependency, side-store, schema object, or configuration knob is
  introduced.

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
9. **Empty-recall observability** **[design]**: every ordinary `recall` call whose result set is empty above the rerank floor (`min_rerank_score`/`doc_min_rerank_score`, step 7) increments a counter. Evaluator calls are excluded from these local counters so offline measurement does not distort the operator-facing rate. The dominant long-term failure mode for this system is an agent that never triggers recall at all — a call that runs and comes back empty is the one signal that would be visible instead, and `doctor` surfaces the running empty-recall count/rate as a named check.

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
  `LastChanged`. Those three methods and `EmbeddingSources` share
  `committedMainConn`, so all four concrete readers are
  committed-`main`-`HEAD` only. Moving `EmbeddingSources`' existing branch
  check into that helper did not change its eligibility or read-only behavior.
  The restriction belongs to these four named LocalStore methods, not to every
  LocalStore reader or every `Store` implementation. Existing `Store.Open`,
  `Commit`, `Query`, and `Close`, review/proposal paths, and deny-list behavior
  remain unchanged. FULLTEXT still gathers at most 50 distinct rows per source
  type by grouping before the limit, and optional provenance comes from each
  table's `dolt_blame` row.
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

That is the M2 before-state. Issue #132 adds repository document ingestion
through the already-existing committed source readers and derived index;
§11.2 records its scope. Retrieval ranking, default-doc rerank requirements,
explicit `doc_chunk` filters and scoring floors remain unchanged.

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

**M2 completion (2026-08-29, issue #91).** `memdolt search <query>` now
ships the M2 text-search surface: plain text falls back to decision search,
memhub's decision prefixes select it explicitly, and Dolt FULLTEXT ranks
committed decision titles and rationales with stable JSON. Empty and
punctuation-only queries are refused before search SQL. An explicit
`file:<path>` request fails loudly with the M5 code-index/git-ingest remedy;
it does not pretend that a missing history corpus produced an empty match.

Before #174 that refusal remained the implemented behavior. After it, the
explicit Git-history ingestion and cached file-search surface in §9 replaces
the refusal, with honest empty results for missing cached history. Explicit
decision prefixes and unindexed decision fallback retain their existing Dolt
FULLTEXT route, output fields and ranking.

`memdolt eval retrieval [--golden <path>] [--mode hybrid|fts]` loads the
committed version-1 golden format and runs every `match` and `empty` query
through the same production `retrieval.Recall` path as the CLI. Its human and
JSON outputs include every outcome, Recall@3, and empty-query safety failures;
the command exits nonzero below the recorded 100% baseline or on any safety
failure. The selected hybrid run remains vector-only candidate ordering with
the 20-row rerank pool (§8.1): the exact hermetic result is **21/21 matches,
Recall@3 = 100%, and zero safety failures**.

The gate runs under the existing protected `Test (ubuntu-latest)` matrix
check, not an advisory job. GitHub Actions caches `~/.memdolt/models` by the
committed `models/manifest.json` hash; `internal/embedding.Open` re-verifies
the cached model files, official runtime archive/extracted-library pins as
applicable, and the runtime library before ONNX initialization, so an
unverified cache entry is never used. MCP exposure remains M3. Actual global
store merging, document ingestion, the code index, git-ingested file history,
`locate`, `eval locate`, and their golden corpus remain M5; M2's hermetic
fixture rows exercise retrieval scoring without claiming those surfaces ship.

Before issue #132, the M5 list above still included all document ingestion.
After it, repository doc commands and `doc_add` ship; global documents,
code indexing/git history/locate and full M5 parity remain separate. No
retrieval golden query, fixture, quality threshold or scoring rule changes.

Before issue #138, that remaining list still included the local code index,
locate and its eval gate. After it, §9's implemented subset and the unchanged
locator golden formats ship. The full Rust match bar is 18/18 on its complete
corresponding benchmark corpus; polyglot is 17/17. Separate harness floor-0
rerank checks reject both nonsense probes on each corpus. Default fusion has
no floor and honestly reports both leaks. [The frozen-reference notice](../../tests/golden/testdata/locate/NOTICE.md)
documents why the later v0.2.0 tree, whose helper relocation left a stale golden
path, is a separate 17/18 diagnostic rather than that benchmark's corpus.
The memory golden, scale sweep and their selected-strategy thresholds remain.

---

## 9. Code index & `locate`

Direct port of memhub's design; storage = local SQLite via `modernc.org/sqlite` (pure Go, no cgo, FTS5 available). Never versioned, never synced, never read by recall.

- File set from `git ls-files -z` through the deny-list; per-file staleness = (mtime,size) fast path then content hash; `HEAD` stamped for reporting only.
- Chunkers: tree-sitter (Go bindings) for the same 7 grammars — rust, c#, java, ts, js, python, go — with memhub's chunking rules (top-level items, `Type::method`, container header-chunks with excised bodies, doc-comment folding, LF normalization); 50-line/4000-byte window fallback.
- Fusion knobs `[code_index]`: fts 0.5 / vector 0.5 / `test_path_penalty` 0.90; reranker off by default (memhub decisions 122/123 carry over); lazy refresh before every query.
- Returns ranked `{path, start_line, end_line, symbol, kind, score, snippet≤6 lines}` — breadcrumbs, never full files.
- Before #174, schema-version mismatch meant drop + rebuild (the index is regenerable). After it, recognized v1 upgrades to v2 in place and unsupported/foreign schema refuses without replacement; explicit whole-index removal deletes cached history too. Durable `upgrade` remains separate.
- Git-ingest history tables (`commits/files/commit_files` + `search file:<path>`) live here too (§6.1 note).

**Implemented code-only subset (issue #138).** Before this delivery the bullets
above were a destination contract with no code-index/locate implementation.
After it, code index/status/rm, locate, eval locate and the typed local MCP
locator ship; the final Git-history bullet remains a placement design only.
No history corpus, `search file:` implementation, global store or full-M5
completion is implied. SQLite remains a pure-Go dependency; the actual seven
tree-sitter parsers use the already-mandatory cgo build.

**Cached Git file history (issue #174).** Before this slice the final Git-history
bullet and `search file:` remained deferred as recorded above. After it,
`ingest-git [--since <nonempty-commit-ish>]` supports `--dir`/`--json`, observes
local committed history through one captured HEAD, and atomically caches Git
author/author-date/subject and exact file-change metadata in the derived index.
`--since` selects the resolved revision difference, not a date. Invalid or
unavailable observations fail; repeated/overlapping ranges do not duplicate
rows. Raw object validation and NUL framing replace the tag's lossy/quoted-path
parsing. Configured deny rules and protected paths apply without echoing denied
contents. Author timestamps sort by parsed instants with full-ID ties; rename
and copy records attach to destinations and merges compare the first parent.

CLI and existing MCP search route explicit file queries, or cached path-looking
queries, before any memory access. Missing cached history is empty. Results
disclose range count/last ingest, author-date semantics, limits and truncation;
the union of prior ranges can be partial and is never a fresh Git observation.
These calls neither open Dolt/its owner, read source bodies, load models, ingest
implicitly nor flush notes. Decision output/ranking/routes and MCP input/names
remain. The derived v1-to-v2 extension preserves source/chunk/vector rows, source
refresh preserves historical paths, and `code rm` removes the whole recognized
cache. Unknown/foreign/unsupported state refuses. [The complete Git-history guide](../git-history.md)
records every reached symbol/file, changed restriction, failure outcome, test,
count and writer/reader boundary. No durable schema, dependency, global history,
scoring rule, frozen corpus, threshold or full-M5 completion is added.

`internal/codeindex` never opens/migrates Dolt or routes to its owner. Before
#174 its operations touched only its own SQLite/lock plus tracked source reads;
afterward explicit Git ingestion also reads local committed objects, and cached
file search reads metadata/path identities only. Memory embeddings,
committed/proposed memory, exports and transfers stay separate.
Status creates nothing. A recognized application ID, safe file identity and
known derived schema bound rebuild/removal; unknown files or unrelated schema
are retained. DELETE/FULL journaling intentionally replaces tagged WAL/NORMAL
here, preventing read-only status from creating WAL/shared-memory sidecars.
Journal residue refuses pending inspection. An exclusive `code_index.lock`
serializes cooperating refresh/query/remove calls and fails visibly on
contention. There is no automatic crash-residue/PID cleanup. Foreign writers
are outside this lock; checked SQLite pathnames and removal identity checks
do not eliminate the final check/open or check/remove interval.

Tracked file diffing retains the millisecond mtime/size fast path, then raw-byte
hash checks. HEAD is only reported. A metadata-preserving edit can remain
unseen. Changed deny rules force content rescanning. Deleted/renamed, denied,
linked, unreadable and binary replacements lose stale chunks/vectors; binary
includes invalid UTF-8 and NUL. Chunk/FTS transactions complete before vector
backfill; a visible inference failure preserves that complete text index.
Model/dimension/text/vector hashes, length, finite values and nonzero norm
gate vector currency. Per-file counts and the committed result distinguish
skips, exclusions, denials, binary files and completed effects.

The seven ABI-15 grammar pins and official Go binding are recorded in AGENTS.md
and `go.mod`; no broad language pack is used. The tagged top-level/container/
method, receiver, documentation/module-doc and fallback rules are retained.
TSX selects its own grammar. Normalizing before parsing also prevents a Go
comment boundary between CR/LF from retaining a stray CR. Parser/tree/cursor
resources are released; native load/parse failures remain errors. Oversized
AST bodies remain intact. The shared inference tokenizer now enforces the
previously missing 512-token fastembed limit, including BERT special tokens
and longest-first pair truncation. This affects every `encodeSingle`/`encodePair`
caller; short inputs, NFD, verified artifact loading and memory scoring/config
remain unchanged. It never truncates stored source text.

Before the #138 review-cycle lifetime fix, closing parser/tree/cursor resources
still left the pinned binding's progress-options C registration and captured
request context unreleased. After it, `ChunkFile` passes nil parse options,
using only the input callback whose registration and C strings the binding
releases. Cancellation checks occur at entry, native input requests, immediately
after native return and after the AST walk. Canceled input can produce a partial
tree; that tree is closed before returning the error. Native work between input
requests is no longer periodically interruptible, so cancellation may wait for
it. This boundary binds code chunking and its CLI/MCP/eval refresh callers, not
every parser or fallback helper. A lifetime regression and the unchanged
reviewer probe verify request-context release. Grammar/binding versions,
chunking/scoring, source protection, corpus pairing and full golden bars remain.

`Locate` returns at most six lines and 400 characters, including an ellipsis,
per snippet; that bound applies to locator outputs, not internal `ChunkFile`
bodies. CLI-only no-refresh retains old ranking/line/HEAD metadata and makes
no Git command, while snippets read current files through the same root,
link/reparse, owner-identity and deny checks. MCP and eval always refresh.
Canonical confinement excludes absolute/traversing/stream paths and protected
metadata. `layout.CheckOwnerSource`, extracted from document ingestion, checks
the opened file against known owner credentials before reads; every `DocAdd`
retains its previous protection, and code/config/header reads now share it.
This protects that known file and aliases, not arbitrary copies or all readers.

FTS5's quoted AND query, 100-candidate lexical gather, min/max normalization,
clamped cosine, 0.5/0.5 fusion, 0.90 top-level test penalty and deterministic
ID ties retain tagged behavior. Runtime reranking remains opt-in and floorless;
only the eval harness applies its optional floor. The [code locator guide](../code-locator.md)
records configuration compatibility, freshness limits, lifecycle and every
surface. AGENTS.md inventories the complete structural blast radius.

---

## 10. Global store

A `global` Dolt database on the hub, cloned to `~/.memdolt/global/` on each machine — **genuinely global from day one**, which memhub only achieves via the r2 hub proposal.

- Same schema as a project DB; scope governed by the active repo's retrieval config (memhub parity).
- Enablement per-repo (`[global] enabled`). Off ⇒ recall byte-identical to repo-only.
- Born-global (`fact add --global`) and promotion (`fact promote <ID> --global` — copy, not move; repo row wins locally) — both **CLI-only** (§7.2).
- Recall merges scopes into one pool, one rerank pass, provenance tags only — never drops a hit for being global.
- Sync: plain push/pull like any project; ULID keys make cross-machine global writes merge-clean.
- memhub's `global_accept_markers` replay machinery is unnecessary — merge idempotency does the job.

Before issue #146, these global replica, human-write, document and merged-recall
surfaces were design-only. After it they ship with the same schema in the exact
layout `~/.memdolt/global/.memdolt/dolt/memory`; global ownership and the separate
derived `embeddings.sqlite` are adjacent under `global/.memdolt`. No existing
repository path changes, new durable table/dependency, hub-only metadata or
host-root column is introduced. Native Dolt remotes, schema/version checks,
authentication and transfer guards are reused. [The global memory guide](../global-memory.md)
specifies bootstrap, layout, metadata, recovery and commands completely.

`global enable/disable/status` use the calling repository's `[global]` settings,
default off. Disable preserves every replica and other repositories' settings.
After enable, explicitly use `global init` or `global clone <remote-url>`; ordinary
reads/writes never bootstrap or migrate. `repo remote add/list`, `repo status`,
`push`, `pull` and `index status/rebuild` select this replica with `--global`.
All retain `--dir`/`--json` conventions and existing transfer/authentication
behavior. The global wrappers take one exclusive replica lock; contention with
another CLI/MCP owner visibly refuses, without a competing engine, policy from
another repository, or an uncertain-write retry.

Human fact/decision commands and `doc add/list/show/remove --global` now exist.
Only normalized trusted user writes enter this global CLI lane. Existing nullable
schema fields survive promotion, which captures committed repository data and
copies it with a fresh ULID, source commit/id in the result and commit message.
Repository rows and history remain intact. The earlier “repo row wins locally”
means no local mutation: both hits remain eligible for ranking. Existing live
global fact keys refuse promotion; explicit human fact add retains its guarded
live-key upsert. Decision title collisions preserve both records and report ids.
Ambiguous keys require ids; promote a live replacement instead of copying a
cross-scope supersession link. Global document content hashes/chunk replacement
and scoped stored-id reads/removals retain their existing semantics. The prior
first-empty-table config flip still governs repository documents; every successful
global add independently enables its calling repository's default global docs,
including an unchanged document already populated by another repository.

The shared CLI/MCP application seam feeds committed scope captures into one
candidate pool, one query embedding and one rerank pass. Equal ids/keys do not
collapse. Hits report scope/captured commit and optional matching blame. Captures
serialize participating mutations and refuse a changed foreign main; each scope
has its own commit, not a distributed snapshot. Active repository retrieval knobs
and independent default-doc settings govern both scopes. Global vectors stay
local, use the same verified model/current-source hashes, and emit scoped fallback
warnings. Disabled recall uses the prior path and output except timing/observability.
Missing/corrupt/newer replicas are reported visibly. Tasks, notes, narratives,
archives and code remain outside the global corpus despite the shared schema.

`OpenGlobal`'s opt-in/lock policy binds its callers, not arbitrary native Dolt
sessions or raw stores opened at that directory. Global document guards and
global `EmbeddingSources`/`RecallSources` filtering bind stores opened by that
wrapper; ordinary repository readers remain unchanged. `CapturePromotion` and
`CaptureRecall` are new authenticated owner reads, with no global write operation.
At #146, existing global-target proposal acceptance refusals and terminal remedies
remained; #161 below replaces only the terminal refusal. No accepting MCP path or
fake success is added. AGENTS.md inventories every
changed structure. Isolated real replicas/local remote, CLI/owner/MCP, mixed
model retrieval and the unchanged golden gates support this delivery; physical
two-machine hub acceptance and full parity remain separately tracked.

Before the #145/#147 integration, the #146 branch still had the older document
native-result handling. After integration, global DocAdd/DocRemove retain #145's
observed commit/identity results on finalization errors and cancellation, while
unobserved results retain ErrCommitUnknown. Global add does not attempt config
finalization after a native error; a confirmed result names the calling repo's
`[global]` repair even if another repository populated the table earlier. The
independent config-only flip for unchanged global docs remains. Real native
tests distinguish observed and unobserved hashes and verify reopened rows/history
without replay. These rules bind the shared document/native seams already named
in #145, not all SQL errors. Session-render NoteCommits, note-group lifecycle,
owner result envelopes and #147's hub/CI behavior remain unchanged.

Before #146's first review correction, enable/disable could mutate config and
then lose its change report on path, read/close or output failure. After it, the
shared CLI path preflights config/global paths and preserves any confirmed flag
change through later failures, including JSON/human inspection remedies. The
rooted writer and every other global/native/MCP policy retain their boundaries.

**Terminal global proposal acceptance (issue #161).** Before this delivery,
`review accept` refused global targets despite §7.2's terminal remedy. After it,
the trusted human can run `memdolt review accept <id> --dir <repository>` with
human or JSON output. Direct and authenticated live-owner terminal routes use
the calling repository's enabled, existing, current global replica. Only global
main changes. MCP elicitation and ordinary `AcceptProposal` retain their global
refusal, and expected-commit repository acceptance keeps its existing behavior.
The full workflow and recovery contract are in
[the global acceptance guide](../global-memory.md#terminal-proposal-acceptance-issue-161).

The complete captured source commit must have one parent and be one commit
past repository main ancestry, preserve the fixed schema, add exactly its own
proposal metadata, and contain only an ordinary new fact, active decision,
live-fact overwrite, or same-key fact supersede. Destination ids/live keys
cannot collide; overwrite/supersede before-images must match complete global
rows exactly, including NULLs. Repository-only references, changed destination
rows, extra commits/schema/payload and disguised supersedes refuse. Stable
source row/proposal ids, nullable fields, timestamps and agent provenance are
copied into a real global staging commit authored by the original staging
author, whose message names the immutable source commit. The human then authors
a real native no-fast-forward merge. No history is fabricated and source
metadata dates are not changed to new commit dates.

Global enablement and the calling repository's deny-list are rechecked. The
shared scorer probes durable global same-kind rows at 2.0 before staging;
model/config/inference/close errors fail closed. Force bypasses only probing,
as does a validated supersede. Path, owner credential, schema, clean/merge and
exclusive-lock refusals remain. Lock order is repository `proposalMu`, a
nonwaiting global file-lock attempt, then the private global `proposalMu`.
Recall's reverse capture order cannot deadlock this path because that attempt
never waits. The operation serializes cooperating repository writes and holds
global ownership through model use and native writes. Foreign writers remain
outside these locks; captured heads are rechecked and the immutable-hash merge
retains native fail-closed semantics, not a distributed transaction or CAS.

Confirmed source/global staging/acceptance hashes and row ids survive late
native finalization, cancellation, owner envelopes and CLI close/output errors.
Unobserved native results and lost owner replies remain explicitly unknown and
never automatically replay. A repeated explicit accept verifies native
two-parent reviewer history, the source-bound staging parent, and the exact
staging/merge payloads before reporting the existing acceptance without a
second merge. Different/incomplete residue or matching rows without that native
history refuses. No `global_accept_markers`, other durable table, dependency,
or migration is introduced.

Acceptance retains both branches because Dolt has no atomic expected-head
delete. Global merged residue follows existing reachability filtering; the
source remains in repository pending lists/counts because repository main is
unchanged. After inspecting current source content/hash and confirmed global
history, the human can explicitly reject the retained source. Changed source
content must be preserved for fresh review. Reject/expiry retain their existing
best-effort cleanup boundaries, not a new deletion guarantee. These restrictions
bind the terminal global acceptance seams, not all native SQL or Store writes.
AGENTS.md records every touched structural element and preserved behavior.

Isolated native tests cover exact payload/history, before-images, conflicts,
probe failures, ownership, cancellation, partial/unknown outcomes and repeated
acceptance. Fresh-process direct/live-owner CLI checks and a checksum-pinned
model acceptance/global-recall check exercise the reached runtime surfaces.
No test uses a live user global store, credentials, host installation or hub.
Frozen golden assertions and all 22 MCP registrations stay unchanged. Broader
M4/M5/M6 parity and physical two-client hub acceptance remain separate.

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

**Approved M3 phasing (2026-08-30, issue #104).** Before this delivery, the
table above described the destination surface without distinguishing which
names the first M3 server could honestly advertise, while the runtime
foundation itself registered no memory tools. After it, M3 registers exactly
the implemented read tools `status`, `recall`, `search`, `list_tasks`,
`list_decisions`, `list_facts`, `list_proposals`, and `get_command`; direct
tools `task_add`, `task_done`, `log_session_note`, and `record_command`; and
staged tools `propose_fact`, `propose_decision`, and `propose_supersede`.
`review_pending` and its fact-key elicitation remain a separate M3 delivery.
The later-backed names `locate`, `doc_add`, `render`, `repo_status`,
`repo_pull`, `repo_push`, `history`, and `archive_transcript` are absent from
`tools/list`, not refusal stubs. This is a registration restriction on the
first M3 implementation alone, not removal of those tools from the destination
contract or permission to substitute a stub before its backend lands.

**Native memory history phasing (issue #173).** Before this slice, `history`
remained one of those absent tools and the combined RegisterTools surface had
22 names. After it, the real typed `history` tool adds one name, making 23.
The CLI and tool share the [native history contract](#native-memory-history-issue-173)
below. Every earlier input/registration, attribution, discovery/cache hint,
review/queue behavior and retrieval score remains unchanged. This count and
schema extension bind RegisterTools alone, not arbitrary SDK servers.

Before issue #132, `doc_add` was still one of those absent names. After it,
the real repository `doc_add` registration initially joined the sixteen tools
present after #106. After integrating #133, those tools and `render` remain,
and `doc_add` became the eighteenth tool. After subsequently integrating #131,
`render` and `repo_status` remain and `doc_add` is the nineteenth tool.
Its typed input is `file` plus optional
`title`, with no global, actor, confinement-disable or arbitrary SQL argument.
§11.2 records the
shared document operation and path boundary; other deferred names stay absent.

**Locator phasing (issue #138).** Before this delivery `locate` was still
absent. After it, the previous nineteen real tools remain and typed `locate`
adds query/limit/rerank input, bounded breadcrumbs and lazy local refresh.
There is no repository-path, SQL, no-refresh or score-floor input. It uses
`Toolset.baseDir` and the independent local index even when the Dolt backend
is unavailable; it neither routes through owner IPC nor flushes notes. The
twenty-tool count binds `RegisterTools` at this delivery. Existing discovery,
attribution, cache hints, review, batching and shutdown behavior remain.
The existing `serve` command still owns its Dolt startup/shutdown lifecycle;
the local-only restriction applies to the locator call, not server startup.
After integrating #137, its twenty-one registered tools including `repo_pull`
and `repo_push` remain, and `locate` makes twenty-two under `RegisterTools`.
The transfer/elicitation implementation, immutable provenance, guarded stdio,
original input closer and legacy negotiation are preserved. Shipped `serve`
therefore also applies its raw-Unicode input guard to locator requests;
standalone `New` and arbitrary transports keep their existing boundaries.
The integration changes combined discovery/template expectations, not locator
storage, parsing, cancellation, inference, source protection or golden data.

**Render phasing (issue #133).** Before this delivery the later-backed list
above still included `render`; after it, the existing sixteen tools (M3's
initial fifteen plus elicited review) remain and the real typed `render` tool is added. It
accepts an empty input and reads only the owner's configured output directory.
It preserves structured written-file/backup evidence on tool errors and never
replays file replacement after a lost response. Modern/legacy attribution,
static `tools/list` cache hints and every prior handler remain unchanged.
This registration rule binds `RegisterTools`, not arbitrary SDK servers.
Before #145 render included only already-committed notes and did not flush the
accumulator. After #145 its session wrapper flushes first under §3.1's result,
retry and inspection rules; standalone Store.Render is unchanged. No other deferred tool is
introduced by this render delivery, and it does not complete M5.

**Elicited-review handoff (2026-08-30, issue #106).** Before this delivery,
the approved M3 implementation above advertised exactly those fifteen tools,
`propose_fact` returned the live-key collision without opening the §11.1
dialog, and this section said `review_pending` and fact-key elicitation
remained separate. After it, the same fifteen tools remain and
`review_pending` is the sixteenth; an elicitation-capable `propose_fact`
resolves a live repository key through the reviewed overwrite, supersede,
keep-both, or cancel paths. The complete structural blast radius is:

- `internal/mcpserver.RegisterTools` retains all fifteen prior registrations
  and adds `review_pending`. `cmd/memdolt.runServe` still registers one
  `Toolset` on the already-open owner, so discovery, static cache hints,
  attribution, shutdown ordering, and the five-minute/per-actor note lifecycle
  are unchanged. The sixteen-name restriction binds production
  `RegisterTools`, not arbitrary go-sdk servers or the later-milestone names
  that remain absent.
- `internal/mcpserver/elicitation.go` owns both dialogs and their
  `pendingElicitation` shape; `elicitation_state.go` stores each one as a real
  row in process-local in-memory SQLite. Its cryptographically random 256-bit
  `requestState` is stored only as a SHA-256 lookup hash, expires after two
  minutes, and is atomically deleted before a response is interpreted. The row
  binds the owning repository data directory, attributed MCP client, exact
  proposal IDs and staging commits, queue position, and action. **Before the
  cycle-1 fix,** it stored IDs and position but not the commit displayed, so a
  branch reset to another single commit under the same ID could pass approval;
  **after it,** the displayed commit travels through state and is checked inside
  the proposal-mutation critical section before any merge. Missing, expired,
  mismatched, forged, replayed, malformed, incomplete, declined, or canceled responses
  cannot promote or discard a proposal. Authorization-state insertion and
  consumption happen before promotion and fail closed. Continuation
  bookkeeping is a distinct phase: its progress update can fail after an
  already-authorized merge, in which case review stops and reports the accepted
  prefix rather than claiming the merge did not happen. Before issue #106,
  `Toolset` held only pending note groups; after, it also owns this short-lived
  relational state, which restart or `Toolset.Close` destroys. This token
  discipline binds request states and cursors issued by this `Toolset` alone,
  not every go-sdk `requestState`; no Dolt migration, persistent side-store
  file, embedding-side-store table, or reusable client-held approval credential
  is introduced.
- `review_pending` snapshots repository proposals oldest-first. Native
  2026-07-28 successive review performs at most nine input rounds per call and
  returns a single-use, two-minute continuation cursor bound to the same
  repository, client, proposal IDs and commits, next position, and accumulated
  progress. **Before the cycle-1 fix,** truncation followed by a fresh call
  restarted at skipped entries and made proposal ten unreachable; **after it,**
  the cursor resumes at the untouched tail without changing which proposal was
  approved. A genuinely legacy session gets one form elicitation with one
  approve-or-skip field per proposal, because the SDK legacy middleware
  reinvokes the handler only once; batch mode also uses one form round. Batch
  acceptance is sequential, not atomic: each successful accept lands and a
  later guard refusal stops with the accepted prefix still durable. Before the
  fix, the dialog incorrectly promised that all shown proposals stayed pending
  on a later failure; after it, the dialog and structured result state partial
  progress. No global-target proposal enters elicitation. Every terminal result
  reports the global count and `memdolt review` remedy; a mixed queue without
  form elicitation reports both repo and global counts. An empty elicitation
  capability retains the protocol's assumed-form compatibility, but a URL-only
  capability gets the CLI fallback. The global exclusion binds MCP
  `review_pending` alone: `list_proposals` and the CLI remain able to see and
  discard global proposals, subject to the shared mutation and cleanup rules
  below.
- `localdolt.Store.proposalMu` is the one memdolt-owned proposal-mutation
  boundary on a repository Store. **Before the cycle-2 fix,** expected-commit
  validation excluded only another accept; staging, reject, and expiry could
  mutate the same branch after validation. **After it,** `stage`,
  `AcceptProposal`, `RejectProposal`, and `ExpireProposals` share the mutex, so
  one of those operations finishes before another can act on the branch.
  `PendingProposals` and `ProposalDiff` remain reads. At issue #106, direct-lane
  commits still moved `main` independently; issue #127 adds them and transfers
  to this mutex as recorded in §11.2. A foreign Dolt session does not share the Go
  mutex. **Before the cycle-3 fix,** expected-commit acceptance shared the
  eager cleanup used by CLI accept, reject, expire, and failed staging: it read
  the branch's `dolt_branches.hash`, compared it with the observed commit, then
  called `DOLT_BRANCH -D`. The table is read-only, the procedure accepts no
  expected hash, and a foreign session could therefore move the branch between
  those operations and have unseen content removed. **After the fix,** an
  `AcceptProposal` call with non-empty `ExpectedCommit` merges only the shown
  hash and never automatically deletes its branch. Production MCP is that
  caller; an unchanged branch is merged cleanup residue hidden from pending
  review, while a foreign commit makes the retained branch pending again. This
  no-delete behavior binds expected-commit acceptance alone, not every
  `AcceptProposal` or every review verb. CLI accept, reject, expire, and
  abandoned-stage cleanup intentionally preserve their earlier automatic
  `deleteProposalBranch` behavior. `proposalMu` excludes memdolt-owned races and
  the helper's comparison catches a foreign change before its final read, but
  those named CLI/cleanup paths cannot claim safety from a foreign change after
  the read because Dolt provides no atomic compare-and-delete primitive.
- A confirmed repository approval calls application
  `ReviewAcceptExpected` with the human `user` commit author and `force=false`.
  `internal/review.AcceptExpected` carries the shown commit into
  `localdolt.AcceptOptions.ExpectedCommit`, which `AcceptProposal` checks inside
  that proposal-mutation boundary before merge. Contradiction
  configuration/inference, accept-time deny-list scanning, exactly-one-commit
  validation, supersede-shape validation, fail-closed conflict and constraint
  verification, and the reviewer-authored merge still run in their established
  order. The expected-commit/non-force/no-automatic-delete rule binds elicited
  MCP acceptance alone. CLI `ReviewAccept` supplies no expected commit and
  still attempts post-merge branch deletion, CLI `review accept --force`
  remains the explicit operator exception, and no other review verb gains
  promotion capability. `cmd/memdolt.localCommandStore`
  implements both application seams and `runServe` exposes the expected variant
  to owner IPC; command selection, config loading, and existing CLI output are
  unchanged.
- CLI `AcceptProposal` retains its established post-merge contract: a branch
  cleanup failure returns the populated result proving `main` moved together
  with the error. Expected-commit acceptance now returns that populated result
  while deliberately retaining the merged branch, which is policy rather than
  a cleanup failure. The owner wire still transports any populated result plus
  application error without retrying. **Before the cycle-2 fix,** successive, batch, and legacy
  elicitation discarded that result and reported the proposal as blocked.
  **After it,** each mode records the accepted proposal and merge, reports the
  cleanup error separately in `failures`, returns `cleanup_failed`, and stops
  without attempting later entries; earlier batch acceptances remain reported
  too. Before this fix, `PendingProposals` treated every physical proposal
  branch as pending, so an unchanged branch left after a landed merge could be
  offered again. After it, a branch whose current head is reachable from
  `main` is cleanup residue and is excluded from pending results, while a branch
  changed to an unmerged head remains pending for review. This reachability
  filter binds `PendingProposals` and its callers, not `ProposalDiff`, reject,
  expiry, or the physical branch itself.
  **Before the cycle-3 terminal fix,** `reviewTerminal` returned an empty tool
  error when its post-merge `PendingProposals` recount failed, discarding the
  accepted prefix and cleanup-failure evidence. **After it,** accepted results,
  status, skipped IDs, and failures remain the primary structured output;
  repository/global counts are best-effort, a separate `recountError` reports
  the failed refresh, and the remedy directs the operator to terminal review.
  This behavior binds terminal recount in `reviewTerminal`; the initial queue
  read in `startReview` still fails closed.
- `propose_fact` still stages a fresh key exactly as before. On a live
  repository key it now shows the current and proposed rows inline and binds
  the response to that exact input and current-row image, including nullable
  fields. **Before the cycle-1 fix,** the handler compared the current row and
  then separately began staging, leaving a main-change window; **after it,**
  `localdolt.Store.ProposeFactResolution` cuts a branch from main and validates
  the exact shown image on that branch before applying any selected write. A
  pre-cut change removes the abandoned branch and stages nothing; a post-cut
  main change remains subject to normal accept-time conflict verification.
  Overwrite stages an in-place value/source/kind/evidence update and clears
  `verified_at`; supersede stages the link before its same-key replacement;
  keep-both validates and inserts a distinct key with at least two non-empty
  dotted segments. Cancel, decline, malformed input, a changed current row, or
  missing elicitation stages nothing. This expected-snapshot behavior belongs
  to `ProposeFactResolution` alone, not every fact update, `ProposeFact`, or
  `ProposeSupersede` call; the three original staged-write tools, one-live-row
  cross-machine conflict signal, and CLI surfaces retain their prior contracts.
- `storeipc.Backend`, its operation allow-list, and `OwnerStore` carry that
  expected-snapshot resolution and expected-commit acceptance fields with the
  same argument-based JSON transport. Existing token authentication,
  one-submit/no-retry write rule, actor propagation, SQL argument binding,
  error visibility, and every prior routed operation are unchanged. The review
  operation now carries a populated post-merge result and cleanup error in one
  authenticated response; `OwnerStore` restores the same result-plus-error Go
  contract without retrying the write. Ordinary pre-merge failures retain the
  existing non-200 error path. These additions bind this owner transport, not
  arbitrary HTTP handlers or other operation results.
- `internal/mcpserver/elicitation_test.go` adds modern MRTR, a real one-round
  legacy multi-proposal path, proposal-ten cursor traversal, mixed and URL-only
  fallback, partial batch progress, pre-authorization and post-merge storage
  failures, guard, replay/state, and atomic fact-conflict regressions. It now
  also covers populated-result cleanup failure in modern successive, batch,
  and genuine legacy review, including the accepted prefix and pending tail,
  plus post-merge recount failure in all three modes and a prior successful
  batch acceptance.
  `localdolt/review_mcp_test.go` retains the pre-response successive/batch reset
  refusals; deterministically pauses after expected-commit validation for
  reject and expiry serialization; resets and amends externally after that
  validation to prove only the displayed hash merges and changed branches are
  retained; deterministically adds foreign content at the final post-merge
  cleanup boundary to prove expected-commit acceptance removes nothing; and
  injects CLI cleanup failure to prove the result is populated and an unchanged
  merged branch is not pending. `storeipc_test.go`
  proves that result-plus-error contract survives the authenticated owner
  route. These tests exercise the named seams and do not claim to coordinate a
  foreign process inside the Go mutex or exhaust every Dolt branch operation.
  `tools_test.go` and
  `cmd/memdolt/serve_test.go` change their exact registration expectation from
  fifteen to fifteen-plus-`review_pending`; `server_test.go` only exposes
  client options to those protocol fixtures; `storeipc_test.go` adds routed
  resolution and expected-commit parity; and callback-signature updates in
  doctor and soak fixtures change no production behavior. `AGENTS.md`
  preserves the matching operator-facing
  before/after. No dependency, durable schema, derived side-store layout,
  global promotion path, host registration, or provenance schema changes. CLI
  and MCP review share mutation serialization, acceptance guards, result
  contracts, and pending filtering; their post-merge cleanup differs only at
  the exact boundary above.

Server instructions embed memhub's routing rules (recall-before-ledger, locate-before-grep, turn-1 PROJECT.md, never-write-durable-directly) adapted to memdolt names, plus two memdolt-native additions **[design]**: the fact-key namespace convention (§6.1 — `build.*`, `convention.*`, `env.*`, `gotcha.*`, and similar dotted prefixes) so agents file under an existing prefix instead of inventing ad hoc keys, and the filing rule that decides fact vs. decision — **facts state what is true; decisions record what we chose and why — if there's a "because," it's a decision.**

The server instructions text is itself a versioned, first-class artifact: checked in, and its changes are reviewed as deliberately as a schema migration, not tweaked ad hoc. It encodes the agent's recall-decision policy — recall-before-ledger, when to file a fact vs. a decision, what prefix a new fact key gets — and an undiscussed edit to that policy is exactly as load-bearing as an undiscussed column change. **[design]**

**Remote inspection handoff (issue #131).** Before this slice, production
registered seventeen tools (sixteen M3 tools plus #133's `render`) and
`repo_status` remained absent. After it, those seventeen tools retain their
behavior and the typed `repo_status` handler calls the same complete
owning-store inspection as the CLI below. It accepts optional `remote`,
`local`, `diff` and `user`; it cannot select another repository or supply a
password. `local` requests offline
inspection; `diff` requests exact committed local-to-remote changes with
nullable row values. Structured output retains local fields and adds selected
remote, captured commit hashes, status, assessment and any diff/remedy.
Modern/legacy agent-only attribution, discovery, long tool-list TTL, notes,
elicitation and orderly shutdown are unchanged. The eighteen-tool count
binds `RegisterTools`, not arbitrary SDK servers. Global proposals remain
counts from this repository. `repo_pull`/`repo_push` remain unregistered;
their destination names above do not authorize refusal stubs.

**Repo transfer tools (issue #137).** Before this delivery those two names
remained absent; after it real typed `repo_pull` and `repo_push` join the
nineteen implemented tools. All prior registrations, discovery/cache hints,
modern/legacy agent-only attribution, note lifecycle, review dialogs and global
exclusion remain. The twenty-one-tool count binds `RegisterTools`, not arbitrary
SDK servers. Both tools select one configured `remote` and optional SQL `user`;
passwords come exclusively from the executing owner. They expose no author,
SQL, force, global target or direct resolution-object argument. Compatible
divergent pull commits as the requesting agent. Human-confirmed choices commit
the entire merge as `user`, retaining earlier row provenance in history.

Modern pull conflict review presents one JSON-choice form per conflict. After
nine forms it returns `nextCursor`; call `repo_pull` with `cursor` and the same
remote/user to reach the remainder. Genuine legacy clients receive all remaining
conflicts in one form. Every choice remains pending until all are present and
validated; no truncation or partial promotion. Each state has a two-minute
expiry, is stored only by a hashed random lookup in process-local SQLite, and
binds repository/client/action, exact local/remote heads, input, displayed
conflicts, position and prior choices. Consumption precedes interpretation.
Missing, forged, replayed, expired, mismatched, declined or canceled responses,
restart and pre-promotion response/state failures cannot merge. Unsupported form
capabilities or absent attribution return a CLI remedy. Final `Pull` re-fetches
and revalidates both heads; a later local write needs fresh review. No dialog
holds a transaction or uncommitted merge on main. Confirmed result-plus-error
responses remain structured; transport uncertainty never causes replay.

### 11.2 CLI

**Code-only M5 surfaces (issue #138).** Before this issue there was no `code`,
`locate` or `eval locate` command. After it, `code index|status|rm`,
`locate <query> [--limit N] [--rerank] [--no-refresh]` and
`eval locate [--golden file] [--k N] [--rerank] [--min-rerank-score F]` all
accept `--dir`/`--json` and implement §9's local-only behavior. Status creates
nothing; index reports complete chunk effects with later vector errors.
Eval preserves every outcome and exits nonzero for a missed match or a failed
explicitly floored rerank safety check. Default no-floor leakage is reported.
Existing `index` still manages durable-memory vectors, and all other commands
retain their routing/semantics. The three hosts' core skills remain; real
locate/eval-locate templates and two OpenCode command entries are additive.

Cobra; every memhub subcommand maps (full disposition in §12). New/renamed: `memdolt pull|push|repo status` (replaces `sync *`), `memdolt review` (same verbs; diffs rendered from proposal branches), `memdolt history <fact|decision|state|arch> <ident>` (`<ident>` names the fact/decision; `state`/`arch` take none — the narrative table itself is the subject), `memdolt hub init|status` (hub bootstrap + doctor), `memdolt import --from-memhub <export.json>`. Dropped: `sync adopt`, `export`/`import` JSON as the sync path (kept only for interop/migration), `wrapup-policy`-style multi-binary — single binary.

**Linux hub surfaces (issue #147).** Before this slice, the `hub init|status`
mapping above was planned. After it, `hub init --output <absolute-local-dir>`
generates a nonsecret bundle using explicit Linux installation/network options;
`hub status --config <absolute-hub.json>` reports observed deployment health.
`hub preflight` and `hub ready` are the generated systemd startup checks. All
support `--json`; none initializes a local store or implicitly installs a hub.
The [deployment runbook](../hub-deployment.md) specifies native 1.88.1, credential
setup, applied nftables enforcement, preservation, supported platforms and the
complete structural blast radius. Existing CLI/MCP/transfer behavior stays.

#### Native memory history (issue #173)

Before this delivery, §11.1's `history` tool and §11.2's top-level history CLI
were deferred. #169's `state history` / `arch history` already listed appended
versions still present at committed main, ordered by stored creation time/id;
their display DTO deliberately collapsed nullable prose to an empty string.
After #173 those listings keep their behavior, while a separate native history
surface preserves all nullable cells and reports actual additions, edits and
deletions from Dolt's commit graph. It does not derive a timeline from stored
timestamps or old rendered files.

CLI selection is `memdolt history fact <id>`, `history decision <id>`,
`history state` or `history arch`, with `--dir`, `--json`, `--limit <n>` and
optional `--as-of <full-Dolt-commit-hash>`. Fact/decision selection requires one
explicit, nonblank UTF-8 row id of at most 26 characters, compared exactly as
stored; it never resolves keys or titles. Narrative subjects take no id.
MCP `history` has required `subject` and optional `id`, `as_of`, `limit`.
Omitting the limit uses 25; explicit positive limits accept 1..200000. Zero,
negative, excessive, malformed and contradictory selections refuse. CLI and
MCP explicitly empty as-of selections refuse too.

The result is identical across direct CLI, authenticated owner and MCP:

| Field | Exact meaning |
|---|---|
| `subject` | `fact`, `decision`, `state` or `arch` |
| `id` | Requested fact/decision id; omitted for state/arch |
| `mainCommit` | One captured immutable committed main hash |
| `revision` | That main hash, or the verified selected ancestor hash |
| `current` | Complete row at revision, or JSON null when absent |
| `blame` | Native last-change commit for current, or null when current is absent |
| `changes` | Array of native parent-to-commit row differences; empty is `[]` |

Every change contains `commit`, `parent`, `type`, `from`, `to`. The type is
`added`, `modified`, or `deleted`. From is null for an addition; to is null for
a deletion. All existing image columns are present, even when a cell is NULL.
SQL NULL maps to JSON null; other cells are exact native strings, including
DATETIME's stored `YYYY-MM-DD HH:MM:SS` form. There is no whitespace, actor,
timestamp or empty-string normalization in this reader. Invalid UTF-8 refuses
instead of allowing JSON's replacement-character conversion. The image columns
are exactly the reached fixed-schema columns:

- Facts: `id`, `key`, `value`, `source`, `kind`, `evidence`, `verified_at`,
  `created_at`, `superseded_by`, and the native generated `live_key`.
- Decisions: `id`, `title`, `rationale`, `summary`, `alternatives_rejected`,
  `evidence`, `status`, `source`, `decided_at`, `superseded_by`.
- State and architecture: `id`, `body`, `actor`, `actor_raw`, `created_at`.

`commit` and `blame` use the same object: `hash`, `author`, `authorEmail`,
`authorDate`, `committer`, `committerEmail`, `date`, `message`. These are read
from the native DOLT_LOG columns, with nullable metadata preserved and native
dates encoded as RFC3339 timestamps with their observed offsets. Neither row
`source` nor narrative `actor` is substituted for native author/committer.
Importing a source row with a 2020 timestamp creates a real import commit at
import time; history reports that native commit plus the old stored metadata,
not manufactured old commits. A deleted current row still returns its changes
with current/blame null; a never-present id returns null/null/`[]`.

Changes preserve native DOLT_LOG iteration order. Within a commit, native parent
order comes first, then row id and native diff type. A merge can show differences
against each parent, each explicitly identified by `parent`; the same row may
therefore appear on more than one merge edge. The limit counts change rows,
including those edges, not commits or appended versions. State/arch changes
span the whole table. Their current/blame selection is the newest stored
creation time, then newest id, at revision; dated rows precede undated ones.
This selection does not imply that the largest stored timestamp is a native
commit time. Human CLI output labels history subject, captured main, selected
revision, current and native blame, then prints each complete change object;
missing values are `null` and an empty timeline says `no changes`.

Only a full lowercase 32-character Dolt hash in captured main's verified native
ancestry can select historical contents. Names, abbreviated hashes, WORKING,
unknown hashes and non-ancestor commits refuse before their rows are read.
An unaccepted proposal or tag target does not become readable by naming its
hash. A commit which is actually an ancestor remains eligible even if another
ref also names it. The selected revision's history and blame are restricted to
its own native ancestry. Native parent membership is verified too; no app-owned
DAG, audit ledger, traversal cache or general ref/SQL interface is introduced.

The opened repository must have the current committed schema. Historical
subject tables use the existing fixed column contract, which already existed
in schema 1; later proposal/note migrations need not exist at the selected past
revision. Every subject-table shape throughout selected ancestry is checked
before returning contents, even beyond the requested output limit, so an
incompatible intermediate schema cannot be hidden by a later repair. Missing
tables are empty diff sides; an absent selected table refuses. Changed/missing
columns, unsupported types and changed fact live-key generation refuse with
the offending revision, without migration or invented images. Native diff
warnings also refuse: incompatible historical primary-key identities can
otherwise return no changes despite matching visible column types. These checks
validate the reached column shape and the fact expression, not every historical
table, index, constraint, foreign writer or arbitrary native SQL operation.

History serializes with this Store's cooperating writers, captures main once,
and reads only immutable qualified revisions thereafter. A foreign main movement
does not relabel that captured revision as the new main; foreign Dolt processes
do not participate in this mutex. The dedicated SQL session is retired after
the operation because revision-qualified blame changes native session database
context. Cancellation, schema/policy refusal and loss of an owner reply return
no partial contents and never replay a request. Later ordinary reads/writes on
the shared handle retain their existing semantics. CLI checks for an existing
store before Open and closes it before successful output. The typed operation
never initializes/migrates, commits, changes main/proposals/tags/working/staged
roots, writes config/derived indexes/rendered files, or flushes MCP notes.
Global-marked stores refuse History and neither CLI nor MCP offers a global
selection; this is the existing explicit global-store marker boundary, not
filesystem classification of arbitrary raw New calls.

Complete changed-element inventory and retained behavior:

- New `internal/store/localdolt/history.go`: `HistoryOptions.Validate` owns
  these four subjects, exact id selection, full-hash syntax and positive limit.
  `HistoryCommit`, `HistoryChange`, `HistoryResult`, private `historyLogEntry`
  own the response/native metadata. `Store.History` delegates to private
  `history`; only the private method's after-blame callback supports native
  cancellation tests. `historyLog` reads native ancestry/metadata/parents;
  `historySchema` checks each reached subject shape; `historyDiff` reads bound
  native parent-to-commit differences with materialized TEXT and refuses native
  warnings; `historyText` rejects lossy UTF-8 output. All are additive. Their
  restrictions apply to this operation and its callers, not every reader of
  their kind.
- Reused unchanged localdolt seams: `initializedMainConn`, `committedMainConn`,
  `handle`, `branchHead`, `proposalMu`, `transferHash`, `transferTables`,
  `transferColumnShape`, `transferLiveKey`, `pullColumns`, `pullProjection`,
  `pullRows`, `quoteIdentifier`, `interopValue`, `discardConn`, and the native
  DOLT_LOG / dolt_commit_diff / revision-qualified dolt_blame primitives.
  Existing repository/identity/schema guards, nullable SQL scanning, owner lock
  and session retirement semantics remain. `repoDiffRows`'s existing CONCAT/CAST
  materialization pattern is reused without changing that symbol or its callers.
  Ordinary `LastChanged`/`lastChanged`, Store.Query, note/narrative readers,
  interop, transfer/review and retrieval scoring retain their prior contracts.
- `internal/storeipc/operation.go` adds only `opHistory`, one typed Backend
  method and its explicit read-only handleOperation case. New
  `internal/storeipc/history.go` implements `OwnerStore.History`: validate before
  JSON encoding, submit once and clear an unsuccessful result. Existing
  authentication, decoding, response bounds, no-fallback/no-replay behavior,
  every other operation and the raw Store interface remain unchanged.
- New `cmd/memdolt/history.go` / `newHistoryCommand` own the four child commands,
  selection/defaults, existing-memory preflight, direct/owner routing,
  close-before-output and full nullable human/JSON output. `root.go` adds only
  that family. `storeFlags.open`/`bind`, `RequireExistingTransferStore` and
  `emit` are reused unchanged. Existing command flags, appended narrative
  listings and writer routing remain.
- New `internal/mcpserver/history.go` / `historyInput`, `historyTool`,
  `Toolset.history` own the typed tool, defaults and nullable row-map output
  schema. The already-present SDK jsonschema dependency supplies inference;
  only this output's map type is overridden to allow null rows and nullable
  string cells. `tools.go` / `registerTools` adds exactly one registration.
  Before, RegisterTools had 22 names; after it has 23. All earlier inputs,
  outputs, handlers, registration names, review/session queues, middleware,
  cache hints and retrieval scoring remain unchanged.
- New localdolt `history_test.go` owns real four-table evolution, deleted and
  missing rows, NULL/empty distinctions, limits/order, schema-1 and as-of values,
  native author/committer/date distinction, actual memhub import metadata,
  merge-parent evidence, long Unicode text, unsupported historical schema/text
  and incompatible native primary-key warnings, dirty/staged/dirty-DDL and
  unaccepted tag/proposal refusals, native hash preservation, canceled-after-blame
  and subsequent ordinary writes. It reuses
  existing disposable store/native snapshot/interop fixtures unchanged.
- New `cmd/memdolt/history_test.go` compares full direct and live-owner outputs,
  past/current values, the default 25 and explicit limits, human/JSON absence,
  existing-memory/refusal preservation, dirty roots/proposals and local
  configuration/artifact sentinels. New `storeipc/history_test.go` compares
  full native/owner outputs and exercises loss without replay, UTF-8 preflight,
  roots and subsequent owner writes. New `mcpserver/history_test.go` exercises
  modern/legacy protocol output validation, nullable images, direct parity,
  past values/default limits, missing ids, unaccepted revisions and unchanged
  queued notes through later ordinary reads and shutdown.
- `mcpserver/tools_test.go` updates only the exact discovery/deferred-name
  expectations. `cmd/memdolt/serve_test.go` updates only the production stdio
  discovery count from 22 to 23. `cmd/memdolt/host_templates_test.go` stops treating
  history as deferred; host templates/registrations themselves do not change.
  README, this PRD, AGENTS and `mcpserver/instructions.md` retain explicit before/after
  records, tool count and usage, including native versus appended/imported
  history. No dependency graph, durable schema/migration, global lane, Git-file
  history, token accounting, installation or live-store change is introduced.

Required verification is process-local CGO_ENABLED=1 and
GOFLAGS=-tags=gms_pure_go: `go build ./...`, `go vet ./...`, `go test ./...`,
`gofmt -l .`, `golangci-lint run`, and
`go test ./cmd/memdolt ./internal/store/localdolt ./internal/storeipc ./internal/mcpserver -run 'Test.*History' -count=1`.
Native tests use disposable databases; they establish no physical hub/client
acceptance or complete M5 parity. Frozen golden assertions remain unchanged.

#### CLI file bodies (issue #171)

Before this slice, `note add [text]`, `state set [body]`, `arch set [body]`
and `opencode wrap-up-note <current-session-id> [text]` accepted an argument
or, when omitted, stdin. After it, each also accepts `--from-file <path>`.
An explicit body argument and that flag conflict, including explicitly empty
arguments/flag values. An empty file flag refuses; omitting both sources keeps
the existing stdin workflow. Absolute paths work and relative file paths use
the invoking process cwd, independently of the repository selected by --dir.
Explicit files outside that repository and generated narratives under either
`.memdolt/rendered` or `.memhub/rendered` remain usable sources.

Only the new file route requires a regular, nonblank, valid UTF-8 source of at
most **65535 raw bytes**, inclusive, before the existing writer's TrimSpace.
The read is bounded to that limit plus one overflow-detection byte. Selection,
canonical resolution, opening, identity verification, reading and closing all
finish before memory opens; any failure refuses without changing memory,
configuration or missing targets. Source content is never put in file errors.
Successful input still goes through the attributed one-note/one-narrative-commit
operation. Deny/schema checks, real producer timestamps, append-only narratives,
confirmed hashes/identities on late errors and unknown-outcome inspection without
replay remain. OpenCode verifies the supplied session after body selection and
before opening memory, retaining exactly Session.Info provenance and fixed
`agent:opencode` / raw `cli`; no caller-supplied provenance override is added.

**Tagged limit disposition.** The v0.2.0 baseline's argument/file selection and
invocation-cwd file resolution are ported; Memdolt's existing stdin fallback is
retained even though the tagged helper lacks it. Tagged note limits of 4096
characters and narrative limits of 65536 characters are not adopted as new
legacy writer restrictions. The already-shipped Dolt TEXT columns have a byte
capacity: the issue's native default-STRICT_TRANS_TABLES investigation preserved
ASCII and multibyte UTF-8 at 65535 bytes and refused 65536/65538 bytes. The new
CLI tests repeat those accepted bytes and refuse overflow before native SQL.
The file cap respects that capacity before normalization; ordinary
argument/stdin/shared/MCP writers gain no new raw byte or character limit and
keep native SQL enforcement after their existing normalization. This does not
change SQL modes, columns or migrations.

Complete changed-element inventory and preservation scope:

- New `cmd/memdolt/body_file.go`: `bodyFileLimit` owns the file-only byte cap;
  `bindBodyFile` adds one flag and shared help to just the four commands.
  `readBodyFile` canonicalizes the explicit source from cwd, captures its
  regular-file identity, opens its parent as os.Root, opens the basename within
  that root and compares the opened regular file before content read. Explicit
  symlink paths resolve to their selected canonical file; replacement identities
  and rooted escapes refuse. `checkBodyFileOwner` opens only existing selected
  repository/metadata directories, checks their identities and rejects linked
  metadata before calling the existing owner guard. Missing metadata means no
  known owner file; unresolvable repository/metadata identities fail closed.
  `readBodyFileContent` bounds the read, reports read/close failures, validates
  bytes/nonblank text and returns the original body for writer normalization.
  Root close failures also erase the returned body. These helpers bind this
  named CLI file input, not every reader, a repository-only allow-list, copied
  credentials or foreign filesystem changes after identity checks.
- `internal/layout/source.go`: only CheckOwnerSource's caller-scope comment
  changes. Before #171 the guard served document/config/code-index readers;
  now the CLI file reader also calls it. Its implementation still compares the
  opened source against the selected `.memdolt/server.pid` opened for Stat only,
  refuses the owner file and path/hard-link aliases before source content read,
  and fails closed on unverifiable owner identity or close. No owner credential
  contents are read by this guard. Existing document and code-index policies,
  global document guards and other file readers remain unchanged.
- `cmd/memdolt/lanes.go`: `bodyArg` gains the repository argument and explicit
  flag-presence selection/conflict check; its original argument and stdin reads
  remain. `newNoteAddCommand` and `newNarrativeSetCommand` bind the flag and pass
  --dir into that selector, retaining args, actors, closures and outputs.
  `cmd/memdolt/opencode.go`: `newOpenCodeWrapUpNoteCommand` does the same with
  args[1:] so the session ID remains separate. `logOpenCodeWrapUpNote` and
  `opencode.VerifySession` keep their verification/open ordering and exact
  metadata mapping. `storeFlags.run/runStore`, `openCommandStore`,
  `requireCurrentSchema`, `runLaneWrite`, `emitLaneWrite`, `noteInfo` and
  `narrativeInfo` retain routing, schema, close and success/late-error contracts.
- Reached but unchanged writer/binding structure: `Lanes.LogNote`,
  `LogNoteWithProvenance`, `PrepareNote`, `noteStatement`, `noteText`,
  `SetNarrative`, `write` and `ConfirmedWriteError` retain SQL, normalization,
  generated ULIDs/real timestamps, provenance and deny declarations. The owner
  receives body values through existing bound raw Commit requests with concrete
  time.Time bindings; OwnerStore/Client/handler Commit and Query behavior,
  authentication, result envelopes and no-fallback/no-replay rules remain.
  Shared nullable read DTOs and committed note/history reads remain. No path
  enters an owner operation or MCP input. MCP Toolset batching/render behavior
  and all 22 RegisterTools registrations/input schemas remain unchanged.
- New `cmd/memdolt/body_file_test.go`: `bodyFileCommands` supplies the four CLI
  shapes. `TestFileBodiesWriteAndReopenDirectAndOwner` exercises Unicode/multiline
  and ASCII/multibyte 65535-byte bodies, cwd/--dir decoys, absolute paths and
  real owner-process shutdown/reopen, checking complete rows, real timestamps,
  authors and commit counts. `TestFileBodyRefusalsPreserveMemoryAndMissingTargets`
  covers limits/invalid/empty/missing/nonregular inputs and explicit-empty
  conflicts with native root/config and missing-target preservation.
  `TestFileBodyProtectsActualOwnerAndAliasesBeforeReading` uses a real owner's
  published credential plus file/directory symlink and hard-link controls in
  human/JSON modes; no content, row or commit escapes. `TestFileBodyOwnerProtectionFailsClosed`
  covers unverifiable metadata/owner paths. `TestFileBodiesPreserveStdinArgumentsAndRenderedInputs`
  preserves old normalization beyond tagged limits and reads actual generated
  views plus the legacy render location. `TestFileBodyReadCloseFailuresAndBoundedAllocation`
  exercises a native closed handle, the narrow bodyFileCloseFailure close-error
  fixture and bounded read consumption. `TestFileBodiesUnreadableSourcePreservesMissingMemory`
  uses makeBodyFileUnreadable in new `body_file_windows_test.go` (mandatory native
  ReadFile byte-range lock) and `body_file_other_test.go` (chmod refusal, skipped
  only when the test user can still read). `TestFileBodiesRetainDenyGuards` checks
  direct/owner denial with unchanged native roots; `TestFileBodiesLostOwnerReplyNeverReplays`
  commits through the actual authenticated handler then drops each reply, proving
  exactly one submission and reopened effects/provenance. Symlink fixture checks
  skip only where the platform cannot create those aliases.
- Existing CLI regressions: `confirmed_result_test.go` extends
  TestConfirmedOwnerCLILanesRetainLateResults and
  TestConfirmedDirectCLIOutputFailureNamesCommitAfterClose to all four file
  commands, retaining their old argument cases and adjusting only expected
  commits. `opencode_test.go` extends TestOpenCodeCLIRefusesBeforeStoreOpen to
  file bodies with the same failed/malformed/mismatched API cases.
  `lanes_test.go` extends TestDirectAndReviewLanesRefuseAStaleSchema to all four
  file commands. Existing fakeOpenCodeAPI, CLI, native store and owner-process
  fixtures are reused; no live host service, memory, installation or credential
  is touched. No frozen golden assertion or unrelated test suite is changed.
- README adds four invocation examples, input/protection/limit rules and its
  former no-flag statement as before/after. AGENTS and CLAUDE preserve their
  old stdin statement and record the additional selector. The shared
  `templates/skills/memdolt-resources/onboarding.md` and Claude/Codex/OpenCode
  wrap-up templates preserve every former no-flag statement as before/after;
  only optional file selection is added. Their per-item approval, attribution,
  OpenCode identity, render and no-replay obligations remain. This PRD section
  inventories the change; the #169 and #155 records keep their historical
  scope, and the §12 follow-up explicitly dispositions only this slice.

No dependency, durable schema, new MCP registration, global lane,
document-ingestion policy change, other parity feature or full M5 completion.

#### Direct-lane CLI read parity (issue #169)

Before this slice, the shipped direct-lane CLI had state/arch set/show,
note add/list (default limit 20, no actor/day filters) and command record/get.
`Lanes.Notes` and `Lanes.Narrative` queried the working set, while command get
already selected committed main. After it, these commands ship with `--dir`
and `--json` on existing current-schema repository memory:

- `state history [--limit <n>]` and `arch history [--limit <n>]` list appended
  narrative rows still present at committed main, newest `created_at`, then
  newest id. Each entry includes full body, id, canonical/raw actor and creation
  time. Human output carries all fields; JSON uses `{"history": [...]}`.
  Empty history is `[]` (human: `no state history` / `no arch history`).
  The default is 25; explicit limits accept 1 through 200000. This is the
  tagged memhub narrative-row workflow. Before #173, §11.1's separate native
  history/blame/as-of engine was deferred; after #173 it ships alongside this
  unchanged listing. Existing show retains its body-only human form,
  full JSON row and not-found behavior, now over committed main.
- `note list [--actor <stored-actor>] [--since-days <days>] [--limit <n>]`
  now defaults to 25, with the same 1..200000 limit range. Actor matches exact
  stored bytes: no trimming, case folding, writer normalization or matching
  against actor_raw. An explicitly empty filter matches empty actors, not NULL.
  Since-days accepts 0..106751 and binds a UTC cutoff computed from the caller's
  current second minus that many 24-hour days; 0 means since the current second.
  Both filters apply before the unchanged newest-created_at/id ordering and
  limit. Invalid UTF-8, negative/overflowing days and invalid/oversized limits
  refuse. JSON retains all fields and the five nullable provenance fields'
  existing NULL/empty-string convention; human text still collapses whitespace.
  Empty results remain `{"notes": []}` / `no session notes`.
- `command list` returns all recorded kinds by newest last-run time, then enum
  order (build, test, run, lint, other). Each row carries command line, last exit
  and time, success count and failure count. Empty results are `{"commands": []}`
  / `no recorded commands`. Existing command get remains supported.
- `command verify <kind> <cmdline> --exit-code <n> [--actor <actor>]` records an
  observed result, never executes the supplied command. Explicit --exit-code
  is required even for success. Existing `command record ... [--exit <n>]`
  keeps its default exit 0. Both use the same kind-keyed SQL upsert, atomic
  counter increments, actor normalization, commit message and one-commit
  operation; the tagged memhub multiple-command-row model is not introduced.
  Confirmed hashes/identities survive late errors and an unknown owner reply
  retains its inspection remedy without replay. The ordinary command writer
  can commit preexisting dirty working changes; this slice adds no RequireClean
  gate and does not claim the read-preservation policy for command writes.

The note/narrative readers now use `AS OF 'main'`; command get retains that
policy and command list shares it. Each SELECT reads committed main, not dirty
or proposal rows; separate calls do not claim a shared atomic snapshot. These
reads do not change main, branch heads, working/staged data, configuration,
derived indexes or queued MCP notes. The selected CLI reads check the existing
store before Open, so absent memory is refused without creating a database.
Schema/identity/routing validation, ownership, actor/deny/review rules, current
writer outcomes and MCP note batching remain. No global lane, file-input feature,
dependency, durable schema, new MCP registration, doctor/ingest/audit/metrics
feature or full M5 completion is introduced.

Native regression evidence: before the shared reader fix, both direct and
authenticated-owner `note list`, `state show` and `arch show` returned fixture
dirty text; command get correctly retained its committed value. The fixture
pins one native connection while preparing separate pending/dirty controls and
checks the original main hash. A separate max-int limit probe reproduced
`panic: runtime error: makeslice: cap out of range` in the pinned
go-mysql-server `topRowsIter.computeTopRows`: it allocates `limit+1` rows before
reading data. The existing 200000-row IPC ceiling is therefore also the shared
note/history limit ceiling. Larger values refuse before native SQL; neither
the IPC default nor its configurable result bound changes.

Complete changed-element inventory and preservation scope:

- `internal/memory/memory.go`: `Notes` keeps its public signature and delegates
  to new `NotesFiltered`; the former working-set read and unrestricted positive
  limit become committed-main reads with the bound above. `NotesFiltered` owns
  literal bound actor/cutoff parameters and preserves scanning of all note
  fields/NULLs and timestamp/id ordering. `Narrative` delegates to new
  `NarrativeHistory(..., 1)` and retains its not-found result; history reuses
  `NarrativeKind.table`'s two constant identifiers and the existing full row
  shape. New `Commands` and private `commands` share the prior command scan
  with `Command`; get retains kind normalization, not-found and committed-main
  semantics, with complete row iteration/close errors. These restrictions bind
  these named readers and their callers, not all SQL readers. The initial
  #169 slice retained the implementations of `RecordCommand`, commandMu,
  `write`, SetNarrative, note preparation/commit/provenance and every other lane
  writer. The review correction below adapts RecordCommand's representation
  while preserving its SQL/policy; the second correction below similarly
  adapts note/narrative timestamp representation. Other implementations remain.
  Their OpenCode/MCP/owner callers inherit no new write policy or queue action.
- `cmd/memdolt/lanes.go`: new `storeFlags.runLaneRead` reuses
  `RequireExistingTransferStore` then existing run/owner/schema/close handling.
  Only note list, command get/list and state/arch show/history use this new
  pre-open check; ordinary `run`/`runStore` writers and other command families
  retain their former behavior. `newNoteListCommand` adds the two flags and
  default change while preserving human/JSON note output. `newCommandCommand`
  registers list/verify; `newCommandRecordCommand(verb)` constructs both writer
  spellings over unchanged runLaneWrite/emitLaneWrite and the same RecordCommand.
  `newCommandGetCommand` adds only the existing-store read guard; new
  `newCommandListCommand` reuses commandLine/emit. `newNarrativeCommand` adds
  history and explains show/history; `newNarrativeShowCommand` adds the guard;
  new `newNarrativeHistoryCommand` owns limit and full-body history rendering.
  Existing root registration, set/add/bodyArg, actor flags and writer results
  remain. No new stdin/file behavior or output-helper contract is introduced.
- `internal/store/store.go`: adds only shared `DefaultMaxRows = 200000`.
  `internal/storeipc/storeipc.go` changes its existing DefaultMaxRows constant
  to an alias of that value. Store/Backend interfaces, Query validation,
  endpoint allow-list/authentication, time argument encoding, configurable
  MaxRows and no-replay/result rules are unchanged. The constant is not a
  universal Store.Query limit; only the named note/history readers enforce it
  on requested limits, while IPC retains its existing result-set bound.
- New `cmd/memdolt/lane_reads_test.go` owns native fixture rows, history/note
  output/filter/limit checks, dirty/proposal roots, input refusal, missing-store
  preservation and production queued-note checks. New `command_verify_test.go`
  exercises direct/owner counters, attribution, list/get ordering, deny refusal,
  no command execution and a real committed write whose owner reply is lost.
  `confirmed_result_test.go` extends its existing owner late-readback and direct
  output-failure cases to verify, adjusting only expected commit/counter totals.
  `lanes_test.go` adds the new operations to its existing stale-schema refusal
  cases. Existing lane tests/fixtures remain; disposable stores are used and
  no live memory, host installation or frozen golden assertion is changed.
- README's everyday commands, AGENTS' direct-lane record and this PRD section
  retain matching before/after scope, defaults and links. Existing documented
  writer/actor/stdin contracts still hold. Broader §12 parity remains separate.

Before #169's first review correction, the new shared command scanner still
assumed non-NULL exit/time/counters, and converted NULL cmdline to empty. The
supported `testdata/memhub-v1.json` import has a build command with NULL exit
and time and known 9/2 counters. Both CLI reads failed: direct returned
`converting NULL to int is unsupported`; owner returned
`cannot assign NULL to *int`. The native schema/interop contract permits NULL
for every non-key command column, so handling only exit/time would leave
supported rows unreadable. After correction, all five fields preserve SQL NULL
as JSON `null` and human `unknown`. Known strings, timestamps and integers keep
their wire/output values; empty string and zero stay distinct from absence.
Rows without a last-run time sort after rows with a known one, with the same
kind tie-breaker. Before this correction, a later read-back failure reported
nonauthoritative zero-value fields with a confirmed hash/error; after it those
unknown fields are null, with that same confirmed hash/error and
inspection/no-replay remedy.

The correction's complete additional inventory and scopes:

- `internal/memory/memory.go`: `Command.Cmdline`, LastExitCode, LastRunAt,
  SuccessCount and FailCount become nullable pointers, without omitempty.
  The private `commands` scanner uses existing sql.NullString/NullInt64/NullTime
  support on direct and owner routes, then fills pointers only for known
  values. `Command`/`Commands` inherit the correction and retain their filters,
  ordering and committed-main policy. `RecordCommand` supplies its former
  scalar SQL arguments from local variables instead of result fields; its
  statements, 0/1 increments, normalization, deny text, mutex, attribution,
  commit/read-back ordering and unknown-outcome handling remain. SQL NULL
  counters still remain NULL when incremented; no COALESCE or invented totals
  are introduced. Other memory lanes remain unchanged.
- `cmd/memdolt/lanes.go`: `commandLine` renders only absent values as unknown,
  retaining the previous human form for known values. `commandInfo` and the
  get/list/record/verify JSON payloads inherit the shared nullable fields.
  No command, flag, input, writer, close or error-emission policy is added.
- Existing `mcpserver/tools.go` commandOutput/commandWriteOutput,
  `storeipc/operation.go` recordCommandResult and `storeipc/owner_store.go`
  OwnerStore.RecordCommand carry the shared type unchanged. The inferred MCP
  get_command and record_command output schemas now admit null for those five
  still-present fields; their input schemas and all 22 registrations remain
  unchanged. Existing owner
  sql.Null* scanning and typed JSON transport suffice, so no IPC operation,
  protocol, authentication or retry policy changes. This first correction left
  OpenCode note handling, MCP notes/queue and other output types unchanged;
  the second correction below changes only the note timestamp representation
  among those types, preserving producer and queue policy.
- `cmd/memdolt/command_verify_test.go` adds actual legacy fixture import and
  native all-NULL/mixed-field export/import reads through both routes, checks
  full JSON/human distinctions and preserved roots, then checks known legacy
  counters and existing NULL-counter arithmetic after verification. Its
  ordinary writer/list tests, `lane_reads_test.go`, `lanes_test.go`, CLI and
  localdolt `confirmed_result_test.go`, and `storeipc/storeipc_test.go` adapt
  their existing value/comparison assertions to pointers without relaxing
  counter, concurrency, dirty-data or confirmed-result expectations.
  `mcpserver/tools_test.go` adds real protocol checks for both inferred nullable
  output schemas, unchanged registration names, NULL get/record results and
  known 0/1 writer counters. Existing import fixtures and import/transfer
  validators remain unchanged; all stores are disposable.
- README, AGENTS and this PRD retain the before/after correction and explicitly
  distinguish durable schema preservation from the two changed MCP output
  schemas. No dependency, migration or writer policy is added.

Before #169's second review correction, the narrative and note scanners still
required non-NULL creation times, and the note scanner additionally required
non-NULL text. The existing `TestInteropCLIReexportPendingDecision` native
fixture imports both narratives and notes without creation timestamps, while
the same native schema/import contract admits NULL in every non-key column of
those tables. Native all-NULL/mixed export/import regressions reproduced direct
`unsupported Scan, storing driver.Value type <nil> into type *time.Time` and
`converting NULL to string is unsupported`; authenticated owner reads refused
the corresponding NULL assignments. After correction, unknown creation times
remain JSON null and human note/history output prints unknown. Known timestamp
wire values remain unchanged. Dated rows come first, then undated rows with
the same descending id tie-breaker; since-days excludes unknown dates. Show
retains its body-only human output, including for undated narratives.

All allowed nullable fields were checked: narrative body/actor/raw actor and
note actor/raw actor/provenance already used the existing NULL-to-empty-string
presentation. They retain it; nullable note text now uses the same convention.
Optional empty provenance fields remain omitted in JSON. SQL NULLs remain in
the database unchanged by reading; this presentation is not an interop rewrite.
Only creation timestamps need a new nullable DTO representation here; command
nullability remains as recorded above, and unrelated task/fact readers are not
part of this correction.

The second correction's complete additional inventory and scopes:

- `internal/memory/memory.go`: `Note.CreatedAt` and `Narrative.CreatedAt` become
  nullable pointers without omitempty. `NotesFiltered` scans text through
  sql.NullString and creation time through sql.NullTime; `NarrativeHistory`
  scans creation time through sql.NullTime. Existing Notes/Narrative wrappers
  inherit the fix. Their committed-main selection, limits, ordering, filters,
  prose/provenance presentation and error/close handling remain. New prepared
  notes and SetNarrative results still use the same real second-resolution
  UTC now(). `noteStatement` unwraps known times to time.Time for existing IPC
  encoding (an absent DTO timestamp binds NULL); SetNarrative binds its known
  timestamp value. SQL, text declarations, actor rules, note provenance checks,
  clean batch guard, commit/result behavior and every normal producer's known
  timestamp remain. No new writer validation or queue policy is introduced.
- `cmd/memdolt/lanes.go`: new nullableStamp is used only by note listing and
  narrative history; existing stamp and other human output stay unchanged.
  noteInfo/narrativeInfo and their list/show/write JSON inherit nullable
  createdAt. State/arch show retains body-only human output. CLI inputs,
  direct/owner selection, missing-store guard and write/error reporting remain.
- Existing `mcpserver/tools.go` noteOutput inherits the shared Note field:
  log_session_note's inferred output schema admits null for note.createdAt,
  while prepared outputs still contain a real timestamp. Its text-only input,
  all 22 tool registrations, grouping/timer/flush/discard semantics and the
  prior command output schemas remain. Existing owner raw Commit/Query and
  typed time/NULL encoding suffice; no IPC implementation or protocol changes.
  OpenCode's existing noteInfo output also inherits the field, with unchanged
  verified identity/provenance and real producer timestamps. The standalone
  renderer's separate snapshot types/readers remain untouched.
- `cmd/memdolt/lane_reads_test.go` adds native all-NULL/mixed-field export/import
  assertions through direct/live-owner routes, checking complete DTOs, filters,
  ordering, undated show, preserved roots and known writer/read-back times.
  Its existing seeded-time fixtures adapt to pointers; its production MCP
  queued-note check additionally verifies the prepared timestamp survives
  orderly flush/reopen. `cmd/memdolt/opencode_test.go` replaces pointer-identity
  comparisons with full value comparisons, retaining its exact single-note
  and batch/provenance assertions. `mcpserver/tools_test.go` expands/renames the existing nullable
  output check to TestLaneNullableOutputsThroughMCP, retaining command checks
  and adding the note timestamp schema and actual queued output. Existing
  native import fixtures, validators and other assertions remain unchanged.
- README, AGENTS and this PRD retain matching before/after and field scopes.
  No migration, dependency, file-input or unrelated memory-reader work is added.

**Trusted human repository facts/decisions (issue #139).** Before this slice,
§12's CRUD port and M5 deferral still included the ordinary human commands.
After it, these commands ship with `--dir` and `--json` on an initialized
current repository store:

- `fact add <key> <value> [--source <source>] [--kind <kind>] [--evidence <text>]`
- `fact list [--prefix <literal-dotted-prefix>] [--limit <n>]`
- `fact verify <id-or-key>` and `fact supersede <old> --by <new>`
- `decision add <title> --rationale <text> [--summary <text>] [--source <source>]
  [--alternatives <text>] [--evidence <text>]`
- `decision list [--status all|active|superseded|draft] [--limit <n>]`
- `decision set-summary <id> <summary>` and `decision supersede <old> --by <new>`

The existing CLI actor default is `user`; these writers also accept `--actor`
only when it normalizes to that human class. This is a trusted CLI policy,
not authentication that a local process is a person. The owning-store seam
validates both the normalized and raw actor; MCP identities remain agent-class,
including raw `user`. Source is separate metadata, defaulting to `user`, with
the tagged vocabulary `user`, `git`, `observed`, `agent:<id>` and
`user+agent:<id>` (lowercase ASCII letters/digits/dots/underscores/hyphens).
Source values retain their text, including accepted surrounding whitespace;
the actual stored value must fit VARCHAR(64). No source, actor flag or remote
SQL user enables a direct human MCP operation. Agents continue to stage claims
and humans review through the unchanged contradiction/elicitation guards.

Fact add requires a dotted key with no empty segment or surrounding whitespace.
It inserts a fresh ULID if no live row exists, even when superseded rows use
that key. Otherwise it updates only the live row, retaining id and created_at,
and replaces value/source/kind/evidence while stamping verified_at. Omitted
kind and evidence clear to NULL. Blank kind or decision summary normalizes to
NULL; every nonblank summary/tag preserves its whitespace, matching memhub
v0.2.0. Confidence stays removed. Old values remain recoverable through Dolt
history; commit messages contain operation plus id, not duplicate old prose.
Fact verify changes only verified_at, and may explicitly select a superseded
row. Verify/supersede resolve one exact unambiguous fact id or key over all
rows; a key shared by live/history rows requires an explicit id. Add's live-key
lookup is deliberately different. Database-collation spelling collisions
refuse instead of choosing another key.

Supersession links existing same-kind rows, retaining both; decisions also
set the old status to `superseded`. It validates the replacement chain before
writing, refusing self-links, cycles, missing rows and dangling/cyclic target
chains. A valid explicit relink can repair an old link; it never clears a link,
deletes a row or resurrects one. Decision add writes active status and the real
source/summary/alternatives_rejected/evidence fields; set-summary changes only
summary. Identical summaries and links report `unchanged` with no commit. So
does an otherwise identical fact assertion or verification whose stored
timestamp already equals this second: the existing DATETIME has whole-second
precision, and no future timestamp or empty audit commit is fabricated.

Fact list preserves superseded rows, literal dotted-prefix escaping and
recall's configured stale horizon. CLI decision list defaults to all statuses,
as the tagged CLI dispatcher did; `--status active` filters it. The shared
reader lets MCP retain its existing active default and ten-row default limit.
CLI limits default to zero (all matching rows). Every list reads committed
main, excluding proposals and dirty data. The existing nullable display shapes
remain; SQL NULL fields are preserved in storage. Value/summary edits change
the existing semantic-text hash, so old vectors are ineligible as current
vectors; production recall uses its visible stale-index fallback, and
`index status/rebuild` reports/repairs the derived data. Render uses the current
committed text and retains superseded rows and real commit history.

Each of the six human Store mutations executes once in the owning process
under `proposalMu`, sharing existing direct/proposal/transfer/document/render/
status ordering. It validates current committed schema including the precise
STORED live_key expression, refuses dirty main and active merge state, binds
all user SQL values, scans newly persisted text/source/raw and canonical actor/
links, rechecks captured main and uses `CommitRequest.RequireClean`. Invalid
UTF-8, field widths, configuration, scan and SQL failures refuse visibly.
Foreign Dolt sessions do not share the mutex; no coordination of their final
read/commit interval is claimed. Owner discovery/authentication failures never
fall back. A lost response is not replayed: inspect lists before retrying.
Confirmed identity/hash evidence remains in human/JSON results on later
connection, transaction, owner or output errors.

Before #139, `commitConn` discarded its result on outer transaction failure
and its comment claimed all failures rolled back. The pinned DOLT_COMMIT
procedure persists before database/sql finalization. After this fix, that
helper retains the real hash with a finalization error naming it; earlier
failures still invoke rollback. The new human seam preserves the structured result.
Before #145 older lane/document/raw owner wrappers still discarded structured
results on error, and note-batch retry bookkeeping remained follow-up work.
After #145 those consumers retain confirmed effects and distinguish unknown
outcomes under §3.1. Staging keeps
its prior late-error expected-head refusal and inspectable proposal residue;
it does not gain automatic deletion from the newly available hash.

The structure touched is additive root registration and new CLI
`human_memory.go`; new localdolt `human_memory.go` types and six methods;
`documents.go`'s `documentConn` renamed/generalized to `initializedMainConn`
without changing its document lifecycle; `localdolt.go`'s `commitConnFinalize`
result preservation; `propose.go`'s retained cleanup policy and fact-only
supersede comment; new `memory/records.go` with the extracted list types/queries
and staleness comparison, `retrieval/recall.go` delegating to that same comparison
without changing scores, and `mcpserver/tools.go` delegation;
`storeipc/operation.go`'s Backend/explicit
allow-list and new `storeipc/human_memory.go` typed operations; four new test
files for CLI/localdolt/storeipc/MCP; server instructions and three wrap-up
templates; and the AGENTS/PRD records. The human restrictions bind those six
methods through `humanMemoryWrite`, not all Store writes or all source fields.
Existing children, nineteen MCP tools, schemas, proposal/review guards,
attribution, notes, retrieval scoring, document behavior and other owner
operations retain their contracts. AGENTS.md gives the complete per-file
before/after inventory. No dependency, migration, global flag/promotion,
top-level history/status/stats command or full M5 completion is added;
`repo status` is already shipped and remains a separate surface.

After integrating #137, those nineteen tools remain alongside the two real
transfer tools, for twenty-one registrations; the human-only mutation boundary
and shared list defaults remain intact. The integrated CLI regression creates
same-row and same-live-key fact conflicts through human `fact add`, resolves
all pull choices directly and through a live owner, then reopens retained
fact/decision fields and exercises verification, summary edits and supersession.
The #139 generic confirmed-commit result and failed-stage residue policies
remain unchanged beside pull's own promotion/finalization handling.

**First M4 subset (issue #123):** before this issue, no `repo` command was
registered. After it, `memdolt repo status [--dir <repository>] [--json]`
reports the resolved local Dolt store path, `main` commit hash, schema version,
clean/dirty main working set with each changed table's staged flag and Dolt
status, and pending proposal counts by repo/global target. These are the
global-target proposals in this repository, not a read of the global store.
The JSON report is one object with `localOnly: true`, `store`, `mainCommit`,
`schemaVersion`, `clean`, `changes` (an empty array when clean), and
`pendingProposals` (`repo` and `global`). Human output labels the same report
local-only and states that remote state was not checked.

Status uses the existing local-store/authenticated-owner route and reads main's
working set explicitly, without checking out a proposal branch. Pending counts
reuse the review list's reachability rule: unchanged merged branch residue is
excluded, while unmerged proposals remain pending. The command performs no
staging, commit, reset, review mutation, migration, or hub/remote request. It
refuses a missing database before opening it and names `memdolt init`; existing
stores retain the migration/upgrade guards and visible probe, authentication,
read, output, and close failures. A direct read may briefly create or update the
local ownership lock, as other direct opens do (§5.2); no durable memory or
working-set contents change. Local observations do not establish whether a
remote is configured, reachable, current, synchronized, or absent. After #123,
remote status/diff (including `repo status --diff`), transfers, conflict dialogs,
and hub setup remained unshipped. Issue #125 adds the clone transfer below;
the other operations and the full M4 scope and exit gate in §16 still apply.

**M4 remote inspection subset (issue #131):** before this slice, the #123
status contract above was the default and the #129 slice below still left
remote status/diff pending. After it, `memdolt repo status [remote] [--local]
[--diff] [--user <sql-user>] [--dir <repository>] [--json]` inspects configured
origin by default; a single optional name selects another configured remote.
Missing origin produces a local `no-remote` result and `repo remote add`
remedy. An explicitly missing remote is an error. `--local` preserves the
former offline report, performs no remote/configuration request, and is
incompatible with a remote operand, `--diff` or `--user`. Invalid combinations
name command help. All prior local fields, pending reachability filtering,
repo/global count meaning, existing-store guards and visible failures remain.

One `Store.RepoStatus` operation captures local committed main, its schema,
main working/staged table changes and pending counts under the owning Store's
existing `proposalMu`. It refreshes validated native remote configuration and
reuses the transfer credential contract: explicit SQL user overrides the
validated stored user, absence of both means anonymous, and only the executing
owner's `DOLT_REMOTE_PASSWORD` supplies a password. Caller passwords never
cross IPC. Unsafe URLs/parameters refuse without echoing credentials; personal
Dolt credentials are never loaded. The existing main-only fetch arm of
`transferProcedure` reads committed remote main and updates only the selected
tracking ref. File sources remain preserved; no tags, proposals, other refs
or protocol stdout are replaced or used for fetch progress. Fetched objects
and that tracking ref may remain after success or refusal. Authentication,
network/read, missing-main and incompatible-schema failures stay visible;
none is reported as current or no-remote.

Ancestry compares the captured immutable hashes: equality is `current`, a
contained remote is `ahead`, and a contained local is `behind`. True divergence
is `diverged-mergeable`, `conflicted`, or `diverged-unassessed`. Dirty divergence
retains its working/staged content and explains that clean main is required
for merge assessment. Unassessable ancestry is an explicit refusal with a
manual-inspection remedy. Hashes identify observed snapshots, not a guarantee
that a remote remains at that head after the fetch.

`statusSchema` retains transfer's exact column/type/generation checks and adds
the known unique/FK constraints for status alone. `previewRepoMerge` requires
clean main without an active merge/conflict and identical validated DDL at
base/local/remote; schema changes or unknown constraints refuse assessment.
It performs an actual no-commit/no-fast-forward merge of the captured remote
hash in a transaction. It reads both Dolt conflict surfaces, attributes and
verifies violation records, and clears only already-satisfied records inside
that transaction. It also checks all maintained unique indexes and the
document FK against the merged rows, even if no surface reports a violation.
It always rolls back, including on cancellation, and verifies main hash,
working/staged roots and clean merge/conflict state afterward. Rollback or
restoration failure is a visible refusal with an inspection remedy, never
successful read-only inspection. No merge/genesis/migration commit is made.
Review's existing allow-list and promotion protocol remain unchanged: the
extracted `verifyMergeViolations` accepts the status caller's separate validated
table/key list, while `reviewViolations` still enforces its prior review list.
`requireConstraintHolds` retains its checks/error text and now distinguishes
proven duplicates from read/unknown failures internally. No contradiction
inference, promotion deny-list scan or automatic conflict resolution runs here.

`--diff` reports actual Dolt differences FROM captured local main TO captured
remote main, ordered by table then primary key and classification. Each changed
table includes its rows and any changed before/after DDL. Row types are
`added`, `modified` and `deleted`; existing row images contain every column as
an SQL-rendered string or explicit NULL, and an absent image is omitted.
Revision-qualified diff tables use the validated committed columns, so even
dirty local DDL cannot substitute working or proposal data. Ordinary status
does not include row bodies. A missing remote or unvalidated incoming schema
gets no claimed diff.

CLI `repo.go`, authenticated `storeipc.Backend`/operation/`OwnerStore`, and MCP
`repo.go` delegate the whole inspection once to this shared store method.
Owner discovery still fails closed; a lost reply is never resubmitted and
names inspection before retry. `runRepoStatus` now closes before success
output. `PendingProposals` retains its behavior through an extracted
connection-capable reader. The mutex now includes status; all earlier
participants, including #133's render, retain their semantics, while other
reads/migrations and foreign Dolt processes remain outside.
No derived/index/render/configuration file is changed. The matching AGENTS
record names every changed file/shared seam and the before/after limits.
Local synthetic CLI/direct/owner/MCP tests exercise
ancestry, both conflict surfaces, exact NULL/deletion diffs, preservation,
interleavings, malformed/unsafe inputs, authentication/cancellation and
cleanup/output/lost-response failures. They establish no real-hub deployment,
credential acceptance, version compatibility or full two-machine M4 gate.
No dependency, migration or deferred backend is added.

**M4 clone bootstrap (issue #125):** before this issue there was no `clone`
command. After it, `memdolt clone <remote-url> [--dir <repository>]
[--user <sql-user>] [--json]` acquires an existing committed `main` using the
pinned embedded driver's clone engine. The local database is the established
`<repository>/.memdolt/dolt/memory`, with `origin` and main tracking registered
through Dolt. Success reports `store`, `mainCommit` and `schemaVersion`; JSON
stdout contains one object, or no success object on failure. Dolt progress is
suppressed in both modes. Inspection and close finish before success is printed.

Only explicit absolute `http://`/`https://` remotesapi URLs with a database
path and absolute `file:///` URLs are supported (`file:///C:/path` on Windows).
URL userinfo, queries, fragments, unsupported schemes, malformed ports and
invalid users fail before transfer. File paths may encode spaces, but encoded
percent signs are refused because of the pinned parser's double-decoding
ambiguity. `--user` accepts 1–32 ASCII letters, digits, dots, underscores or
hyphens and reaches Dolt's Basic authentication path; only the process
environment `DOLT_REMOTE_PASSWORD` supplies the password. The password never
enters a URL, argument or persisted configuration. Diagnostics redact its
plaintext, URL-encoded and Basic forms. Without `--user`, authentication is
anonymous; clone loads no personal Dolt credentials. File remotes reject
`--user`. The private-network posture in §13.1 still applies.
Destination paths containing `?` or `%` are refused because they cannot
round-trip reliably through the pinned embedded path parsers.

Clone refuses a held store lock, an existing database or nonempty data
directory before remote contact, and reserves the destination database
exclusively. Managed-path symlinks are refused. Existing repository files,
adjacent configuration and derived indexes survive. Transfer artifacts and an
empty internal Dolt bootstrap configuration in `dolt/.clone-home/` are kept
inside the destination. Failure retains artifacts with an explicit location
and remedy; inspect them and choose a fresh `--dir` to retry. Missing `main`,
missing/invalid memdolt metadata, absent core tables or task/note/proposal columns,
transfer/authentication/cancellation and close failures cannot report success.
An older initialized schema is retained for explicit `memdolt init` with the
same `--dir`; a newer schema requires a newer binary (§6.4). No genesis or
migration commit is added by clone, and no fallback initializes memory.
memdolt submits one clone operation; Dolt may retry transport reads/downloads
internally. Source history, main hash, authorship, row contents and nullable
note provenance remain the remote's, without conversion.

Before the issue #125 cycle-1 fix, the containment statement overlooked a
source-side write: file URLs passed through `GetRemoteDBWithoutCaching` into
the pinned `FileFactory.CreateDbNoCache`, which created missing `oldgen/`
directories even for an empty source subsequently refused by clone. After
the fix, `openCloneRemote` resolves file URLs with the shared `cloneFilePath`
decoder, opens existing NBS files directly, combines old-generation and ghost
readers only when `oldgen` exists, and wraps them with `DoltDBFromCS`. The
clone flow performs reads on those source handles and closes them without
initialization. This binds `openCloneRemote`/clone, not the write-capable
`NewLocalStore` type or other factory callers. HTTP/HTTPS authentication and
the destination transfer/inspection lifecycle remain unchanged. Regression
tests compare source names, bytes, modes and modification times after
success/refusal, including an empty source and a valid source without
`oldgen`; OS-managed access-time updates from ordinary reads are not writes
performed by memdolt. The CLI regression verifies nonzero refusal, empty
JSON stdout and an unchanged empty file source.

The structural changes are the root command's additive registration; new
`cmd/memdolt/clone.go` for flags/help/rendering; new
`localdolt/clone.go` for validation, ownership, transfer and inspection; new
`localdolt/clone_fs.go` for contained environment writes without deletion or
moves; two additive clone test files; and `go.mod` bookkeeping making the
already-selected Dolt, go-mysql-server and test gRPC modules direct imports.
The dependency graph and versions are unchanged. `Clone` uses the same `CloneRemote` engine
as `DOLT_CLONE` while avoiding that SQL wrapper's unconditional recursive
failure cleanup. Its supplied filesystem also refuses initialization cleanup
and the environment's old-temp-file sweep; NBS still manages its own transfer
files. These restrictions bind this clone environment, not all Dolt/store
operations. The existing ownership boundary covers cooperating memdolt
processes, not foreign Dolt sessions. Clone calls alone serialize access to
Dolt's global progress writers. The DSN builder validates destination
compatibility. `inspectClone` reads the already-open committed root;
`cloneSchemaVersion` reads its metadata with the pinned table iterator and
SQL string unwrapping, and reuses the existing pure version guard. It never
opens another engine, which would reinstate native environment cleanup.
The existing DSN builder/schema reader/guard, `Open`'s missing-database
creation, explicit `Migrate`, offline status,
memory/review operations and sixteen-tool MCP discovery retain their contracts.
The bootstrap table/column check is not a comprehensive schema/constraint audit.
`AGENTS.md` records the same before/after and per-symbol boundaries.

Deterministic tests use actual embedded push into a fresh filesystem remote,
clone, close and reopen, comparing main/history/authorship, task and note rows
including nullable provenance, and exercise the CLI's JSON/human/reopen paths.
They cover refusals, old-schema recovery, close-error propagation, foreign-file
retention during initialization/remote failure and across committed-root
inspection/close (including an old temp file and its timestamp), and local
synthetic gRPC authentication and cancellation. They do not establish real-hub credentials,
hub/client version compatibility across releases, or the two-machine network
gate. No configuration editor, application push/pull, remote status/diff,
conflict dialog, hub installation, topology backend or full M4 completion is
part of this slice.

**M4 main transfer subset (issue #127):** before this issue, the status and
clone subsets above left application push/pull deferred. After it,
`memdolt push [remote]` and `memdolt pull [remote]` ship with `--dir`, `--user`
and `--json`, defaulting to configured `origin`. They require an existing,
initialized current-schema store and never create or migrate one. Before
#129, missing remotes reported “no remote” and this remedy: stop the owner,
configure
`dolt remote add <name> <absolute-url>` in `.memdolt/dolt/memory`, or clone
into a fresh `--dir`. At #127 no remote editor, arbitrary URL/branch/refspec
operand, force, prune or all-branches option shipped. After #129, the remedy
names `memdolt repo remote add` below; the transfer operand/force/prune
restrictions still hold.

Push scans and publishes only the captured committed main hash, creating
remote main or advancing it by fast-forward. It never publishes unmerged
proposal refs or working-set changes and never resets remote history.
A later local main advance cannot change the captured upload. Pull fetches
remote main into its tracking ref before inspecting the exact immutable
candidate; equal or already-contained history is unchanged. A fast-forward
candidate must have current metadata and the supported application table and
column shapes before promotion. Missing/malformed/older/newer/incompatible
metadata refuses with a compatible-client/remote-repair remedy. Divergence
refuses without auto-merge or conflict resolution. Successful pull preserves
commit identities and adds no merge, genesis or migration commit.

Before the issue #127 cycle-2 fix, the main fetch in `transferProcedure` used
`actions.FetchRefSpecs`, whose non-shallow path also called `FetchFollowTags`.
That could replace existing local tag hashes and metadata or add remote-only
tags before candidate validation; its `cli.Println` also emitted unsolicited
newlines on direct CLI or live MCP owner stdout. After the fix, that pull path
uses `FetchRemoteBranch` for `refs/heads/main` and `SetHeadToCommit` only for the
selected `refs/remotes/<remote>/main`. Local tags and their metadata remain
unchanged, remote tags are not followed, and fetch emits no progress output.
These restrictions bind memdolt's pull path through `transferProcedure` alone;
native Dolt fetch/tag operations and clone keep their existing behavior.
Captured push, remote authentication and file-source preservation remain;
main still moves only after ancestry, schema and changed-text validation, and
fetched objects/tracking refs may still remain after refusal.

Both refuse dirty main working sets and active merge/conflict states before
transfer. Before #127, `Store.proposalMu` covered stage, accept, reject and
expiry but not direct `Commit`. After it, those mutations, direct commits and
entire transfers share the mutex on one owning Store, preventing pull from
replacing successful writes and review from sweeping another actor's write.
Network latency therefore delays memdolt mutations. At #127 reads and
migrations were outside this mutex; #129 adds remote configuration reads and
writes alone. Other reads and migrations remain outside, as do foreign Dolt
processes; no external-process lock
claim is made. Pending proposal refs/contents and the existing review,
contradiction, attribution and note-batching rules remain intact.

Configured URLs retain clone's explicit HTTP/HTTPS remotesapi or absolute
file-URL restrictions. URL credentials, queries/fragments, unsupported
schemes, invalid ports/users, flag-like names and unsupported stored driver
parameters refuse before contact. Only the native Windows stored spelling
`file://C:/...` is restored to `file:///C:/...`, and native decoded file-path
spaces are re-escaped before validation. An explicit
`--user` overrides the validated stored SQL username; neither means anonymous.
Passwords come only from `DOLT_REMOTE_PASSWORD` in the process executing the
transfer, including an already-running owner. Missing owner credentials name
the restart-with-environment remedy. No caller password crosses IPC, no
personal Dolt credential is used, and no process-global credentials are
changed per request. Plaintext, URL-encoded and Basic forms are redacted from
diagnostics and never persisted in configuration.

`transferTables` explicitly maintains the application column shapes and scan
coverage: facts, decisions, tasks, session notes with all five provenance
fields, commands, both narratives, documents/chunks, proposal metadata and
meta. Its shape check requires the known table/column sets, types,
nullability and primary columns; it is not a comprehensive index/constraint
audit. Push scans persisted snapshot text/provenance; pull scans only added
or changed values relative to captured local main. Scan/config/read failure
refuses upload or promotion. Unchanged local history is not rescanned on pull.
Fetched objects and tracking refs may remain after refusal, including
historical matching text: this is not a history scrub. The original write
declarations and review scanner remain unchanged; this third scan binds
`Push`/`Pull` alone, not every native Dolt transfer or future memory lane.

Before the issue #127 cycle-1 fix, that column check did not validate generation
mode or expression, yet the scanner exempted `facts.live_key` as derived.
Replacing it with a writable column or a changed generated expression could
therefore transfer denied text and lose the supported live-key derivation.
After the fix, `transferLiveKey` reads `SHOW CREATE TABLE` at the exact captured
or candidate commit, parses the complete DDL with the pinned SQL parser, and
requires STORED generation and the complete canonical expression
`IF(superseded_by IS NULL, key, NULL)` before granting its scan exemption.
Read/parse/shape failure refuses without echoing remote DDL. This restriction
binds `facts.live_key` in push/pull validation alone; it does not change clone,
migrations, review or every generated column, and it adds no comprehensive
index/constraint audit. Existing table/column and scan behavior remains.
The regression covers upload and promotion refusal for writable, changed
STORED-expression and VIRTUAL columns, with remote files and local main intact.

File pull reuses clone's source-preserving opener, including sources without
oldgen; journaled or otherwise unsupported sources refuse with inspection and
prepared-remote remedies. File transfers resolve the selected remote and
refuse contained symlinks. Push writes only within the selected file remote,
and no recursive cleanup or unrelated-file deletion is added. Embeddings,
rendered output, adjacent configuration and code indexes remain untouched;
existing stale-index detection and explicit rebuild remain the remedy.
Success is one JSON object (or the equivalent human report) containing
`operation`, `remote`, `localCommit`, `remoteCommit`, `mainCommit`, `changed`
and `status` (`changed`/`current`). Close/output failures are visible;
confirmed hashes remain in later-failure diagnostics. Lost responses require
inspection before retry, without automatic resubmission.

The structure touched is the root's additive command registration; new CLI
`transfer.go` and its tests; `repo.go`'s help-only removal of the obsolete
deferred-push/pull statement; new `localdolt/transfer.go`, `transfer_schema.go`
and `transfer_engine.go`, plus the two transfer test files; `Commit`'s added
mutex participation in `localdolt.go`; the typed `storeipc/operation.go` and
`owner_store.go` additions and their transfer tests; and this PRD/AGENTS record.
`transfer_engine.go` registers `memdolt_transfer` once before engine startup.
Only `runEngineTransfer` supplies its private context capability; ordinary
SQL/IPC Commit callers fail closed. This seam accesses the existing owning
session's DbData and pinned push/fetch actions, with fresh remote handles and
the credential-free dialer used by clone. Native SQL push/fetch would load
personal credentials when no username is supplied, and their stored username
can override `--user`; native procedures keep those behaviors, while memdolt's
transfer path avoids both. The capability restriction binds that one private
procedure, not all SQL procedures. Strict Dolt hash validation precedes
revision-identifier construction; raw SQL values remain bound. The existing
owner authentication, cancellation, operation allow-list, unlimited transfer
duration and single-submit rules still hold. `Open`, `Migrate`, clone/init,
offline repo status and the sixteen MCP tools retain their prior contracts.
`AGENTS.md` records the same per-symbol blast radius.

Automated evidence uses fresh filesystem remotes and initialized clients,
direct production operations and authenticated owner IPC, ordinary CLI reads,
independent clones and exact history/data/provenance comparisons. It exercises
no-op, divergence/non-fast-forward, dirty/incompatible/deny-list refusals,
note provenance, pending proposals, deterministic main-write/proposal
interleavings, source/adjacent-file preservation, synthetic authentication,
cancellation and lost/post-success responses. It establishes no real-hub,
two-machine network, cross-version, v2.x or full-M4 acceptance. No dependency
version, migration, MCP sync tool, remote editor, merge/conflict dialog,
topology backend or hub deployment change is included.

The cycle-2 regression in `cmd/memdolt/transfer_test.go` exercises allowed
and deny-list-refused pulls in subprocesses, both directly and through a real
`serve` owner verified by authenticated IPC. It compares the complete local
tag list, commit hashes, taggers, emails, timestamps and messages against
conflicting and remote-only tag fixtures. Raw CLI stdout must contain only
one success JSON line or remain empty on refusal; owner stdout must remain
empty when no MCP input was sent. It also checks clean owner shutdown and
reopened main/working-set state. This adds local synthetic evidence only;
the preceding acceptance limits still hold.

**Divergence and conflict resolution (issue #137).** Before it, the #127
fast-forward-only and no-MCP statements above remained the pull contract.
After it, `memdolt pull [remote] [--dir <repository>] [--user <sql-user>]`
retains fast-forward/contained history behavior and auto-merges compatible
divergence. `--json` reports captured local/remote/base hashes, resulting main,
changed/current/conflicted status, every conflict and its real provenance,
cleared verified records and remedies. A conflict prints its report and exits
nonzero after rollback; main, working/staged roots, proposal/tag refs, source
files and local derived/config/render artifacts are preserved. Only fetched
objects and the selected tracking ref may remain.

Submit all operator choices with `--resolve <file>` or `--resolve -` for stdin:

```json
{"localCommit":"<displayed local hash>","remoteCommit":"<displayed remote hash>","choices":[{"conflict":"<displayed id>","take":"ours"}]}
```

Data conflicts accept `take=ours|theirs|manual`. Manual supplies a complete
`row` of writable columns, including nulls and omitting generated `live_key`.
Live-fact-key conflicts accept `take=winner|manual` plus a displayed `winner`
ID; manual also supplies the complete winner row. Losers are superseded, never
deleted. A task choice must keep done unless `reopen=true` explicitly permits
open/blocked. Manual identity/provenance changes, malformed/duplicate/unknown
JSON fields, incomplete choices, changed heads and coerced/truncated values
refuse before promotion. The resolution file is only read as a rooted regular
file and remains unchanged. `pull --help` states the exact supported classes
and remedies from §6.3. No UI dependency is added.

Capture, native merge, inspection and rollback use the same transaction helper
as repository status. The native revision-qualified blame view internally
changes database context, so provenance is read after verified rollback;
explicit resolution recreates the same captured merge and validates all choices
and invariants together. The existing Store mutation mutex covers those steps
and final head checks. No transaction remains open during human input.
`DOLT_COMMIT` itself is the promotion point in the pinned driver, even before
`database/sql.Tx.Commit`; subsequent finalization is not a reversible phase.
Confirmed results survive late errors and are retained in CLI/MCP structured
output. An uncertain promotion/reply reports inspection before retry, never an
automatic replay. Existing upload/incoming changed-text scans remain, including
provenance; new chosen text and supersession links pass the local deny-list.
Unchanged local history is not retrospectively scrubbed. Foreign Dolt sessions
are outside the Go mutex; no cross-process compare-and-swap is claimed.

The structural change is `localdolt/transfer.go`'s options/result and divergence
dispatch; new `pull_merge.go`'s transaction/conflict/provenance/repair/invariant
helpers and `pull_json.go`'s strict shared parser; and `repo_status.go`'s reuse
of that transaction helper with its existing read-only assessment preserved.
Existing `review.go` helpers retain proposal allow-lists, contradiction checks
and cleanup policy. `cmd/memdolt/transfer.go` adds help/file/stdin/structured
conflict handling while preserving command registration and direct/owner
routing. Existing IPC transfer operations carry the extended types and keep
authentication, credential isolation, result envelopes and one-submit semantics.
New `mcpserver/transfer.go` and two `tools.go` registrations implement the typed
forms/continuation; `elicitation.go` and `elicitation_state.go` add an ephemeral
pull payload while preserving earlier fact/review states, expiry and atomic
consumption. No durable migration or dependency changes. The exact per-symbol
scope and every touched test/instruction file are recorded in `AGENTS.md`.

New localdolt, storeipc, CLI and MCP pull tests use only synthetic local remotes
and existing dependencies. They check both-parent history and blame, independent
and same-key merges, satisfied supersession, data/manual/task policies, schema,
metadata/unknown constraints and invalid links, malformed/denied/stale input,
root/ref/source preservation, modern continuation and genuine legacy forms,
capability fallback, state attacks/expiry/cancel/storage failures, interleavings,
and lost/late replies. Reopened ordinary CLI reads and a fresh clone verify
successful results. Existing transfer/status tests replace only obsolete
independent-divergence refusal expectations; tool discovery tests retain prior
checks and include both real tools. MCP instructions and the three wrap-up
templates add guidance for already-authorized transfers, retaining approval,
provenance and queued-note boundaries. Hub deployment/version/two-machine
acceptance, optional live SQL and global promotion remain separate.

**Issue #137 review-cycle corrections.** Before cycle 1, the shared operator
decoder's `encoding/json` calls replaced malformed UTF-8 and unpaired UTF-16
surrogate escapes, so SQL readback could not detect the original loss. After
it, `ValidateJSONUnicode` rejects those encodings before decoding, while the
existing decoder still owns JSON syntax/field validation. Its streaming
`JSONUnicodeReader` uses bounded buffering and passes bytes/framing unchanged;
valid surrogate pairs, escaped literal backslashes and literal U+FFFD remain
valid. Typed `TransferOptions` strings are checked before direct execution and
OwnerStore JSON serialization. `operationArgs` checks raw Unicode for every
typed owner operation using that helper, before unmarshalling. Other wire paths
retain their prior scope; no already-rewritten client text can be reconstructed.

Before cycle 1, the SDK could also replace malformed outer MCP response text
before receiving middleware. `newServeCommand` now gives the SDK IOTransport
a guarded stdin reader with its original closer and unchanged no-close stdout
semantics. The SDK connection itself still owns negotiation, batching and
cancellation; valid message sizes/framing and runServe's shutdown order remain.
This wire check binds that actual stdio construction and explicit reader users,
not `mcpserver.New` alone or arbitrary runServe-injected transports. Invalid
wire text terminates the connection without resolving the pull; the existing
orderly pending-note shutdown policy remains unchanged.

Before cycle 1, manual notes could replace session_id, agent_id, provider_id,
model_id and variant despite the provenance promise. `writePullRow` now freezes
all five alongside actor/actor_raw. Unchanged nullable metadata and selecting
an actual complete existing side remain supported. The new test also exposed
null absent images violating the generated MCP object schema. `PullConflictRow`
now follows `RepoRowDiff`'s omission convention for absent base/ours/theirs/
merged images; every nullable column of present rows remains represented.
Fallback, refusal and confirmed-error results stay typed and preserve effects.

The added regressions exercise raw and typed Unicode at decoder/direct/CLI/
owner/MCP boundaries, each immutable provenance field, valid text and side
choices, preserved roots, and schema-valid absent-image results. Actual stdio
tests retain modern discovery, genuine legacy and batch negotiation, large
valid messages and source-close cancellation. AGENTS.md enumerates every
touched symbol/file and its retained behavior. Existing confirmation, deny-list,
no-replay, #139 commit/result/residue and durable-schema policies remain; no
dependency or migration is added.

**M4 remote configuration subset (issue #129):** before this slice, the #127
setup remedy above required native Dolt with the owner stopped. After it,
`memdolt repo remote list` and `memdolt repo remote add <name> <absolute-url>
[--user <sql-user>]` ship with `--dir` and `--json`, using the initialized
store directly or through its verified authenticated owner. Missing or
unsupported stores refuse without initialization/migration. List returns
name-sorted `{name,url,user?}` entries in a `remotes` array, including
`{"remotes":[]}` (human: `no remotes configured`). Add returns that one stored
entry only after persistence confirmation and successful close; output/close
errors stay visible, and confirmed persistence survives later errors.

The existing transfer name/URL/user contract applies: names are 1–64 ASCII
letters/digits/dots/underscores/hyphens starting with a letter or digit;
usernames are 1–32 of those characters without a leading hyphen. URLs are
explicit HTTP/HTTPS remotesapi URLs with a database path or absolute file
URLs. Credentials, queries/fragments, unsupported schemes/ports, ambiguous
file-path encoding and unexpected operands refuse without quoting rejected
values. File remotes reject usernames. Configuration contacts no remote,
needs no password and does not require an existing file target. Transfers
still require a prepared file target and use only their executing process's
`DOLT_REMOTE_PASSWORD`. No caller password is an operand, IPC field or stored
configuration value. An owner must still be restarted with its own password
environment when transfer credentials change.

Add persists the validated URL and optional SQL username in native Dolt
remote state, with Dolt's ordinary default fetch specification. An occupied
name refuses; no remove/replace/force/arbitrary parameter/refspec interface
ships. Existing native remote fields remain semantically intact. Committed
main/history, working/staged memory, proposals, tags, derived indexes,
rendered files and adjacent user configuration remain untouched. Dirty memory
is allowed. Unsafe legacy stored URL/parameter values refuse before output
or configuration writes. Reads normalize only the established native Windows
file URL spelling and decoded file spaces for presentation/validation, without
rewriting any entry. No caller password is required for safe stored-user reads.

Both operations hold the same Store mutex as transfers, direct commits and
proposal mutations through native load/save/read-back. This excludes memdolt
interleavings, not foreign Dolt sessions. The private procedure verifies the
native configuration path, uses the owning session's filesystem without
opening another engine, and invokes native RepoState saving (temporary file,
sync and rename, with platform-specific crash guarantees). Native
`SessionStateAdapter.AddRemote` updates its live cache before saving; that
native method retains its behavior. Memdolt's `remoteProcedure` instead
publishes the new cache entry only after reading back the exact persisted
entry. A save-finalization failure after persistence returns both the
confirmed entry and a visible error. A successful list refreshes the owner's
in-memory view from validated native entries, allowing inspection after a
lost response or read-back failure before transfer. These restrictions bind
this configuration seam, not native Dolt writers, every read, or migrations.
Probe/auth failures never fall back to another engine. Add is one typed
owner operation, never replayed; unknown outcomes name
`memdolt repo remote list` before retrying.

The complete structure touched is `cmd/memdolt/repo.go`'s additive remote
registration/help (local-only status unchanged); new CLI `remote.go` for
validation/routing/finalization/reporting; `cmd/memdolt/transfer.go` and its
test's replacement setup remedy (transfer behavior unchanged);
`localdolt/clone.go`'s extraction of pure `validateRemoteURL` (clone's existing
user/password gate retained); `localdolt/transfer.go`'s reuse of stored-data
sanitization and new remedy (user override/password/transfer rules retained);
new `localdolt/remote.go` for the typed data, shared validators,
`ListRemotes`/`AddRemote`, native configuration and its private capability;
`storeipc/operation.go`'s Backend/allow-list/result additions and
`owner_store.go`'s validation/one-submit/error transport (existing
authentication, cancellation and operations retained); the three new remote
test files in CLI/localdolt/storeipc; and this PRD/AGENTS record.
`RequireExistingTransferStore` now also protects configuration CLI callers;
ordinary `Open` still creates and `Migrate` remains explicit.
`validateTransferSchema` checks their committed main without requiring clean
memory. Only `runRepoRemote` owns these commands' close-before-output ordering.
Only callers with its unexported context capability can reach
`memdolt_remotes`; ordinary SQL/IPC Commit cannot. `memdolt_transfer` retains
its separate capability. No general SQL-procedure restriction is implied.
The tests cover direct/authenticated-owner commands, named filesystem
push/pull, synthetic stored-user authentication, save/interleaving/transport
failures, unsafe/schema/missing/invalid/duplicate refusals and preservation.
They establish no real-hub or two-machine acceptance. No dependency, durable
schema, MCP tool, remote status/diff, automatic merge/conflict elicitation,
topology setting, hub operation or full M4 completion is added.

**Repository document ingestion subset (issue #132).** Before this slice,
the documents/doc_chunks schema, committed retrieval readers and derived
embedding index existed, while document ingestion was deferred. After it,
`memdolt doc add <file> [--title <title>]`, `doc ls` (alias `list`),
`doc show <id-or-path>` and `doc rm <id-or-path>` ship with `--dir`/`--json`;
add/rm also take the existing `--actor`. They require initialized current
stores, never implicitly initialize/migrate, and use the verified owner
when present. They report metadata, ordered chunk breadcrumbs (show also
returns bodies), created/updated/unchanged/removed/not-found outcomes, and
the exact commit for changed operations. Validation, read, output and close
failures remain visible. A source that no longer exists can still be shown
or removed by its stored path or ULID.

Before #132's missing-source identity fix, `documentIdentityPath` fell back
to lexical cleaning when the full source path no longer existed. Existing
parent aliases then lost their canonical spelling, and the same original
operand returned not-found from both show and remove. After the fix, this
helper resolves the nearest existing directory ancestor and appends only the
missing literal path components. `resolveDocument`, its only caller, preserves
exact stored-path/ULID matches without requiring source resolution and reports
unexpected filesystem errors when no exact match exists. `DocShow` and
`DocRemove` share this behavior through direct and authenticated-owner routes.
No deleted symlink target is guessed or recorded in an alias registry. The
changed structure is those two helpers, the original-path CLI lifecycle and
deterministic directory-alias regression, localdolt identity/error tests and
this AGENTS/PRD record. Source reading, confinement, owner-file protection,
schema and mutation contracts remain unchanged.

The chunk/title port follows the original memhub v0.2.0 functions, including
their edge behavior: first heading leaf or filename title, nonempty ATX
headings at levels 1–6, retained heading lines, ` > ` hierarchy and optional
preamble. Chunk lines normalize LF/CRLF to LF; SHA-256/byte length use original
UTF-8 bytes. Three backticks/tildes open fences; any three-or-longer matching
family closes them. The 2000-character (Unicode scalar) soft target packs
paragraphs greedily at blank lines outside fences, keeping a single oversized
paragraph/fence intact. This is not a CommonMark claim. Empty/whitespace
documents have zero chunks. Existing VARCHAR limits remain 1024/512/1024
characters for path/title/breadcrumb; each TEXT chunk must fit 65535 bytes.
Failures refuse before durable replacement. No dependency or migration.

`localdolt.DocAdd` holds `proposalMu` across source preparation, hash/identity
comparison, one direct-main transaction/commit and optional local config
finalization. Unchanged bytes keep the actual stored metadata/chunks and add
no commit, even with a different requested title. Changed bytes retain the
document ULID and replace every chunk with a fresh ULID. The existing path
unique index remains; a collation collision between distinct filesystem
paths refuses. `DocRemove` shares that mutex and deletes chunks then parent
in one commit. Both opt into `CommitRequest.RequireClean`, preserving dirty
memory, unrelated rows, proposals and their authorship. This coordination
binds document mutations plus the already-participating Store methods, not
foreign Dolt sessions or migrations. `DocList`/`DocShow` instead capture one
immutable committed-main hash and read that revision; no dirty/proposal row
enters their snapshot. SQL values are bound, and the unavoidable revision
database identifier is built only from a validated Dolt hash.

Source is `user` regardless of caller; commit provenance uses the normalized
caller. Ingestion scans source path, supplied/derived title, fixed source,
raw/normalized actor, heading breadcrumbs and content through the established
deny-list before durability. `doc_add` sets `DocAddOptions.Confined=true`:
file and configured allowed directories must resolve, and the target must
be within the canonical repo root or a resolved `[doc] allowed_dirs` entry.
Unresolved entries grant nothing. Relative MCP file/configured paths start
at the repo root; CLI file paths start at the caller's working directory and
may be outside those roots. Canonicalization followed by `os.Root` confines
the read against symlink escape. Missing config uses defaults; an existing
unreadable/invalid config fails closed, and managed document config rejects
links. These restrictions bind this document seam, not every filesystem read,
and neither paths nor document source can grant human review authority.

**Owner metadata protection, #132 before/after.** Before the safety fix,
`readDocumentFile` relied on root confinement and optional regex rules alone:
`.memdolt/server.pid` is inside the root, so absent/empty rules let its token
reach `doc_add`'s returned chunks, a Dolt commit and production recall. After
the fix, the same shared reader reserves this store's known owner path before
opening it as a source and invokes `checkDocumentOwnerFile` on the opened
source's `Stat` identity before `io.ReadAll`. The existing metadata directory
handle locates the protected file. It is opened for `Stat` only, never content;
both compared identities come from opened handles, so Windows file-ID lookup
errors cannot silently mean different files. Canonical, symlink and hard-link
aliases are refused. Missing owner metadata is normal for a direct store;
required link/type/open/stat/identity/close checks otherwise fail closed with
no document, commit, returned credential content or config finalization.

This rule binds every `DocAdd` through `readDocumentFile`, including CLI and
confined MCP calls; it is independent of `Confined`, allowed directories and
optional regex patterns. Ordinary CLI source-root freedom and configurable
deny-list behavior remain. It protects the known store owner file and its file
aliases, not arbitrary copies of secrets, other filesystem/SQL readers or
existing stored history. `document_file.go`, `documents.go`'s metadata-handle
argument, CLI help, MCP input description, the new MCP owner regression file,
added CLI/localdolt tests and this PRD/AGENTS record are the complete additional
structure. Synthetic fixtures prove absent/empty-rule refusals, actual alias
handling, no response/data/commit/config effects, verification failures and
ordinary external CLI source acceptance. IPC ownership, token generation,
schema, retrieval, rendering and repository-status contracts remain unchanged.

That scope statement records the #132 boundary. After #140, selected import
bundles and existing export outputs also reach the same opened-identity guard;
the document rule remains unchanged. Protection still belongs to its explicit
callers, not every filesystem/SQL reader or arbitrary copies of secrets.

The first ingestion into an empty documents table enables
`[retrieval] include_docs_in_default`, with a visible result/notice if changed.
Removing all documents resets that baseline trigger. Later unchanged/changed
ingests or second documents do not undo an opt-out. The latest complete TOML
is reread and unrelated values are preserved semantically (not comments or
formatting), through temporary-file/sync/rooted-rename replacement. Detected
concurrent config edits refuse; foreign edits in the final read/rename
interval and stronger cross-platform crash guarantees are not claimed.
If this finalization fails after commit, CLI, MCP and owner IPC retain the
confirmed document/commit with the visible error and a manual config remedy;
ingestion is never replayed. Lost owner replies require doc ls/show inspection.
Existing retrieval scoring stays intact: default documents must undergo a
rerank to survive, while explicit `doc_chunk` queries also work in FTS.
Index status/rebuild remain the derived-store workflow. Replaced/deleted
chunk vectors become orphaned, new chunks missing until rebuild; stale vectors
cannot impersonate current chunks. No eager model work is added to ingestion.
Config/default flags and the embedding side-store remain machine-local.

The complete changed structure is new `localdolt/documents.go` (typed
operations, coherent readers, serialized writes/results),
`document_markdown.go` (tagged parser/title), `document_file.go` (path/width/
config helpers), CLI `doc.go` (flags, routing, reporting), MCP `doc.go`
(typed confined handler and result-bearing errors), their four test files,
root/tool registrations, `storeipc.Backend`/allow-list/wire result and
`OwnerStore` methods, tool/serve expectation tests, `localdolt.commitConn`'s
clean-guard diagnostic, and this PRD/AGENTS record. Before, that diagnostic
named only note batches; after, it says guarded write, with the same opt-in
semantics. All prior CLI/tools, discovery/cache hints, attribution, note
batching, elicitation, shutdown, authenticated routing/cancellation and
single-submit/no-fallback rules remain. The exact tool set binds
`RegisterTools`, close/report ordering binds `runDoc`, and config finalization
binds first-document `DocAdd`, not every symbol of those kinds. Existing
schema/migrations, ULID/manifest/path helpers, review gates, committed recall
readers and index behavior are reused unchanged. Tests exercise actual local
CLI/owner/MCP, original tagged cases plus verified edge cases, Unicode/empty
input, no-op/replacement, rollback/dirty/proposal preservation, path/deny/
config/width failures, retrieval after indexing and lost/confirmed-late errors.
They establish no broader cross-machine path/format guarantee, global
document backend or full M5 acceptance.

After integration with #133, the existing seventeen tools including `render`
remained and `doc_add` became the eighteenth. After integration with #131,
the existing eighteen tools including `render` and `repo_status` remain and
`doc_add` is the nineteenth. All three complete owner operations share
`proposalMu`. Direct/authenticated-owner CLI and modern/legacy MCP integration
tests verify rendering captures the ingested document commit while preserving
its metadata, chunks and content-hash no-op. The first-document config update
preserves real render settings. The renderer's existing categories, file
replacement rules, instructions and templates remain; document commits appear
as real activity, without a new document-body category.
The same combined CLI/MCP checks now exercise explicit offline status and
default no-remote status against the document commit, preserving the stored
metadata/chunks and main hash. Configured-remote status behavior remains #131's.

**Committed memory rendering (issue #133).** Before this delivery, the §12
render workflow and its CLI/MCP names were deferred. After it, `memdolt render
[--dir <repository>] [--json]` generates `PROJECT.md` and `PROJECT_LEDGER.md`;
the real `render` MCP tool invokes the same owning-store method. Both files
carry `<!-- memdolt:rendered -->`, source schema version, exact main commit and
generation time. Reports identify confirmed written files, recoverable backups
and any error, including partial completion or store-close/output failure.
Missing/unsupported stores and malformed configuration fail visibly; render
never initializes, migrates or creates a Dolt commit.

`internal/render.capture` reads one committed `main` hash, validates its exact
32-character Dolt grammar, and uses it for every category and `DOLT_LOG` walk.
The pinned driver panics preparing `AS OF ?`, so only the validated revision
becomes a literal clause; ordinary SQL values remain bound, and table/column/
ordering syntax comes solely from a fixed list. No caller supplies a revision
or query. The snapshot contains the newest state and architecture with actor
and raw provenance, the last ten notes with optional session metadata, all
decisions (including summary, alternatives and evidence), tasks ordered open/
blocked/done then newest update, and facts ordered by key with verified/stale
and superseded annotations. Superseded rows remain visible; references use
actual ULIDs. Multiline bodies and evidence remain intact in blocks instead
of flattening them into ledger table cells. Activity is the newest fifty real
reachable commits in thirty days with hash, author, email, date and message;
it contains no invented writes_log entries. Empty categories are explicit.
Transcript archives and gated token-accounting data are absent.

`localdolt.Store.Render` shares `proposalMu` with participating direct writes,
proposal mutations and transfers for the complete operation. Previously render
did not participate because it did not exist; every prior mutation keeps its
existing behavior. Other reads/migrations and foreign Dolt sessions remain
outside the mutex. The immutable revision keeps capture coherent even across
a nonparticipating change to main; a source/scan/row-close failure refuses
before output preparation. Pending proposals and uncommitted rows are excluded,
and the current working set, refs and history remain unchanged. This boundary
binds `Store.Render`, not every store read or generated-text helper.
Issue #131 subsequently adds repository status to the same mutex without
changing render's snapshot/file ordering or preservation behavior.
Before #145 this described the complete session render too. After it,
Toolset.Render first commits eligible pending notes under its separate queue
mutex, then calls this unchanged Store.Render. Failed flushing publishes no
files. Both the MCP tool and `runServe`'s authenticated CLI route use that wrapper.

`internal/render.writeFiles` uses directory handles, refuses symlink/reparse
traversal and unmarked same-name user files, and prepares both complete sibling
temporary files and all original backups before replacing either output.
Backups stay in `.memdolt/backups/rendered` under exclusive unique names; complete
backups are retained even after a preparation failure. Files are synced and
closed before replacement by `os.Root.Rename`. Windows reaches native
`NtSetInformationFile` replacement with its compatibility fallback; other
supported platforms use their native rename. The pair is not transactionally
atomic. No stronger per-platform crash guarantee or directory-entry durability
is claimed. If the second replacement or finalization fails, the result retains
the exact successful prefix and backups. Cleanup only removes this invocation's
verified temporary artifacts; unrelated files are preserved. An exclusive
`.memdolt-render.lock` in the output directory refuses overlapping cooperating
renders, including owners from different repositories using that directory.
It has no automatic stale/PID recovery: after a crash, stop all renders and
inspect outputs/backups before manually removing residue. Foreign file writers
are not coordinated; identity/content rechecks catch prior changes but cannot
provide a compare-and-swap in the final interval before native replacement.
These restrictions bind this file writer alone, not every filesystem writer.

New `render/render.go`, `files.go`, `path_windows.go` and `path_other.go` own
that capture/format/config/file boundary; new `localdolt/render.go` supplies
the owner method. `storeipc/operation.go` adds its explicit typed allow-list
entry and `owner_store.go` submits once, preserves result/error and names the
inspect-before-retry remedy. New `cmd/memdolt/render.go` owns flags, preflight,
close and human/JSON output; `root.go` retains all children and adds render.
Existing `RequireExistingTransferStore` also protects render before ordinary
`Open` can create anything. New `mcpserver/render.go` handles the empty typed
input and preserves structured error results; `tools.go` registers it and
`instructions.md` records its committed-only scope. Prior CLI/MCP/owner behavior
is retained. The five new render test files, shared `render/testdata/memory.sql`,
and updated discovery/template tests exercise the named synthetic boundaries,
not full M5 or real-host acceptance. AGENTS.md records the complete file inventory.

### 11.3 Config

`.memdolt/config.toml` mirrors memhub's structure where semantics survive: `[deny_list]`, `[render]`, `[retrieval]` + `[retrieval.scoring]` (identical knobs/defaults), `[code_index]`, `[doc] allowed_dirs`, `[global]`, `[audit]`, `[wrap_up]`. Replaced: `[sync]` → `[repo] remote_url, topology = "clone" | "live" | "local", auto_pull_on_session_start (bool)`. Machine config `~/.memdolt/config.toml` holds hub defaults + known-projects registry (upgrade enumeration — never a filesystem scan; memhub parity).

Before #138 `[code_index]` had no reader. Its new independent reader consumes
only `fts_weight`, `vector_weight` and `test_path_penalty`; it retains the
tagged shared retrieval mode/pool, not recall's scoring/toggle/floor settings.
Invalid TOML, unknown code keys, invalid consumed values and invalid deny rules
fail visibly. Memhub deny patterns were path globs; memdolt's existing regex
meaning is unchanged and now scans code paths/content. Glob `private/**`
maps to regex `(^|/)private/.*$`, and `*.generated.go` to
`(^|/)[^/]*\.generated\.go$`, written as TOML literal strings. The tagged default
secret paths are fixed code exclusions even with an explicit empty regex list;
that opt-out behavior differs from memhub. There is no byte-compatible TOML
claim, second pattern setting, or change to existing memory-write enforcement.

Before issue #133, `[render]` had no implementation. After it,
`render.output_dir` defaults to `.memdolt/rendered`. Relative paths must stay
within the canonical repository root; an explicit absolute local path in this
machine-local configuration can route elsewhere. Network/device namespaces,
symlink/reparse traversal, `.git`/`.memhub`/`.orchestrator` metadata and
`.memdolt` destinations outside its `rendered` subtree are refused, protecting
the store, config, backup and index paths. Backups always remain in the owning
repository's `.memdolt/backups/rendered`. Default outputs/backups are already
ignored by the existing `.memdolt/` rule; the operator supplies ignore rules
for custom destinations. The optional root `project_name` defaults to the
repository basename; `[retrieval].fact_stale_after_days` uses the existing
positive-int64 ninety-day default without a duration-overflow conversion.
The independent render reader rejects invalid TOML, unknown render keys and
invalid consumed values; it does not load models or change the independent
deny-list/retrieval readers. MCP accepts no destination override.

That project-topology configuration remains the planned surface. Issue #129
stores remote entries only in Dolt's native configuration; it does not add
or rewrite TOML `remote_url`, topology, auto-pull or machine hub settings.

Before #163 the preceding planned-surface statement still applied. After it,
`[repo]` has an independent protected reader and `repo configure` updates only
supplied keys with the owner stopped, preserving other TOML values. Local and
clone share the embedded format. Local suppresses implicit status network
requests; explicit push/pull and named status remain available. Clone uses the
existing native pipeline. Live refuses before local access. Absent settings
preserve existing native-remote behavior. TOML remote_url supplies origin when
absent; simultaneous native origin must match exactly, while another explicit
name keeps its selected target. Clone can consume an omitted URL from TOML,
but conflicting explicit URLs refuse. No native remote is silently rewritten.

Startup pull is off by default and requires clone. It calls existing Pull once
before MCP/IPC publication, using memdolt attribution for conflict-free startup
merges. Conflicts require normal human terminal review; errors/cancellation and
unknown outcomes stop startup with inspection remedies, never automatic replay.
CLI opens never auto-pull. Current policy is rechecked per reached Store
operation, including authenticated owner methods. Malformed TOML now refuses
these operations; NoText retains its deny-regex exemption, not an exemption
from routing-file parsing. Explicit global entry points retain separate native
policy. Confirmed config/identity/migration/pull effects survive later failures.
The [guide](../repository-topology.md) records every changed component, original
guarantees and exact symbol scope. Machine defaults/registry and live SQL remain
planned, and this introduces no new schema, dependency or host/hub installation.

Before #163's first review correction, struct case folding could let `Topology`
or `[Repo]` override correctly named configuration. After it, exact map lookups
consume only `[repo]` and its exact lowercase keys with their declared types;
aliases and mixed-case collisions refuse. Unrelated TOML semantics survive.
This restriction binds the shared repository reader/setter and their reached
CLI/owner/MCP callers, not every configuration reader in the application.

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
    "memdolt-wrap-up": {
      "description": "Wrap up this memdolt session",
      "template": "Use the memdolt-wrap-up skill. Arguments: $ARGUMENTS"
    }
  },
  "skills": ["templates/skills/opencode"]
}
```

Before #155 the example command was `wrap-up`, referring to the "memdolt wrap-up
skill". After it, the key and reference are `memdolt-wrap-up`, matching the
namespaced installed skill. The source-relative `skills` example still works
in this checkout; copied installations use an actual installed absolute path
as described in §11.5. The MCP registration shape is unchanged.

`commands` is the plural command-object map and `skills` is a path array. Doctor recognizes a registration only after parsing a repo or user `opencode.json`/`opencode.jsonc`: native `mcp.servers.memdolt` and officially supported V1 `mcp.memdolt` count; JSONC comments and trailing commas are accepted before a real JSON parse; malformed files and similarly named unsupported paths do not count.

Before issue #116's cycle-1 parser correction, the doctor implementation used
Go struct decoding at the root and also accepted `MCP`/`Mcp`, contradicting the
exact-path rule above. After it, `openCodeConfigRegistersMemdolt` uses exact
map lookups for every path segment. Unknown keys, including case variants
alongside a valid registration, remain ignored. JSONC support and the optional
advisory behavior remain unchanged; this rule binds the doctor reader alone,
not every JSON decoder.

**Wrap-up provenance.** Before issue #116, this paragraph described the workflow
without separating host obligations from guarantees the CLI can enforce:

> An OpenCode wrap-up takes its current session id only from host context — never a session list, title, active-session heuristic, or filesystem — then independently verifies it with `opencode2 api get "/api/session/<id>"`. It parses `data` as `Session.Info` and requires the returned `data.id` to exactly equal the host-supplied id. An unavailable id, failed API call, malformed response, missing returned id, or mismatch stops before every durable memory write, render, or sync. On success the session note stores `session_id` plus nullable `data.agent` (`agent_id`) and, when present, `data.model.providerID` (`provider_id`), `data.model.id` (`model_id`), and `data.model.variant` (`variant`); they never enter free-form text or derive actor/source values.

After issue #116, obtaining the current ID only from host context and never
discovering or guessing another session remains the **workflow obligation**.
The CLI accepts a caller-supplied ID and **cannot authenticate its origin**.
`opencode.VerifySession` validates `ses` plus a nonempty ASCII alphanumeric,
underscore, or hyphen suffix (255 characters total at most), then calls
`opencode2 api get /api/session/<id>` through argument-based process execution.
Only an unavailable bare executable permits the Windows `opencode2.cmd`
fallback; a command that ran and failed is not retried. A parsed object `data`
with an exact, nonempty `data.id` is required. API failure, malformed metadata,
missing identity, or mismatch refuses before `opencode wrap-up-note` opens the
store or writes its note. `opencode session-info` offers the same verification
without opening a store. Neither command proves the supplied ID is the caller's
current session, nor governs writes through other commands or MCP tools.

Before the cycle-1 parser correction, struct decoding treated protocol keys
case-insensitively, so `data.ID` could replace a mismatched actual `data.id`,
and uppercase-only `DATA.ID` could satisfy missing lowercase members. After
it, `VerifySession` looks up every consumed key exactly: `data`, `id`, `agent`,
`model`, `providerID`, and `variant`. Only the actual `data.id` can satisfy
the identity check; other casing is unknown input and stays ignored. Typed
string decoding, nullable optional metadata, and unknown-field compatibility
remain, with no metadata alias able to overwrite the selected exact field.
This correction binds `VerifySession` and its two CLI callers, preserving
refusal before the writer opens the store; it changes no other JSON reader,
note lane, or actor derivation.

Successful wrap-up notes persist the field mapping above separately from the
body, retain the existing note-body whitespace normalization, and always use
canonical actor `agent:opencode` with raw `cli`, independently of API metadata.
The ordinary CLI note lane remains available without OpenCode verification;
its nullable metadata is absent. `LogNoteWithProvenance` and `CommitNotes`
validate metadata widths without trimming values and include the new strings
in their deny-list declarations (§6.1); these general lanes do not verify an
OpenCode API identity. Authenticated owner routing, note batching, the sixteen
MCP tools, and elicited human review retain their existing behavior.

At M3, the committed core templates for Claude Code, Codex, and OpenCode offered
only check-init, recall, and wrap-up using implemented M3 operations. OpenCode
wrap-up verifies the host-context ID before any workflow write and re-verifies
it when writing the approved summary; a failure stops later steps. Facts and
decisions remain proposals for human review. Before issue #133, no template
added render, sync, transcript capture, wrapper installation, or the other
deferred backends. After it, the three wrap-up templates add only render after
approved writes and report its committed source and file effects. Before #145,
Claude/Codex notes remained queued until their deadline/shutdown flush, so the
summary was absent from earlier renders. After #145 the then-step 6 flushes that owner's
pending notes through either MCP or authenticated CLI before the snapshot;
flush errors stop publication and confirmed/unknown outcomes require inspection.
OpenCode's verified CLI summary is already committed. Their review, approval
and identity pre-flight gates remain unchanged; a partial/unknown render stops
the workflow for inspection before retry. Other deferred operations remain
outside these templates, and transcript/token-accounting gates are unchanged.

**Transcript archive.** Transcript mode requires a separate explicit approval that warns the archive is unredacted. It reuses the already verified current id and never discovers or guesses another one. Before any process invocation, validate an OpenCode id as `ses` followed by a nonempty ASCII alphanumeric, underscore, or hyphen suffix. Invoke `opencode2 api v2.session.export --param sessionID=<id> --param sanitize=false` through argument-based process APIs (on Windows, fall back to `opencode2.cmd` only when the bare program is unavailable). Before opening the project or writing, require a JSON object `data`, object `data.info`, exact `data.info.id`, and array `data.messages`. Archive the exact complete, unsanitized response bytes as `.json.zst` and retain one replaceable local pointer per session. Archive and pointer data are excluded from recall, embeddings, and every export; `[wrap_up].transcript_retention_days` governs retention, and expiry removes the local archive and its pointer.

**Skills and upgrade.** Hibernated `metrics` and `viz` may remain in source for feature builds but are absent from normal OpenCode `skills` and command discovery. Additive wrapper sync never creates an absent agent directory, overwrites an unowned file, or automatically deletes stale or hibernated wrappers. It reports stale or unowned wrappers as actionable orphans for manual review/removal and leaves user files unchanged.

### 11.5 Guided onboarding and narrative continuity (issue #155)

Before this slice, #138/#146 had expanded the three-host package to six generic
workflows: check-init, eval-locate, global, locate, recall and wrap-up. README
installation prompts primarily initialized an empty store and connection;
wrap-up did not inspect or refresh project_state/project_arch. After it, seven
`memdolt-*` entry points per host add guided project bootstrap and separately
approved narrative updates. Existing store operations supply all behavior;
there is no runtime wizard or installer. The complete touched structure is:

- `README.md` Quickstart adds guided bootstrap and links the shared procedure;
  MCP/hosts documents copying the resource beside the discovery root, explicit
  authority/target selection and old-alias reporting. One-machine/transfer
  guidance clarifies host handoff and explicit vector/view/global refresh.
  Existing build, model verification, human review, secret handling and
  runtime/acceptance limits remain. Existing memhub installations are preserved.
- New `templates/skills/memdolt-resources/onboarding.md` owns the common
  inspect → fresh/clone/existing/migration choice → missing-context questions
  → separate narrative approvals → optional retrieval/code/docs/global/remote
  choices → attributed stdin writes → verification procedure. It requires
  inspecting existing source narratives instead of reseeding, distinguishes
  narrative-only blank recall from failure, and describes partial/unknown
  results without automatic replay. Migration is a separately approved
  disposable rehearsal; hub deployment and new catch-up/audit/upgrade
  workflows remain outside this delivery.
- New `claude/memdolt-init-project.md`, `codex/memdolt-init-project/SKILL.md`
  and `opencode/memdolt-init-project/SKILL.md` under `templates/skills/` load
  that one procedure. Their relative links resolve from the installed file
  to a sibling `memdolt-resources/onboarding.md`, independent of the target
  project's cwd or presence of this source checkout. They supply actual host
  actor spellings (`Claude Code`, `codex`, `opencode`); no human impersonation.
- The eighteen existing entry points are renamed: each of **check-init,
  eval-locate, global, locate, recall, wrap-up** gains the `memdolt-` prefix
  in `templates/skills/claude/<name>.md` and
  `templates/skills/{codex,opencode}/<name>/SKILL.md`, including frontmatter.
  Check-init, eval-locate, global, locate and recall retain their prior
  operating rules. Namespacing changes source discovery, not installed user
  files; old generic aliases are reported for human review without deletion.
- The three renamed `memdolt-wrap-up` templates now inspect narratives in
  step 1, require separately approved changes in step 2, and write those changes
  with attributed CLI `state set` / `arch set` and stdin in step 6 before
  render in step 7. Unchanged/rejected narratives require no set call. The
  task/command/proposal approval rules, human fact/decision gate, note flushing,
  confirmed/unknown outcomes, no-replay and explicit transfer boundaries remain.
  OpenCode still verifies the current host-context ID before later workflow
  writes and makes its verified summary the first durable write in step 3.
- `opencode.json` retains its native V2 MCP server and source-relative skills
  root. Its six command keys/template references gain the exact `memdolt-`
  names and the seventh wraps `memdolt-init-project`. The README describes
  merging selected entries with a real absolute installed skills path while
  preserving unrelated configuration and host trust choices.
- `cmd/memdolt/host_templates_test.go` updates
  `TestTrackedHostRegistrationsUseNativeCoexistingShapes` to check the seven
  command keys and exact skill references while retaining native registrations.
  `TestCoreSkillTemplatesMatchAcrossHostsAndUseImplementedTools` checks actual
  names/frontmatter and narrative/identity/approval ordering. Before this
  correction its deferred list erroneously included implemented `doc_add` and
  `repo_status`; after it only `history` and `archive_transcript` stay forbidden.
  New `TestInstalledOnboardingResourcesResolveOutsideCheckout` copies the
  documented layout into isolated host roots, preserves a generic memhub
  fixture and resolves the actual shared link from an unrelated project cwd.
  `allSkillFiles` skips the shared resource directory because it is outside
  workflow discovery; the other file/JSON enumeration helpers remain unchanged.
- `cmd/memdolt/code_test.go` updates only the two template lookup paths in
  `TestLocatorSkillTemplatesAndProtectedGoldenCommand` to their namespaced
  names. Its existing locator language checks and exact protected golden CI
  command assertion remain unchanged, as do all runtime code-index tests.
- New `cmd/memdolt/onboarding_test.go` adds
  `TestOnboardingNarrativeRecipePreservesMemoryAfterReopening`. Existing CLI
  helpers initialize disposable replicas and close/reopen them across stdin
  bootstrap/update/show/render calls for all three host actors. Checks retain
  approved fixture bodies and earlier versions, row/commit attribution,
  unrelated tasks/notes, pending proposal heads and render exclusion; rendering
  without another approved update adds no narrative commit. FTS narrative-only
  recall is empty. This is deterministic package/CLI evidence, not live-agent
  approval or host compliance, OpenCode session verification, or cold-process
  cache acceptance. No fixture touches live host directories or user memory.
- This PRD updates §11.4's command example and historical template scope,
  records the complete before/after here and adds the bounded §16 delivery
  record. Its parity baseline and M3/M4/M5/M6 acceptance gates remain unchanged.

Approval, authority selection and current-session provenance are obligations
of these onboarding/wrap-up templates, not new runtime security enforcement on
every CLI/MCP entry point. `newNarrativeSetCommand` still accepts stdin and
explicit attribution through the existing direct lane; `Lanes.SetNarrative`
still appends a version and does not enforce template approval. The
`opencode.VerifySession` origin limitation in §11.4 remains scoped to that
verifier and its two CLI callers, not all memory writers. `Toolset.Render`
flushes that owner's queue; standalone store rendering has no session queue.
`RegisterTools` still registers exactly 22 tools; no registration is added.

Before #167 the guidance distinguished `doctor`'s five checks from actual host
workflow/tool discovery and target verification. After #167 the sixth check
reports the embedded Dolt release; the original five and that distinction remain.
MCP `status.dataDir` reports
`<root>/.memdolt/dolt`, whose `memory` child is the database. Each `serve` owns
its local clone; supported CLI calls route through its authenticated owner,
but a second MCP server does not. Executing-owner credentials, optional
verified model downloads, separate global transfers, local-only artifacts,
nontransferred proposals/queued notes and the still-refusing global proposal
acceptance path retain their existing contracts. No runtime code, dependency,
schema, auto-migration, live configuration or hub deployment changes.

### 11.6 Existing-repository catch-up (issue #157)

Before this delivery, §11.5's seven host workflows and shared onboarding
guidance described the daily transfer sequence but left a new catch-up
workflow outside that slice. After it, `memdolt-catch-up` is the eighth
namespaced entry point for each host. It selects an existing repository and
configured remote, fetches status, performs authorized reconciliation with
human conflict choices, then refreshes applicable local vectors and rendered
context. It adds no CLI command, MCP tool, installer or transfer implementation.
The complete touched structure is:

- New `templates/skills/memdolt-resources/catch-up.md` owns the shared procedure:
  authority/absolute-target/live-MCP binding checks, offline local/remotes
  inspection, selected-remote fetch/diff, current/ahead/behind/divergence and
  refusal handling, authorized pull and complete hash-bound human resolution.
  It retains the shipped owner/credential, main-only transfer, proposal/queued
  note exclusion and separate-global contracts. MCP forms retain single-use
  continuation after nine forms and fresh review after cancel/expiry/restart
  or changed heads. Vector staleness is distinct from configured retrieval
  mode, verified model availability and authorization; FTS-only work can skip
  vectors without changing configuration or provisioning models. Explicit
  render accounts for queued note commits, then reads the resulting context
  and queue without reseeding narratives or inventing work. Confirmed hashes,
  noteCommits and file/backup effects remain visible on later failures;
  unknown or partial outcomes stop dependent work for inspection, not replay.
- New `templates/skills/claude/memdolt-catch-up.md`,
  `templates/skills/codex/memdolt-catch-up/SKILL.md` and
  `templates/skills/opencode/memdolt-catch-up/SKILL.md` keep the existing
  host-specific frontmatter conventions and load that one procedure relative
  to the installed entry point. Missing resources refuse the workflow. The
  prior seven templates per host and generic installed memhub files remain.
- `templates/skills/memdolt-resources/onboarding.md` updates its discovery count
  and links the sibling catch-up resource for daily use. Its former statement
  that no new catch-up workflow exists is preserved there as before/after;
  the new source procedure replaces only that absence. Onboarding's authority,
  setup choices, separate narrative approvals, host attribution, optional
  model/doc/global/remote setup, migration boundary and outcome rules remain.
- `opencode.json` adds one `memdolt-catch-up` command referencing that exact
  skill. Its seven previous commands, native V2 server registration and
  source-relative skills array are unchanged; no live host configuration is
  installed or rewritten.
- `README.md` updates the three Quickstart package counts from seven to eight,
  exposes catch-up under moving between machines and clarifies that status
  fetches. MCP/hosts preserves the historical seven-workflow record and adds
  the eighth with both shared filenames under the existing sibling-resource
  copy layout. Existing collision inspection, generic memhub preservation,
  target/binding, trust, model/credential and transfer limits remain.
- `cmd/memdolt/host_templates_test.go` extends
  `TestTrackedHostRegistrationsUseNativeCoexistingShapes` and
  `TestCoreSkillTemplatesMatchAcrossHostsAndUseImplementedTools` to expect
  eight names/exact OpenCode references, retaining every previous registration,
  frontmatter and wrap-up check. The former
  `TestInstalledOnboardingResourcesResolveOutsideCheckout` becomes
  `TestInstalledOnboardingAndCatchUpResourcesResolveOutsideCheckout`: it copies
  the same documented layout, resolves both entry-point links and onboarding's
  sibling link from an unrelated cwd, compares actual resource bytes, and
  preserves generic recall and catch-up fixtures. Enumeration/JSON helpers
  and the exclusion of shared resources from discovery remain unchanged.
- New `cmd/memdolt/catch_up_test.go` adds
  `TestCatchUpFTSRecipePreservesContextAfterReopening`, using existing disposable
  CLI/remote/owner helpers. Direct and real owner-process cases inspect local
  and named remote state, pull a behind replica, observe unchanged views until
  explicit render, then reopen and read narratives, rendered context, tasks,
  pending proposals and current heads. FTS retrieves the incoming task despite
  missing vectors, preserving local configuration and an empty model location.
  The recipe deliberately skips optional vector rebuilding. Existing tests
  retain real inference, conflict forms/cursors and late/unknown render/transfer
  coverage; this recipe does not claim to exercise those branches anew.
- This PRD adds this bounded delivery and the §12/§16 follow-ups while keeping
  the prior scope records, parity baseline and acceptance gates.

The new authority/authorization/sequence restrictions bind the catch-up host
procedure, not every CLI/MCP caller. `RepoStatus` still fetches and rolls back
its merge preview without promoting main; `Pull` retains its separate schema,
deny-list and merge validation. Their mutation mutex coordinates cooperating
Memdolt operations, not foreign Dolt sessions. `index status` compares source
and vector rows, not mode/artifacts; `index rebuild` can lazily open the existing
engine, which verifies/provisions both models, tokenizers and native runtime.
`Toolset.Render` flushes that owner's queue; standalone `Store.Render` has no
session queue. These named operations keep those specific boundaries, not a
new blanket rule for all readers/writers. `RegisterTools` still registers 22
tools. No runtime code, dependency, schema, model manifest, golden assertion,
user memory, installed host file or global replica changes. Package/CLI recipe
evidence does not establish live-agent compliance, real hybrid catch-up, cold
client-process behavior or physical two-client/hub acceptance.

---

## 12. Feature parity matrix (memhub v0.2.0 baseline + v0.2.2 OpenCode 2 supplement → memdolt)

Every ordinary row uses v0.2.0 as its baseline. Rows explicitly labeled v0.2.2 are the narrow OpenCode 2 supplement only, not a claim that all intervening memhub behavior was audited.

Before #171 the CRUD row below still lacked explicit CLI file bodies. After it,
the four commands in §11.2 accept them with native TEXT byte protection while
preserving existing stdin/writer contracts. That section explicitly dispositions
the tagged note/narrative character limits; broader CRUD parity remains separate.

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
| render PROJECT.md / PROJECT_LEDGER.md (previously called "atomic two-phase write") | shipped in #133; prepare both files/backups, then native per-file replacement; the pair is not atomic; ledger activity uses real Dolt commit metadata (§11.2) |
| doctor (19 checks) | port + memdolt-specific checks (LOCK/pidfile/IPC §5.2, remote reachability, schema skew, model presence, empty-recall rate §8.1) |
| audit md (CLAUDE.md/AGENTS.md linter) | port (pure text tool) |
| export/import JSON v1 | import kept (migration §15); export kept for interop; neither is the sync path |
| ingest-git + `search file:` | Before #174, placement design only (§6.1 note); after #174, explicit local derived-cache ingestion and exact cached search ship (§9), with partial-range coverage disclosed. |
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

Before #157, the skills row above characterized catch-up as trivial `pull`.
After it, §11.6 ships the bounded host procedure with target/remote inspection,
authorized pull and human conflict review, explicit applicable local refresh,
and confirmed/unknown outcome handling. The remaining skill set and M5 parity
still need their separate delivery; this is not full memhub workflow parity.

Before #139 the CRUD row's `port (§6)` still left ordinary human fact/decision
commands deferred. After it, the repository human subset in §11.2 ships,
alongside the unchanged reviewed agent lane. It does not complete every CRUD
or parity item: global memory/promotion, top-level history/status/stats and
the remaining matrix still need their explicit delivery/disposition.

Before #140 the JSON interop and legacy import rows above remained planned.
After it, native memdolt interop v1 and explicit tagged memhub-v1 import ship
under §15's supported-payload, fresh-target, file and partial-progress rules.
Native ULID/null row images are distinct from numeric-ID memhub input; no
reverse compatibility or JSON sync is claimed. The nullable v0.2.2 note
metadata import is included; transcript archives and global migration remain
excluded. This does not complete the remaining parity matrix.

---

## 13. Hub deployment & operations

### 13.1 The hub is one process

**Before issue #147**, the design used the following SQL-bind-only example and
tailnet-perimeter description. Preserve them as the historical before-state:

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

**After issue #147**, native Dolt 1.88.1's remotesapi is explicitly accounted for:
it binds `:port` independently of SQL `listener.host`. The example above does
not enforce private remotes ingress. The measured managed-startup baseline is
exactly **1.88.1**, not the historical ≥1.30 capability statement or an unmeasured
compatibility range. Remotes wire-format metadata is not a Dolt release version.
This delivery adds no skew guard to existing clone/push/pull clients or doctor.

**Doctor compatibility subset (issue #167).** The #147 before-state above left
doctor without release evidence. After #167 ordinary doctor adds the embedded
Dolt release from the pinned `dolt/go/cmd/dolt/doltversion.Version` and compares
it with the measured exact **1.88.1** baseline, failing unsupported/unobservable
evidence. The driver environment label and an uninitialized embedded SQL version
string do not identify that release. The five existing local checks and offline
default remain. Explicit `doctor --hub --config <absolute-hub.json>` reuses
`hub.Inspect` status, retaining observed native release, failures and unknowns
without opening repository memory or changing deployment. It requires the same
Linux deployment/file/nftables inspection privileges as `hub status`.

Explicit `doctor --remote <name> --dir <repository> [--user <username>]` reuses
`RepoStatus` through the existing direct/authenticated owner route. It requires
an initialized store, fetches only the selected committed remote main, and
checks schema and project identity. Main, proposals, tags, working memory,
derived indexes and configuration remain unchanged; fetched objects and the
selected tracking ref may remain even on refusal. A username overrides the
stored username; only the executing owner's `DOLT_REMOTE_PASSWORD` supplies a
password. No password flag, config value, IPC field, replay or personal Dolt
credential loading is added. Conflicting/incomplete selections refuse before
inspection. Hub selection cannot combine with repository `--dir`, `--remote`
or `--user`; `--config` requires `--hub` and `--user` requires `--remote`.

Remote transport/schema/identity evidence never establishes a remote executable
release: remotes metadata carries storage formats, not native version. Doctor
warns that release is unobserved and names the on-hub doctor command as the
inspection remedy. Both absent identities warn without changing RepoStatus's
legacy acceptance; conflicts or unassessed divergence remain visible advisories.
The local release check binds this doctor's running binary, not a potentially
different owner binary. No transfer path gains an inferred remote version guard.
The [hub runbook](../hub-deployment.md#doctor-compatibility-issue-167) names every
changed symbol/file and preserved boundary. There is no dependency, schema,
live-SQL store, MCP registration, installation or credential change. §13.2's
backup-age/headroom/trend checks, full parity and the physical two-client gate
remain unexecuted/separate.

`hub init` generates exact nonsecret YAML, a dedicated-user native systemd unit,
a separate privileged boundary unit, an nftables file and setup instructions.
The generated deployment requires Linux/systemd/nftables and both configured
private address families on its chosen interface. The boundary atomically creates
only its own `inet memdolt_hub` input table; it refuses an existing table and
never flushes the ruleset or unrelated traffic. The four rules permit loopback
or the selected private interface/source prefix/destination on both configured
TCP ports, then drop remaining traffic to those ports on both IP families.
Other chains can deny more; accepts elsewhere cannot override the drop.

Every generated server start checks trusted exact artifact bytes and the applied
nftables table, refusing missing/unreadable/malformed/unapplied/dormant/weakened
protection. The root check runs no Dolt command; native version, private privilege
file existence and bounded address readiness run as the unprivileged service
account before one native server. `ready` verifies the effective nonroot UID/GID
against the configured account/group, including refusal of root-ID aliases;
status and privileged preflight retain their distinct identities.
Explicit absolute paths/names/addresses are
validated before interpolation; no shell or password argument is used. The
unit orders/binds after the boundary and selected private-network service and
uses bounded visible failures/retries. No implicit install/firewall/account or
database mutation occurs from the hub CLI. Native event flushing is disabled in
generated server/probe environments. Ordinary data schema/history stays intact.
The native version probe uses a temporary home/cwd and native metrics/update-check
disable settings, retaining strict parsing without contacting GitHub or changing
the operator's configuration; cleanup failures are visible. Nft probes retain
their read-only applied-table behavior without temporary configuration.

Only applied protection passes full preflight; `--files-only` validates artifacts
before application and is not a server startup authorization. Stop leaves the
table applied. The guard is a startup snapshot, not a monitor: a later root
firewall writer can change protection, and the final check/start interval is not
atomic against root. Do not enable a distribution nftables.service that flushes
the machine ruleset; preserve this table in other firewall reloads and stop Dolt
before changing it. Direct native server invocations do not inherit the guard.

`hub status` reports observed release/configuration/private-interface/applied
boundary and local TCP listener checks with failures/unknowns; it cannot prove
process identity, credential correctness or physical off-network denial.
Non-Linux live inspection/startup is explicitly unsupported. Native credentials
are bootstrapped through loopback-only native SQL and prompted/environment flows,
then persisted privately outside artifacts: CLONE_ADMIN is global remote-read
authority and SUPER is broad remote-write authority, not database-scoped isolation.

The [hub runbook](../hub-deployment.md) records the complete artifact/symbol/file/
CLI/test/CI structural blast radius, output preservation and private path checks,
native SQL/grant commands and exact enforcement limits. Existing local stores,
owner routing, all MCP tools, transfer/merge/auth and code-index behavior remain;
no Go dependency, durable schema change or inference is added. Topology/project
identity, backup/retention, physical hub deployment and two-client/off-network
acceptance remain separately tracked. The earlier physical/ARM64 evidence is not
expanded by this isolated Linux-amd64 gate.

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

Before issue #140, the following paragraph specified the planned surface,
which had not yet shipped:

`memdolt import --from-memhub <export.json>`: consume memhub's export v1 JSON (facts, decisions, tasks, commands, session_notes while preserving nullable `session_id`, `agent_id`, `provider_id`, `model_id`, and `variant`, project_state/arch, pending_writes → recreated as proposal branches; writes_log → imported as a single annotated genesis note, not fake history). Docs re-ingested from source files (export excludes them by design). Embeddings and code index rebuild locally. Follow with `eval retrieval` against the ported golden set before trusting recall. The operator's own migration, if ever, follows the r2 §13.1 discipline: converge memhub first, lowest-stakes project first, one week soak, quarantine (`.memhub` renamed, not deleted), old state retained a month.

After #140, `export <bundle.json>`, `import <bundle.json>` and explicit
`import --from-memhub <export.json>` ship with `--dir`/`--json` through the
existing direct/authenticated-owner selection. [Migration instructions](../migration.md)
define the exact formats and operator runbook; AGENTS.md inventories every
touched element and retained behavior.

Native `memdolt_export_version: 1` carries integer source schema 4, one captured
main hash, complete fixed memory-table images and captured pending
head/parent/deltas. Every non-NULL cell is a lossless string (canonical ULIDs,
decimal INTs and UTC DATETIME seconds included); NULL is explicit. Evidence,
alternatives, superseded rows, narratives, counters and exact note text/null
provenance remain. Accepted proposal metadata is committed memory; pending
payloads remain separate. Dolt reconstructs generated live_key. This differs
from numeric-ID memhub v1, is not reverse-compatible with it and is not sync.
Documents/chunks, indexes, config, ownership credentials, transcript archives/
pointers and repository/host identity are excluded.

Before #153, this native round-trip contract had a cold-process failure when
re-exporting an imported pending decision: sorted diff rows could retain lazy
TEXT whose iterator context had already been canceled. After #153, the shared
`repoDiffRows` materializes those cells with the caller's query context. Fresh
direct and authenticated-owner exports retain complete rows, exact NULLs and
pending proposals, including after another import. SQL ordering, immutable
hashes, caller cancellation and all import/review/file guards remain unchanged.
This correction binds that export/status helper alone; the [migration guide](../migration.md#re-export-after-reopening-issue-153)
records the cause, full structural inventory and cold-process regression.

Legacy input follows actual tagged v0.2.0 `src/export/v1.rs`, plus only
v0.2.2's five nullable note fields; declared source schemas 1-24 are supported.
Older absent optional arrays/fields use original serde defaults. Only
ID-bearing target tables receive ULIDs; commands map to the actual kind key.
Supersession references map consistently. Task/note prose stays exact: neither
v1 task schema has a structured task-link column to translate. Confidence is
explicitly omitted because §6.1 removed it. Invalid versions, shapes, duplicate
members/identities, dangling/cyclic links, ambiguous live keys under the
destination collation and unrepresentable timestamps/widths refuse before
writes. Opaque source provenance never supplies import or review authority.

Before #159, the "declared source schemas 1-24 are supported" claim above
described numeric-string acceptance only. Tagged memhub exporters copy the
stored migration identifier verbatim, so otherwise supported exports naming
`0023_session_transcripts` (v0.2.0) or `0024_session_note_provenance` (v0.2.2)
refused; the synthetic fixture's `"24"` masked that mismatch. After #159,
`supportedMemhubSchema` admits the exact 24 names in those tagged migration
lists as well as the existing numeric strings in 1-24, including leading zeroes
and optional `+`. `memhub_export_version` remains integer 1; source schema
remains a JSON string. Unknown/malformed names and newer versions refuse before
memory/ref changes. The original declared string survives in the genesis note.
This check binds `decodeMemhubExport` and the legacy `Store.ImportMemory` path,
including authenticated-owner execution, not native headers or every Store write.
All other import guards remain; the [migration guide](../migration.md#tagged-source-schema-headers-issue-159)
records the complete touched-element inventory and tagged fixture provenance.
Fresh-process synthetic tests import/reopen/export both headers through both
routes with mappings, nullable note provenance and pending payloads intact.
The unchanged real export clears this header check through direct and verified
owner execution but still refuses duplicate command kinds without changing
source or destination. Explicit source command reconciliation remains required;
successful real migration, adoption and soak are not claimed. Frozen retrieval
golden files and assertions remain unchanged.

Two actual model differences need explicit disposition. memhub commands are
identified by `(kind, cmdline)`, while §6.1 permits one current command per
kind. Legacy pending supersede names existing fact/decision `old`/`new`
identifiers, while staged supersede inserts a fresh fact replacement. #140
does not change these models or silently select/drop rows: duplicate command
kinds, pending legacy supersedes, unsupported/global pending targets and
native proposals whose before-image conflicts with exported main refuse the
whole bundle. Reconcile them in a retained source copy through human review,
then export again. Supported pending legacy facts/decisions and native
ordinary insert/overwrite/fresh-fact-supersede shapes become inspectable
one-commit proposals under all existing review guards; import never accepts.

Before this replacement, memhub import offered force-wipe and removed target
writes_log even on its docs-only path. After #140, memdolt has no force/wipe/
history-rewrite path. It requires current initialized empty durable memory,
no proposal branches and clean main without merge/conflict state. Existing
documents/config/derived/render artifacts and prior Dolt history remain.
Changed main is one real current-human-authored import commit. Legacy
writes_log and historical accepted/rejected/expired pending rows become counts
in one annotated digest/count genesis note, not fabricated history. Pending
raw actor/provenance remain in that annotation. Recreated proposal commits
also belong to the current importer at the actual import time; their original
actor/time remain row metadata. No host root becomes durable identity.

ExportMemory/ImportMemory share the existing owning Store mutation mutex;
foreign Dolt sessions, other reads and migrations retain their prior boundaries.
Export captures main and proposal heads together and reads immutable hashes
throughout. New bundle file APIs reuse rooted renderer preparation/sync/
identity checks and native per-file replacement. They require an existing
local parent, reject protected plumbing, opened credential aliases,
symlink/reparse traversal and registered doc source destinations, and preserve
old output on preparation failure. Existing outputs need a supported native
header. A separate export lock coordinates cooperating generations; crash
residue requires inspection with exports stopped. No export backup,
foreign-writer compare-and-swap or stronger crash guarantee is claimed.
Renderer configuration, backups and its two-file behavior remain unchanged.

Import reads only the selected bundle, opens no metadata pointers, and
prevalidates/deny-scans all persisted text/provenance before bound writes.
Its decoder reuses #137's shared Unicode validator before tokenization,
including nested legacy pending JSON; valid pairs and opaque case-sensitive
provenance remain. Exact format-member spelling refuses struct aliases before
encoding/json can overwrite them. Typed owner paths are checked before
marshaling and the existing raw operationArgs Unicode guard remains in force.
Main may commit before proposal creation or finalization fails. Results retain
its confirmed hash, planned identity map and exact created/remaining proposal
prefix. Imported staging alone retains/returns a confirmed branch on late
failure; ordinary staging keeps its prior cleanup/residue policy. The owner
submits the complete operation once and preserves populated results/errors.
Lost replies or unconfirmed commits report unknown, not proof of no write:
inspect main, rows, proposals and files, never auto-replay, and use a fresh
target for a reviewed new import attempt. No import/export MCP mutation tool,
dependency or durable migration is added.

Only synthetic migration and real derived-index/recall/golden checks are
exercised. The user's own migration remains optional and unperformed.
Converge-first, low-stakes-first, one-week soak, quarantine and one-month
old-state retention still hold. Full M5, global migration and the physical
hub acceptance gate remain separate.

---

## 16. Milestones & gates

| M | Scope | Exit gate |
|---|---|---|
| **M0 — Spike (go/no-go)** | Embedded-driver soak: MCP-server-owns-store + CLI-routes-through-IPC under concurrent load, incl. unclean-kill/stale-LOCK recovery. ONNX-in-Go: embeddings + rerank scores match memhub within tolerance on a probe corpus (tokenizer ids byte-identical). Retrieval rig: golden set on Dolt FULLTEXT + brute-force cosine. Hub rig: sql-server + remotesapi on the Pi or Linux box, clone/pull/push/merge round trip over Tailscale from two machines. Resolve every **[verify]**: ARM64 release asset, gc flag semantics, shallow clones, Linux-AMD64 onnxruntime bundling, tokenizer lib, current vector-index status. | Recall@K ≥ memhub baseline (or the BM25 contingency proves it); zero data-loss events in the concurrency soak; push/pull round trip clean. **Fail → project stops; write up findings.** **Decided 2026-08-02: GO** — all three conditions met, each in a narrower scope than its wording suggests, and two of the scope column's six `[verify]` items (gc flag semantics, shallow clones) did not complete. Condition-by-condition evidence, the limits the GO carries, and the obligations it leaves open: `docs/spikes/m0-gate.md` |
| **M1 — Core** | init, schema+migrations, CRUD all lanes, proposal branches, review CLI (accept/reject/expire/stale, contradiction probe), commit conventions, doctor basics, deny-list. | Lifecycle test suite green; a full propose→review→merge cycle audited via `dolt_log`. |
| **M2 — Retrieval** | Embedding side-store, vector-only recall candidate gather + warnings, rerank, eval harness, staleness machinery, `search`; FULLTEXT remains available to the text-search surface. | Golden gate green in CI (hermetic fixture). |
| **M3 — MCP** | Full tool surface, 2026-07-28 behaviors, elicitation review loop + fact-key conflict flow, server instructions, `.mcp.json`, native OpenCode V2 registration/doctor/identity, verified OpenCode wrap-up provenance, skills for 3 agent CLIs. | Previous gate, replaced 2026-09-06 as recorded below: End-to-end session from real Claude Code: recall, propose, elicited review, task ops. |
| **M4 — Hub & repo ops** | remotes config, pull/push/repo-status + conflict elicitation, hub init/systemd docs, auth setup, version-skew guards, topology config; `Store` remote impl (topology B) if time allows. | Two-machine round-trip acceptance test (r2 §13.6 analogue): fixture data, write from both machines, merge, verify counts/hashes; re-open with plain local memdolt — no conversion (D13 gate). |
| **M5 — Parity long tail** | docs ingestion, code index + locate + eval, render, global store, import-from-memhub including session-note provenance, audit md, ingest-git. | Parity matrix (§12) fully dispositioned; locate golden gate green. |
| **M6 — Ops polish** | backups + doctor --hub, gc/retention, upgrade machinery with safe wrapper resync, token accounting (gated), validated unredacted OpenCode transcripts, README/status discipline. | Quarterly-drill-style restore test documented and executed once. |

**M4 diagnostic subset (issue #167):** before this slice doctor lacked embedded
release, selected hub and selected remote diagnostics. After it, §13.1's explicit
checks and isolated CLI/native Linux rig cover that subset. The exact baseline
is 1.88.1; no compatible version range is inferred. These checks do not execute
or replace the physical two-client counts/hashes/reopen gate above, complete M4,
or deliver M6's backup/disk diagnostics and restore drill.

**M3 exit-gate replacement (2026-09-06, issue #119).** The previous gate above
required a real Claude Code session. The user canceled their Claude subscription
and explicitly waived live Claude acceptance. The approved replacement is
passing deterministic compatibility checks for registration, stdio, recall,
proposal isolation, review, task operations, and actor attribution, plus a real
OpenCode wrap-up with a host-provided current session ID independently matched
to the API and one explicitly approved synthetic note whose persisted text,
nullable provenance, and canonical/raw actor are verified. The evidence and
reproduction commands are in [the M3 acceptance report](../spikes/m3-acceptance.md).
Claude compatibility is expected from official host documentation and these
checks; actual Claude recall, proposals, task operations, and human elicitation
remain unverified. This replaces M3's acceptance evidence only; the phased tool
surface in §11.1 is unchanged. At that gate replacement, M4–M6 remained deferred.

**Host workflow subset (issue #155):** before this slice, the six generic
workflow templates lacked guided project bootstrap and continuing narrative
updates. After it, §11.5's seven namespaced entry points, portable shared
procedure and approved narrative continuity ship. Copied-package and disposable
CLI checks exercise content, attribution and preservation after reopening.
They do not establish live three-host compliance, replace M3's acceptance
record, complete the M5 parity matrix or change physical M4/M6 gates. Catch-up,
optional user migration, global proposal acceptance and runtime upgrade/audit
work remain separate.

**Catch-up workflow subset (issue #157):** the #155 scope record above left
catch-up separate. After #157, §11.6 adds it through existing operations and
the same portable host layout. Package-copy and disposable direct/owner FTS
recipes exercise resource resolution, pull, explicit view refresh and reopened
context/queue. They do not prove live human-approval compliance, real hybrid
catch-up, physical two-client acceptance or completion of M4/M5/M6. Optional
user migration, global proposal acceptance and runtime audit/upgrade remain
separate work.

**M4 Linux hub subset (issue #147):** the historical subset records below left
hub setup, authentication instructions and measured startup compatibility pending.
After this slice, §13.1's reviewable artifacts, fail-closed private startup checks
and hub status ship. The new Linux CI lane verifies generated unit grammar and
actual ordered startup arguments with a checksum-verified native 1.88.1 in a
disposable container/network namespaces, including real dual-stack allowed/denied
traffic, preservation and refusal cases. It does not install under PID 1 or touch
the runner-host firewall. Ordinary/golden gates retain their existing contexts.
The physical two-client round trip/off-network acceptance, topology/project
identity, backups/retention and other M4–M6 requirements remain separate.

**M4 repository identity/topology subset (issue #163):** the preceding records
left identity and topology pending. After this slice, §§5.3/11.3's Git-derived
identity, explicit existing-store adoption, protected local/clone configuration
and opt-in owner startup pull ship. Disposable cross-root native transfers
preserve rows, identity and real history; direct/owner/MCP refusals cover drift,
collisions, invalid configuration and startup failure. This is not physical
two-client/private-network acceptance or measured hub/client compatibility.
Those M4 checks, optional live SQL, machine registry/upgrade, M5 and M6 remain
separate; no current user's memory or live installation is changed.

**M4 first subset (issue #123):** local-only `repo status` now ships as described
in §11.2. Remotes configuration, remote status/diff, pull/push, conflict
elicitation, hub init/systemd documentation, authentication setup, hub/client
version-skew guards, topology configuration, and the optional remote `Store`
implementation remain pending. This offline inspection slice does not satisfy
or replace the two-machine round-trip/no-conversion acceptance gate above.
M5 and M6 remain deferred.

**M4 clone subset (issue #125):** the earlier offline-status subset left all
transfers pending. Clone bootstrap now ships as described in §11.2; its
filesystem round trip and local synthetic authentication checks add evidence
without replacing the two-machine round-trip/no-conversion gate. Application
push/pull, remote status/diff, configuration editing, conflict elicitation,
hub setup, measured hub/client version compatibility, topology configuration
and the optional remote `Store` implementation remain pending.

**M4 main transfer subset (issue #127):** before this slice, the #125 record
above left application push/pull pending. After it, main-only push and
validated fast-forward pull ship as bounded in §11.2. Local synthetic direct
and authenticated-owner tests add transfer and preservation evidence; they
do not replace the two-machine round-trip/no-conversion gate or establish
hub/client version compatibility. Remote editing/status/diff, divergence
merge/conflict elicitation, hub setup, topology configuration and the optional
remote Store remain pending. M5 and M6 are unchanged.

**M4 remote configuration subset (issue #129):** before this slice, #127
left remote editing pending. After it, additive `repo remote add` and local
`repo remote list` ship with the boundaries described in §§11.2–11.3. Local
synthetic direct/owner configuration and named-transfer checks add evidence;
the two-machine round-trip/no-conversion gate remains unchanged. Remote
status/diff, divergence merge/conflict elicitation, hub setup/authentication
operations and measured version compatibility, topology configuration and
the optional remote Store remain pending. M5 and M6 are unchanged.

**M4 remote inspection subset (issue #131):** before this slice, #129 still
left remote status/diff pending. After it, remote-aware `repo status`, explicit
offline `--local`, exact committed `--diff`, and the real `repo_status` MCP
tool ship with §§11.1–11.2's captured-snapshot, retained-fetch and rollback
boundaries. Local synthetic tests cover direct/authenticated-owner and
modern/legacy MCP operation, conflicts/constraints and preservation/refusals.
Divergence resolution, hub deployment/auth setup, measured version acceptance,
topology configuration and the optional remote Store remain pending. The
two-machine round-trip/no-conversion exit gate is unchanged; this slice does
not complete M4.

**M4 divergence subset (issue #137):** before it, #131 still left divergence
resolution pending. After it, shared CLI/MCP pull merges and complete explicit
conflict choices ship within §§6.3/11.1/11.2's atomicity, provenance, confirmation
and owner boundaries; `repo_push` is also real. Synthetic local acceptance is
not hub deployment, version compatibility or the two-machine/no-conversion
exit gate. Optional live-SQL topology, global promotion and full M4 remain
separate; M5/M6 and the remaining parity matrix are unchanged.

**M5 repository document subset (issue #132):** the earlier milestone records
left all M5 work deferred. Repository doc add/ls/show/rm and real MCP `doc_add`
now ship with §11.2's boundaries and existing schema/retrieval/index contracts.
Global documents, the global backend, code index/locate/golden coverage and
the remaining M5 parity items remain separate; this delivery does not satisfy
the full M5 acceptance gate or change M6.

---

**M5 render subset (issue #133):** the earlier records left all M5 deferred.
Issue #145 subsequently preserves direct-lane late results and adds session-note
flush before explicit rendering (§3.1); it does not complete M5 or M6.
After this delivery, committed-memory rendering through CLI and MCP ships
within §§11.1–11.3's read/file/owner boundaries, with the three core workflow
templates updated. Synthetic direct, authenticated-owner and modern/legacy
MCP checks cover its content and preservation contract. This does not complete
the parity matrix or the locate golden gate; other M5 capabilities and the
gated M6 token/transcript workflows retain their prior phasing.

**M5 code-index subset (issue #138):** before this slice, the preceding
records left code indexing, locate and its golden gate deferred. After it,
§9's local-only code surfaces and the unchanged golden formats ship. Protected
Ubuntu CI runs the exact production locator gate with verified model-cache
reuse alongside the preserved memory retrieval/scale gate. On the complete
corresponding Rust benchmark corpus the full bar is 18/18; the exact tagged
polyglot fixture passes 17/17. Both floor-0 harness safety checks reject 2/2
probes while default fusion reports 2/2 leaks. The separate complete-v0.2.0
17/18 diagnostic preserves the stale-path evidence and does not lower the
benchmark bar. Git-ingested history, global memory, public memory commands and
remaining M5 parity stay separately tracked; this does not satisfy full M5.

**M5 human memory subset (issue #139):** before this slice, the remaining
CRUD/parity work included the ordinary trusted human fact/decision commands.
After it, the repository operations and direct/owner/read/history checks in
§11.2 ship. The retrieval golden gate remains required. This does not complete
the parity matrix, global memory/promotion, top-level history/status/stats,
locate acceptance or the other M5/M6 capabilities. Earlier subset deferrals
remain the historical record, not a claim that those follow-ups shipped here.

**M5 memory interoperability subset (issue #140):** before this slice, JSON
interop and import-from-memhub remained planned. After it, §15's bounded
native/legacy surfaces and synthetic direct/owner round-trip, real index/recall
and unchanged retrieval golden checks ship. Full parity, global migration,
physical hub acceptance and the user's optional migration remain separate;
the existing convergence/soak/old-state-retention runbook still applies.

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

The historical R9 rationale assumes client-minted direct-write IDs. Before
#140 no import operation existed. ImportMemory instead builds legacy mappings
inside the owning operation and returns them with progress; it therefore
inherits no idempotent-replay claim. Its whole-operation one-submit rule,
fresh-target refusal and explicit inspect-before-retry/unknown-outcome report
apply. Existing direct-write and other owner paths retain their prior behavior.

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
