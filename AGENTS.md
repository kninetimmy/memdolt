# memdolt

Local-first CLI + MCP server (Go) giving coding agents durable per-repo
project memory stored in Dolt — a MySQL-compatible database with git
semantics.

**`docs/prd/memdolt-prd.md` is the product authority.** Read it before
proposing design changes; don't silently diverge from it.

## Session Continuity

memhub is the source of truth at `.memhub/project.sqlite`.
The rendered files under `.memhub/rendered/` are the local human-readable
view. They are generated from the DB and ignored by Git by default.
Re-render after `/wrap-up` with `memhub render`.

The PRD is ingested into memhub (54 chunks), so `recall` surfaces the
relevant sections directly — prefer it over re-reading the whole document.

## Build / test / run

Go module at the repo root: `github.com/kninetimmy/memdolt`, Go ≥1.26.2
(the minimum `github.com/dolthub/driver` requires).

**Two build settings are mandatory**, because the embedded Dolt driver
requires them:

- `CGO_ENABLED=1` and a working C compiler (gcc/clang; MinGW-w64 on
  Windows). Dolt's block store imports `github.com/dolthub/gozstd`, a cgo
  wrapper around zstd, unconditionally — there is no pure-Go build.
- The `gms_pure_go` build tag, which swaps go-mysql-server's ICU-backed
  `REGEXP` implementation for the standard library's. Without it the build
  also needs system ICU development headers, which are not available on all
  three supported platforms.

Set them once per shell (or with `go env -w`) so the commands below work
as written; CI sets them at the workflow level:

```sh
export CGO_ENABLED=1
export GOFLAGS=-tags=gms_pure_go
```

- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...`
- Format check: `gofmt -l .` (must print nothing; `gofmt -w .` fixes it)
- Lint: `golangci-lint run` (config in `.golangci.yml`; not vendored —
  install it separately, e.g. from
  https://golangci-lint.run/welcome/install/; CI pins the version it uses
  in `.github/workflows/ci.yml`)
- Run the CLI: `go run ./cmd/memdolt version` (add `--json` for a single
  machine-readable JSON object instead of the human-readable line)
- Run the MCP server: `go run ./cmd/memdolt serve` owns the repository store
  and authenticated IPC endpoint while serving MCP over stdin/stdout. **Before
  issue #103, the root command had no `serve` child and the owner lifecycle was
  exercised only by lower-level IPC tests and the soak; after it, `serve`
  stops protocol handling, closes pending session work, then closes IPC and
  the store.** The complete structural blast radius is:

  - `cmd/memdolt/main.go` now gives Cobra an interrupt/SIGTERM context. Normal
    command execution and the existing stderr plus nonzero-exit error behavior
    still hold; only cancellation delivery is new.
  - `cmd/memdolt/root.go` retains every existing command and adds `serve`.
    `cmd/memdolt/serve.go` alone owns the new long-lived ordering above. It
    opens one `LocalStore`, reuses the existing authenticated IPC handler and
    `localCommandStore`, and closes protocol, pending work, IPC, then store.
    Existing short-lived direct/owner routing, the IPC operation allow-list,
    schema guard, actor propagation, and contradiction-gated `ReviewAccept`
    behavior still hold. The ordering restriction binds `runServe`, not every
    CLI command, store owner, or go-sdk server.
  - `internal/mcpserver/server.go` adds modern discovery, legacy initialize,
    the embedded instructions, and request attribution. Middleware installed
    by `internal/mcpserver.New` applies attribution to every `tools/call` that
    server handles, not arbitrary go-sdk servers or non-tool requests;
    `tools/list` alone receives the static 24-hour `ttlMs` cache hint.
  - `memory.NormalizeActor` is reused and unchanged: empty input and the
    case-insensitive literal `user` select the trusted human CLI actor, while
    every other accepted identity becomes `agent:<name>`. That behavior binds
    every direct caller of `NormalizeActor`; it is intentionally not sufficient
    by itself at the MCP trust boundary. **Before the review-cycle fix, MCP
    `clientInfo.name = user` inherited that CLI-only mapping and could claim
    human provenance; after it, `mcpserver.actorFor` forces every present,
    accepted modern or legacy MCP identity into the agent class, so `user` and
    already-prefixed variants are `agent:user`.** This MCP-specific rule binds
    `actorFor` calls reached by `New`'s `tools/call` middleware alone, not CLI
    callers or every `NormalizeActor` use. Missing identity still becomes
    `agent:unknown`; raw provenance is retained; raw `cli` still becomes
    canonical `agent:opencode`; and the existing raw/canonical length and
    invalid-name errors still fail closed.
  - `internal/mcpserver/instructions.md` is a new checked-in policy artifact
    and replaces no prior instructions. `server_test.go` and
    `cmd/memdolt/serve_test.go` are additive protocol, attribution, stdio, and
    lifecycle coverage; they change no production behavior.
  - `go.mod` and `go.sum` add the approved go-sdk v1.7.0 graph and its selected
    transitive upgrades. Existing Dolt, CLI, retrieval, and inference
    dependencies and behavior still hold. This `AGENTS.md` record preserves
    the before-and-after above; all other guidance and its managed block are
    unchanged.

  **Before issue #104, `serve` had the protocol and attribution foundation but
  registered no memory tools. After it, production registers exactly the 15
  real M3 tools named in PRD §11.1's approved phasing; deferred names are absent,
  not stubs.** The complete structural blast radius is:

  - `internal/mcpserver/tools.go` adds `RegisterTools`/`Toolset`, the 15 typed
    handlers, committed-main list readers, and the session note accumulator.
    Existing `internal/mcpserver.New`, discovery, instructions, cache hints and
    `actorFor` behavior still hold. In particular, every handler still receives
    `New`'s tools/call attribution: all MCP identities remain agent-class, raw
    `cli` remains canonical `agent:opencode` with raw `cli` provenance, and a
    missing identity remains `agent:unknown`. The fixed-name restriction binds
    `RegisterTools` alone, not arbitrary go-sdk servers or the destination tool
    table. `list_facts` alone interprets `prefix` as one literal dotted prefix,
    uses an explicit non-backslash LIKE escape so percent and underscore are
    escaped in the bound value while backslash stays ordinary, appends the
    terminal SQL wildcard itself, and retains superseded rows; this is not
    permission for other list filters to accept wildcard syntax.
    `Toolset.flushLocked` attempts every queued actor group even when another
    group fails, removes each success immediately, and retains only failures for
    the orderly-shutdown retry. `Toolset.Close` reports both deadline and final
    failures, then discards any still-failed in-memory rows because a closed
    session cannot retry them. This retry/discard rule binds the MCP `Toolset`
    accumulator alone, not every batch loop or direct-lane write.
  - `cmd/memdolt/serve.go` registers that toolset on the already-open owner and
    closes it after protocol serving but before IPC and the store. The issue
    #103 lifecycle ordering, schema gate and authenticated owner endpoint still
    hold; only `runServe` owns this ordering, not every CLI command or MCP
    server. `cmd/memdolt/serve_test.go` retains the lifecycle and stdio checks
    and now verifies production advertises 15 tools.
  - `internal/memory.Lanes.PrepareNote` mints an in-memory row and
    `CommitNotes` commits one actor's accumulated rows as `note batch (N)`.
    `LogNote` still gives the short-lived CLI one note and one attributed commit;
    task and command validation, ordering, commit messages and attribution still
    hold. Their reads now say `AS OF 'main'` explicitly: before they depended on
    the store session being on main; after they refuse to expose a proposal or
    uncommitted branch view. The note-batch rule binds `CommitNotes` and the MCP
    accumulator alone, not every `Lanes` write. Multiple request actors are
    committed in separate attributed batches because one Dolt commit has one
    author.
  - `store.CommitRequest.RequireClean` is an opt-in guard used by note batches.
    `localdolt.commitConn` still validates and deny-list scans every request
    before writing, but a guarded request also refuses an already-dirty working
    set before its transaction opens, so `DOLT_COMMIT('-A')` cannot sweep an
    unrelated change. The restriction binds requests that set this field alone,
    not every `CommitRequest`, proposal stage or review accept; their established
    clean-working-set rules remain separate. `localdolt.CheckWriteText` exposes
    the same scanner for an early visible MCP refusal, while the real commit
    checks again and remains authoritative.
  - `internal/storeipc.CommitRequest`, `Backend`, `handleOperation` and
    `OwnerStore` carry `RequireClean` and `CheckWriteText` without changing the
    token gate, explicit operation allow-list, one-submit write rule, SQL argument
    binding, error visibility or existing direct/routed parity. These additions
    bind this owner transport, not arbitrary HTTP handlers.
  - `retrieval.Run` now owns config loading, effective-mode selection, inference
    open/close and close-error joining for both surfaces. `cmd/memdolt/recall.go`
    still builds the same options and emits the same response; retrieval ranking,
    filters, warnings, observability and provenance are unchanged.
    `retrieval.FactIsStale` applies recall's floating-point day comparison to
    `list_facts` too, so every positive int64 horizon remains valid without a
    `time.Duration` conversion. This helper binds fact staleness in recall and
    MCP fact listing alone, not every age or retention calculation. `search.Run`,
    `memory.Lanes`, and the `localdolt.Propose*` methods remain the sole business
    logic for the corresponding MCP and CLI operations.
  - `internal/mcpserver/tools_test.go` is additive in-memory client coverage for
    all 15 registered tools and confirms each has non-nil input and output schemas.
    It checks representative successes and visible refusals, main/proposal
    separation, attribution, literal fact-prefix and superseded-row retention,
    recall provenance, note deadlines, and orderly shutdown. Its mixed
    denied/allowed regression proves the first flush attempts both actor groups in
    that fixture despite one failure, leaves only the failed group pending, and
    does not recommit the successful group when `Close` reports the failure and
    discards pending rows. It does not directly observe the failed group's shutdown
    retry attempt. More precisely, this file asserts the exact tool names, non-nil
    input and output schemas for all 15 tools, recall's exact source-type
    description, deferred-name absence, structured successes and visible refusals,
    task and note attribution, proposal/main isolation, command lookup,
    duplicate-key and file-search refusals, superseded fact retention, literal
    percent/underscore/backslash/escape-character prefixes, max-int64 fact
    staleness, recall fact provenance, and advertised `doc_chunk` acceptance. It
    does not claim exhaustive coverage of every ordering, filter, warning, or
    schema structure.
    `server_test.go`
    keeps the backend-free `New` foundation expectation but updates its diagnostic
    wording. `localdolt/note_batch_internal_test.go` and the added
    `storeipc_test.go` assertion cover dirty-working-set and routed deny-list
    preflight behavior. They change no production behavior. PRD §§3.1 and 11.1
    preserve the batching and phasing before/after records; no dependency, schema
    migration, deferred backend, elicited review, host registration or provenance
    workflow is added.

  **Before issue #106, production advertised those exact 15 tools,
  `propose_fact` returned a live-key collision directly, and elicited review was
  still absent. After it, the 15 tools and their behavior remain, and
  `review_pending` is the sixteenth tool; an elicitation-capable
  `propose_fact` also offers the approved same-key dialog.** The complete
  structural blast radius is:

  - `internal/mcpserver/tools.go` keeps every previous handler and note-batch
    rule, registers `review_pending`, routes same-key `propose_fact` calls into
    the new dialog, and clears pending elicitation rows when `Toolset.Close`
    runs. `cmd/memdolt/serve.go` still registers that one toolset on the live
    owner, so discovery, cache hints, agent-only attribution, shutdown ordering,
    and note flush/retry/discard behavior still hold. The exact-sixteen rule
    binds `RegisterTools` alone, not arbitrary go-sdk servers or deferred names.
  - `internal/mcpserver/elicitation.go` owns both dialogs and their
    `pendingElicitation` shape; `elicitation_state.go` stores each one as a real
    row in process-local in-memory SQLite. Each 256-bit cryptographically random
    `requestState` is stored only as a SHA-256 lookup hash, expires after two
    minutes, is atomically deleted before response interpretation, and is bound
    to the repository data directory, attributed MCP client, exact proposal IDs
    and staging commits, queue position, and action. Before the cycle-1 fix,
    proposal IDs and position were stored but the displayed commit was not, so a
    branch reset to another single commit under the same ID could pass approval;
    after it, every displayed commit is stored and passed into the
    proposal-mutation critical section before merge. Missing, expired,
    mismatched, forged, replayed, malformed,
    incomplete, declined, or canceled responses cannot promote or discard a
    proposal. Authorization-state insert or consume failure happens before
    promotion and fails closed. Continuation bookkeeping is different: its
    progress update runs after a selected proposal may already have merged, so
    a failure stops traversal and reports that accepted prefix rather than
    claiming the merge did not happen. Before issue #106, `Toolset` held only
    pending note groups; after, it also owns this short-lived relational state,
    which restart or `Close` destroys. This discipline binds states minted by
    this toolset alone, not every go-sdk `requestState`; no Dolt migration,
    persistent side-store file, or embedding-side-store table stores approval
    material.
  - `review_pending` offers repo proposals oldest-first. Modern 2026-07-28
    successive review performs at most nine input rounds per call and returns a
    single-use, expiry-bound continuation cursor carrying the untouched snapshot
    and progress; using it reaches proposal ten without re-showing skipped
    entries. Before the cycle-1 fix, the queue was truncated to nine and a later
    call restarted at the same skipped entries, making the tail unreachable.
    Genuinely legacy sessions use one form elicitation containing an approve or
    skip field for every shown proposal, because the SDK legacy shim reinvokes a
    handler only once; batch mode remains one form round on both protocols.
    Batch approval is sequential, not atomic: each successful accept lands, and
    a later guard refusal stops with the accepted prefix still durable. The
    dialog and result now state that partial-progress rule; before the fix they
    incorrectly promised that cancel or failure left the whole batch pending.
    Global proposals never enter a dialog. Every terminal result reports their
    count and terminal `memdolt review` remedy; a mixed queue on a client without
    form elicitation reports both repo and global counts. Empty elicitation
    capabilities retain the protocol's assumed-form compatibility, while a
    URL-only capability gets the CLI fallback. The global exclusion binds
    `review_pending` alone: `list_proposals` and the CLI remain able to see and
    discard global proposals, subject to the shared mutation and cleanup rules
    below.
  - `localdolt.Store.proposalMu` is the one memdolt-owned proposal-mutation
    boundary on a repository Store. Before the cycle-2 fix, expected-commit
    validation excluded only another accept; staging, reject, and expiry could
    mutate the same branch after validation. After it, `stage`,
    `AcceptProposal`, `RejectProposal`, and `ExpireProposals` share the mutex, so
    one of those operations finishes before another can act on the branch.
    `PendingProposals` and `ProposalDiff` remain reads, direct-lane commits still
    move `main` independently, and a foreign Dolt session does not share the Go
    mutex. Before the cycle-3 fix, expected-commit acceptance shared the same
    eager cleanup as CLI accept, reject, expire, and failed staging: it read the
    branch head, compared it with the observed commit, then called
    `DOLT_BRANCH -D`. Dolt v1.88.1 exposes branch heads through the read-only
    `dolt_branches` table and gives `DOLT_BRANCH -D` only a branch name, so a
    foreign session could move the branch after that read and lose unseen
    content in the unconditional delete. After the fix, any `AcceptProposal`
    call with non-empty `ExpectedCommit` merges only the displayed hash and
    never deletes the branch. Production MCP is the caller with that option;
    the unchanged merged branch becomes cleanup residue hidden by
    `PendingProposals`, while a foreign commit makes it pending again and is
    never removed by elicited acceptance. This no-delete restriction binds
    expected-commit accepts alone, not every `AcceptProposal` or review verb.
    CLI accept, reject, expire, and abandoned-stage cleanup intentionally retain
    their prior automatic `deleteProposalBranch` call. Its read/compare catches
    a foreign change before the final read and `proposalMu` excludes memdolt-owned
    races, but no atomic expected-head deletion exists, so those CLI/cleanup
    paths do not claim protection from a foreign change in the final interval.
  - Confirmed MCP acceptance calls `ReviewAcceptExpected` as reviewer `user`
    with `force=false`; `internal/review.AcceptExpected` carries the displayed
    commit into `localdolt.AcceptOptions.ExpectedCommit`.
    `localdolt.AcceptProposal` compares that commit inside the proposal-mutation
    boundary before any merge. Its contradiction validation
    and inference, accept-time deny-list scan, one-commit and supersede-shape
    checks, conflict/constraint verification, and reviewer-authored merge still
    run. The expected-commit/no-force/no-automatic-delete rule binds elicited
    MCP accepts only. CLI `ReviewAccept` passes no expected commit, still
    attempts post-merge branch deletion, and CLI `review accept --force`
    retains its prior operator behavior.
    `cmd/memdolt.localCommandStore` implements both application seams, while
    `runServe` gives owner IPC the expected variant; command selection,
    application config loading, and the existing CLI output remain unchanged.
  - CLI `AcceptProposal` retains its established post-merge contract: a branch
    cleanup failure returns the populated result proving `main` moved together
    with the error. Expected-commit acceptance now returns its populated result
    with the merged branch deliberately retained, so that policy is not a
    cleanup failure. The authenticated owner wire still preserves any populated
    result-plus-error returned by the application gate. Before the cycle-2 fix,
    successive, batch, and legacy
    elicitation discarded that result and reported the proposal as blocked.
    After it, each mode records the accepted proposal and merge, reports the
    cleanup error separately in `failures`, returns `cleanup_failed`, and stops
    without attempting later entries; earlier batch acceptances remain reported
    too. Before this fix, `PendingProposals` treated every physical proposal
    branch as pending, so an unchanged branch left after a landed merge could be
    offered again. After it, a branch whose current head is reachable from
    `main` is cleanup residue and is excluded from pending results, while a branch
    changed to an unmerged head remains pending for review. This reachability
    filter binds `PendingProposals` and its callers, not `ProposalDiff`, reject,
    expiry, or the physical branch itself.
    Before the cycle-3 terminal fix, `reviewTerminal` replaced all of that
    landed-result and cleanup-error evidence with an empty tool error if its
    final `PendingProposals` recount failed. After it, the accepted prefix,
    status, skips, and failures are primary; repository/global counts are
    best-effort, `recountError` names a failed refresh separately, and the
    terminal remedy sends the operator to `memdolt review` rather than
    presenting zero counts as current. This result-preservation rule binds
    `reviewTerminal`; the initial queue snapshot in `startReview` still fails
    closed if `PendingProposals` cannot run.
  - On a live repository key, `propose_fact` shows the current and proposed
    facts and binds its response to both. Before the cycle-1 fix, the handler
    re-read the shown row and then separately called a staging method, so main
    could change between comparison and branch cut; after it,
    `localdolt.Store.ProposeFactResolution` receives the exact nullable row
    image, cuts its proposal branch from main, and compares that image on the
    new branch before applying the selected write. A change before branch cut
    therefore deletes the abandoned branch and stages nothing; a main change
    after the cut remains an ordinary accept-time merge conflict. Overwrite
    stages an in-place value/source/kind/evidence update and clears
    `verified_at`; supersede stages the existing link-first/replacement-second
    shape; keep-both validates and inserts a distinct dotted key. Cancel,
    decline, malformed input, changed current row, or missing elicitation writes
    nothing. This expected-snapshot rule binds `ProposeFactResolution` alone,
    not every `ProposeFact`, `ProposeSupersede`, or fact write. Fresh-key staging
    and all three original staged tools retain their prior contracts, and every
    successful conflict choice remains a one-commit proposal off `main`.
  - `storeipc.Backend`, the explicit operation allow-list, and `OwnerStore`
    carry the expected-snapshot fact resolution and expected-commit review
    fields without changing authenticated ownership, actor propagation, bound
    SQL arguments, visible errors, or the one-submit/no-retry write boundary.
    The review operation now carries a populated post-merge result and cleanup
    error in one authenticated response; `OwnerStore` restores the same
    result-plus-error Go contract without retrying the write. Ordinary pre-merge
    failures retain the existing non-200 error path. These additions bind that
    owner transport, not arbitrary HTTP handlers or other operation results.
  - `internal/mcpserver/elicitation_test.go` adds modern MRTR and legacy-shim
    coverage, real one-round legacy multi-proposal review, proposal-ten cursor
    traversal, mixed and URL-only fallback, partial batch progress, the two
    storage-failure phases, request-state attacks, and atomic fact-conflict
    outcomes. It now also covers populated-result cleanup failure in modern
    successive, batch, and genuine legacy review, including the accepted prefix
    and pending tail, plus terminal recount failure in all three modes and a
    prior successful batch acceptance. `localdolt/review_mcp_test.go` retains the pre-response
    successive/batch reset refusals; deterministically pauses after
    expected-commit validation for reject and expiry serialization; resets and
    amends externally after that validation to prove only the displayed hash
    merges and changed branches are retained; deterministically inserts foreign
    content at the final post-merge cleanup boundary to prove expected-commit
    acceptance removes nothing; and injects CLI cleanup failure to prove the
    result is populated and an unchanged merged branch is not pending.
    `storeipc_test.go` proves that result-plus-error contract
    survives the authenticated owner route. These tests exercise the named
    seams and do not claim to coordinate a foreign process inside the Go mutex
    or exhaust every Dolt branch operation. `tools_test.go` and
    `cmd/memdolt/serve_test.go` still expect the prior 15 plus
    `review_pending`; `server_test.go` only exposes client options to these
    fixtures; `storeipc_test.go` covers routed resolution and expected-commit
    parity; and the callback signature updates in doctor/soak fixtures change no
    shipped behavior. PRD §11.1 preserves the matching before/after. No new
    dependency, persistent side store, durable schema, host registration,
    global promotion, or provenance schema is added. CLI and MCP use the same
    mutation, acceptance-guard, result, and pending-list paths; only their
    post-merge cleanup differs intentionally as stated above.
- Create a store: `go run ./cmd/memdolt init` makes `.memdolt/dolt` beneath
  the current directory (`--dir` points it elsewhere) and applies every
  schema migration the store is missing, one Dolt commit each (PRD §6.1,
  §6.2, §6.4). It is idempotent: a second run adds nothing to the Dolt
  history.
- Check a repository: `go run ./cmd/memdolt doctor` reports the store
  lock's ownership state (held, an orphaned record, absent), whether a live
  owner answers on its IPC endpoint, and whether the store's schema is
  newer than the binary (PRD §5.2.4, §6.4). It also names the machine-local
  empty-recall count and rate from PRD §8.1. Against a directory with no
  store it reports the absence without initializing a store or creating its
  directory. With no live owner, it opens an existing store directly to read
  its schema; this briefly takes the ownership lock and may create
  `.memdolt/LOCK`, but makes no durable database change. It exits nonzero
  when a check fails. A condition memdolt clears by itself (a stale lock
  record, an orphaned pidfile) is a warning and exits zero.
- Build or inspect the derived embedding side-store (PRD §8.2): `go run
  ./cmd/memdolt index rebuild` synchronizes `.memdolt/embeddings.sqlite`
  with every committed fact, decision, task, and document chunk, while
  `go run ./cmd/memdolt index status` reports current, missing,
  content-hash-mismatched, wrong-byte-length, and orphaned rows. `status`
  is read-only and does not create a missing side-store; `rebuild` writes
  SQLite only and never changes a Dolt source row or commit. Both accept
  `--dir` and `--json` like the other CLI surfaces.
- Recall durable memory (PRD §8): `go run ./cmd/memdolt recall <query>` uses
  the configured FTS or hybrid mode over committed facts, decisions, tasks,
  and document chunks. Hybrid candidate ordering is vector-only when vectors
  are current; stale vector rows warn and may enter through the explicit
  lexical fallback. `--source-type`, `--max-results`, `--accepted-only`,
  `--include-stale`, `--no-rerank`, `--min-rerank-score`, and `--provenance`
  narrow or annotate one call; `--json` emits the complete response object.
- Search committed decisions (PRD §8): `go run ./cmd/memdolt search <query>`
  accepts plain text or a memhub-compatible decision prefix and uses Dolt
  FULLTEXT over decision titles and rationales. `search file:<path>` refuses
  with the M5 code-index/git-ingest remedy until that corpus exists.
- Evaluate retrieval (PRD §8.4): `go run ./cmd/memdolt eval retrieval` runs
  the committed golden JSON through production hybrid recall, reports every
  match/empty outcome plus Recall@3 and safety failures, and exits nonzero
  below the recorded 100% baseline. `--mode fts`, `--golden`, `--dir`, and
  `--json` provide the corresponding explicit overrides and output forms.
- Write and read the direct lanes (PRD §3.1): `memdolt task add|done|block|list`,
  `memdolt note add|list`, `memdolt command record|get`, `memdolt state set|show`
  and `memdolt arch set|show`. Each write is one Dolt commit on `main`, authored
  by the actor (`--actor "Claude Code"` normalizes to `agent:claude-code`; the
  default is `user`), so `dolt_log` answers provenance on its own. `note add`
  and the two `set` commands read their body from stdin when given no argument.
- Review what agents staged (PRD §7, §11.2): `memdolt review list|show|accept|reject|expire|stale`.
  `show` renders a proposal as the single-commit diff of its branch; `accept` merges
  exactly the one commit that proposal was staged with into `main` under a `--no-ff`
  merge commit — authored by the reviewer and messaged `review accept <kind> <id>`,
  so `dolt_log` alone carries the whole propose-review-merge cycle — and then deletes
  the branch. `reject` deletes the branch and leaves `main` where it was, `expire`
  sweeps branches older than `--older-than`, and `stale` reports them without writing
  anything. The merge is fail-closed (PRD §6.3): a data conflict, a constraint
  violation that verification shows is real, or a row memdolt cannot attribute leaves
  `main` untouched and the proposal still pending.

  **Before M3, acceptance had no contradiction probe.** It moved from the
  one-commit and accept-time deny-list guards directly into the merge protocol.
  After issue #102, ordinary repository fact and decision accepts compare their
  incoming prose with every current same-kind durable row using the shipped
  cross-encoder; a score at or above 2.0 refuses before merge. An already-staged
  supersede proposal or explicit operator `review accept --force` is the only
  bypass, and both still land through the same reviewer-authored merge. A model
  configuration, model-open, inference, non-finite-score, or model-close failure
  refuses before durable state changes. **This restriction binds repository
  promotion through `internal/review.Accept`/`localdolt.AcceptProposal` alone,
  not every review verb or every `Store` write**: list, show, reject, expire and
  stale do not promote and run no inference. Force skips only this probe; the
  target, one-commit, deny-list, conflict, constraint, attribution and branch
  deletion rules above still hold.

  Before issue #106, `AcceptProposal` calls on one repository-owning Store were
  serialized so the second accept probed the first one's durable result; that
  lock bound acceptance alone. After issue #106, `proposalMu` retains that
  ordering and also covers memdolt proposal staging, reject, and expiry. It does
  not cover reads, direct-lane commits, every Store, or foreign Dolt sessions;
  the issue #106 blast-radius record above states the external-mutation and
  deletion behavior.

  A branch does not gain the supersede bypass from its metadata label alone:
  accept verifies the exact staged shape (one old live fact linked, one live
  same-key replacement, no other old-row or decision change). Before this
  shape check, accept trusted `proposals.kind = 'supersede'`; after it, a
  hand-crafted mislabeled commit is refused. This check binds
  `KindSupersede` acceptance alone, not every two-row proposal.

  **That sentence used to read "merges exactly that branch".** It merged the branch
  by name and trusted it to hold one commit, which is true of every branch `stage()`
  produces and of nothing else: a branch carrying two commits was shown and
  deny-list-scanned as its head commit alone and merged whole. `accept` now counts
  what the branch carries past its merge base with `main` and refuses unless the
  count is one, then merges the staging commit *by hash* — so the commit review
  showed is the commit that merges. **The count binds `AcceptProposal` alone, not
  every review verb**: `show`, `reject`, `expire` and `stale` still take a proposal
  branch whatever is on it, and `show` renders its head commit against that commit's
  first parent — a partial view of a branch carrying more, which is how an operator
  sees what a branch holds before rejecting it. Refusing at the one verb that writes
  to `main` is the point, not an oversight at the others.
- Run the M0 rig-1 concurrency soak (PRD §16): `go test -tags
  soak,gms_pure_go ./tests/soak/...`

The soak lives behind the `soak` build tag, so `go test ./...` never starts
it — it runs real processes and kills one of them. Note that the mandatory
tag has to be repeated on that command line: a `-tags` there *replaces* the
one in `GOFLAGS` rather than adding to it, so `go test -tags soak
./tests/soak/...` silently drops `gms_pure_go` and fails inside
go-icu-regex. Duration and concurrency are flags (`-soak.duration`,
`-soak.owner-writers`, `-soak.client-processes`, …), and a long run needs
`-timeout` raised past the 10-minute default. Findings and measured
numbers: `docs/spikes/m0-rig1.md`.

## The deny-list (PRD §11.3)

`.memdolt/config.toml` is per-machine and optional. **Before recall,
`[deny_list]` was the only table with a reader. After recall, `[retrieval]`
also has a reader for retrieval behavior; deny-list enforcement still reads
only `[deny_list]` and remains independent.** A deny-list is configured as:

```toml
[deny_list]
patterns = ['(?i)\bAKIA[0-9A-Z]{16}\b']
```

Patterns are Go regular expressions, written as TOML *literal* strings
(single quotes) because a regex's backslashes are not valid escapes in a
TOML basic string. They are matched against `store.CommitRequest.Text` —
the memory a write records in its own words — before the write's
transaction opens, so a refused write leaves no row and no commit. Every
failure refuses: an unreadable file, TOML that does not parse and a pattern
that does not compile all refuse the write rather than let it through
unscanned (PRD §13.3 — memdolt keeps secrets out rather than promising to
delete them).

**Every `CommitRequest` declares its text, or declares it has none.** One
that sets neither `Text` nor `NoText` is refused before anything is
applied, so a lane that writes through a `CommitRequest` cannot skip the
deny-list by leaving a field at its zero value — it fails loudly on its
first write instead. `NoText` is for commits that carry no prose anyone
wrote; the migration runner is the case it exists for, deliberately, so
that a deny-list config that cannot be evaluated never stands between an
operator and `memdolt init`. The declaration travels over IPC too, where
`storeipc.CommitRequest` carries both fields to the owner that enforces
them.

**That tripwire binds `CommitRequest`, not every write to `main`.** It was
written when the two were the same thing: before the review lane, every
write went through `localdolt.commitConn`, so "a new write lane fails
loudly rather than skipping the deny-list" was true of write lanes in
general. After it, that sentence is true of `CommitRequest` alone, and
`review accept` is the counterexample that proves the difference — it
promotes a proposal by merging its branch, so the rows are already staged
and it concludes with a `DOLT_COMMIT` and no `CommitRequest`. Nothing
failed loudly; it simply wrote. Its coverage is a second, hand-maintained
scan in `internal/store/localdolt/review.go`, over prose read from the
proposal branch's own diff, listing the columns in `scannedColumns` —
which has to be kept in step with `Fact.text`/`Decision.text` and the
proposal rationale/actor declarations in `propose.go` by hand, since
nothing checks it. That diff is the head commit's, so the scan covers the
whole of what merges only because `requireOneCommit` refuses to promote a
branch carrying anything else.

**That scan used to read whole row images off the diff.** Before,
`proposalText` appended every scanned-column value present on a diff row's
To side, whatever the diff said had happened to it; after, an added row is
still scanned whole (it has no From side to compare against), a modified row
contributes only the columns whose value moved, and a removed row has no To
side and never contributed. What stopped being scanned is unchanged durable
text: a supersede's UPDATE touches `superseded_by` alone, so its diff
presented the superseded row's key and value — prose an earlier review had
already made durable — and a rule written after that refused an unrelated
supersede of the very row it matched. Every durable string reached main
through one of these scans when it was first written; what the narrowing
gives up is re-matching history, not scanning new prose. That diff is still
the head commit's, so the narrowed scan still covers everything that merges,
because `requireOneCommit` refuses to promote a branch carrying anything else.

**That sentence used to name only `Fact.text`/`Decision.text`.** Before,
actor values were the exception it failed to name: the note and narrative
lanes persisted `Actor.Raw` in `actor_raw` without adding it to their
`CommitRequest.Text`, while proposal staging persisted `Actor.Name` in
`proposals.actor` and the proposed row's `source` without declaring it,
and review accept omitted `proposals.actor` from `scannedColumns`. After,
those two direct-lane callers explicitly add `Actor.Raw`, proposal staging
explicitly adds `Actor.Name`, and accept explicitly reads
`proposals.actor`. These obligations bind those named seams alone, not all
columns of their kind: `CommitRequest` scans only the text each caller
declares, and review accept scans only `scannedColumns`; a future persisted
string elsewhere still needs its own declaration.
**A future lane that writes to `main` without a `CommitRequest` owes the
same, and will get no warning if it forgets.**

## Conventions for agents (PRD §14)

- The PRD is authority; don't silently diverge from it.
- Agents are untrusted writers — the review gate is non-negotiable.
  Propose; humans promote.
- Fail loudly.
- No scope creep beyond the §12 parity matrix; §1.2 non-goals are enforced
  in review.
- Feature branch + PR always.
- Flag new dependencies before adding them.

<!-- orchestrator:managed:start version=1 -->
This file is partially managed by Orch (see `.orchestrator/config.toml`).
- In **Assist** mode, tracked-file changes are mechanically denied; a mutating
  request triggers read-only planning instead.
- In **Delivery** mode, work happens in an isolated per-issue worktree, never in
  this checkout directly.
- Model/effort routing, concurrency, and host plugin setup live in
  `.orchestrator/config.toml` — edit that file, not this block.
- Orch upgrades this block through Delivery. Do not hand-edit it; a hand edit
  blocks the next install/upgrade until reverted or removed.
<!-- orchestrator:managed:end -->
