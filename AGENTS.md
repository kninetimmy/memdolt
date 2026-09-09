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

**Repository identity and topology (issue #163).** Before this slice, native
transfers shipped but `[repo]` was ignored and `meta.project_id` was absent.
After it, explicit init records a memhub-compatible Git-origin identity, with
full canonical origin evidence for collision refusal. Existing unidentified
stores require `init --adopt-identity`; repeated init and reopen do not add
identity commits. Clean-main/schema/deny checks, honest attribution, native
late-result reporting and pending-ref preservation apply. Global identity
remains separate. The complete changed-element inventory, exact symbol scopes,
precedence and recovery are in [the topology guide](docs/repository-topology.md).

`repo configure` updates supplied machine-local keys with the owner stopped,
using the existing protected rooted config replacement. `local` keeps ordinary
status offline; explicit transfers and named remote status remain. `clone`
uses native transfers; `live` refuses instead of falling back. An optional TOML
default must match native origin exactly; other explicit names remain supported.
Malformed repository TOML and unknown consumed keys now refuse reached Store
operations, including direct authenticated owner requests. Before this delivery
NoText could avoid reading any config; afterward it still skips deny-regex
evaluation but not repository routing-file validation. Other valid tables keep
their independent readers. `Close` still releases ownership after refusal.

Startup pull is off by default, clone-only when enabled, and runs existing Pull
once before owner publication. Errors/conflicts stop startup; real confirmed
hashes survive later failures and unknown outcomes never trigger replay. The
shared handle rechecks policy/identity per Store operation; captured native
transfer identities are checked separately. This does not constrain arbitrary
native SQL or turn caller-written Commit SQL into an identity immutability API.
Existing credentials, ownership, review/queue/provenance behavior, all 22 MCP
registrations, frozen golden gates and native history remain. Synthetic
direct/owner/MCP checks use disposable stores; no live hub, installation,
dependency, schema migration, physical M4 acceptance or M5/M6 completion is added.

Before #163's first review correction, TOML struct decoding could let case
aliases override routing, branch-qualified identity reads included dirty rows,
and the origin parser admitted Windows drive-relative paths and Unicode folds.
After correction, repository maps enforce exact table/keys and value types;
Open/InitializeIdentity capture committed main and readProjectIdentity requires
immutable hashes for all callers; origin parsing rejects non-ASCII before
folding and leading drive-letter/colon forms everywhere. Dirty rows and
unrelated TOML survive.
These restrictions bind the shared repository configuration/identity helpers and
their callers, not every TOML reader or arbitrary native SQL. The topology
guide retains the complete branch inventory and focused before/after evidence.

**Terminal global proposal acceptance (issue #161).** Before this delivery,
the #146 global-target refusal and terminal remedy below still excluded actual
acceptance. After it, `memdolt review accept <id> --dir <repository>` accepts a
global fact/decision proposal through direct or authenticated owner terminal
review, preserving repository main. [The global guide](docs/global-memory.md#terminal-proposal-acceptance-issue-161)
and PRD §10 specify supported shapes, ownership, outcomes and manual recovery.
Full changed-element inventory and preserved boundaries:

- New `localdolt/global_review.go` owns source capture, exact source/destination
  validation, native prior-acceptance verification, destination staging and
  reviewer merge, source/head checks and progress/error reporting. It accepts
  ordinary new facts/active decisions, exact live-fact overwrites and same-key
  supersedes. Metadata, ids, NULLs and source timestamps/provenance survive;
  new native commit dates are real. Destination ids/keys, changed before-images,
  malformed/extra changes or commits, invalid stores and uncertain history
  refuse. Copied native author names/emails also enter the deny scan. Repository
  main and unrelated proposals stay unchanged. Source-bound
  staging and two-parent review history establish idempotency, never row equality
  alone. Both refs remain; source pending counts remain honest about repository
  ancestry. No automatic source deletion is attempted because native Dolt has no
  expected-head delete. Manual reject/expiry retain their existing best-effort
  rules. This policy binds terminal global review, not all native SQL/Store writes.
- `localdolt/review.go` adds `AcceptTerminalProposal`, a private terminal opt-in,
  and optional global progress on `AcceptResult`. Before #161 `AcceptProposal`
  refused every global target; now only that explicit terminal entry diverts to
  the new destination. Ordinary/expected-commit acceptance still refuses global
  and retains repository count, deny, supersede, merge and cleanup behavior.
  `checkContradictionClaims` extracts the shared scorer loop without changing
  durable candidate queries, threshold, model-close checks or ordinary input
  shapes. `merge` reuses `nativeCommitResult` only for global results; repository
  result handling remains as before. The outer source-connection close is
  reported for global acceptance only. Existing review verbs and list counts
  are unchanged.
- `localdolt/interop.go` extracts `captureInteropProposal` from the existing
  complete fixed-schema diff reader. `interop_format.go` extracts target-neutral
  `validateProposalPayload`; `validateInteropProposal` still enforces repo-only
  import/export. Column/NULL/Unicode/provenance/relationship checks, immutable
  materialized diff reads, bundle formats, interop target exclusions and import
  behavior remain. Global callers add their own target/same-key/destination
  constraints; these do not loosen interop policy.
- `localdolt/propose.go` lets only private `globalReview` staging reuse validated
  imported identities/statements while checking the destination after branch
  cut. Confirmed and unknown global staging residue survives restoration/close
  failures and is not automatically removed. Ordinary staging and imported
  proposal validation, author override, collision and cleanup rules remain.
  `OpenGlobal`, its path/owner guards and `requireTransferClean` are reused,
  not rewritten. Global acceptance's new lock order is source proposalMu,
  nonwaiting global file lock, destination proposalMu; recall contention refuses
  rather than forming a cycle. No new native/foreign-writer coordination exists.
- `internal/review/accept.go` routes only the empty-expected-commit terminal
  application call into `AcceptTerminalProposal`; the existing owner callback
  uses that same path. Nonempty expected commits still call the ordinary gate.
  Production config and checksum-verified scorer setup remain. No model/config
  override crosses IPC. `storeipc/operation.go` retains captured global results
  even before a confirmed merge, using its existing result/error envelope;
  `owner_store.go` preserves native unknown markers and reports unobserved
  terminal replies without fallback/replay. Expected-commit repository replies,
  authenticated ownership, allow-list and every other operation stay unchanged.
- `cmd/memdolt/review.go` collects the result until store close, then emits
  compatible success fields plus optional global progress/error information.
  Confirmed hashes survive close/output failures; unknown responses never imply
  rollback. The private runner constructor supports those CLI regression seams.
  Help explains both destinations and retained source refs. CLI flags, actor
  normalization, repo-only success wording and other review commands remain.
- New localdolt/CLI/storeipc `global_review_test.go` files exercise isolated
  native payload/history, explicit destination before-images, malformed/collision
  refusal, actual unknown native results, confirmed cancellation/finalization,
  concurrency and authenticated loss/late results. CLI helper processes reopen
  both direct and live-owner paths. `cmd/memdolt/global_model_test.go` adds
  real checksum-pinned model acceptance and resulting global recall/provenance.
  Existing fixtures and frozen golden assertions are unchanged; tests touch no
  live user stores, credentials, host installations or hub.
- README, this record, PRD §10, `docs/global-memory.md`, server instructions,
  the Claude/Codex/OpenCode global templates and shared onboarding retain matching
  before/after records and the terminal human gate. MCP still excludes global
  review; all 22 registrations, input schemas, session queues, recall scoring,
  global human/docs/promote operations, host registrations and installation
  behavior remain. No dependency, schema/migration or broader M4/M5/M6/physical
  two-client hub acceptance is introduced.

**Shared global memory (issue #146).** Before this delivery, PRD §10's global
replica, human operations and combined recall were design-only; earlier #132,
#138–140 records below accurately excluded them. After it, the CLI global
operations and merged CLI/MCP recall ship. The complete commands, exact layout,
metadata and recovery contract are in [the global memory guide](docs/global-memory.md).
At #146, global-target proposal acceptance still refused with its existing
terminal remedy; #161 above replaces that terminal refusal. No complete parity
or physical two-machine hub acceptance is claimed.
The complete structural blast radius is:

- New `localdolt/global.go` and `global_path_windows.go`/`global_path_other.go`
  resolve `~/.memdolt/global/.memdolt/dolt/memory`, reuse the existing Paths and
  store schema, check local/contained managed paths and links/reparse points,
  own the two per-repository `[global]` flags and open only an existing current
  replica. No machine registry/config or live user store is touched by tests.
  The ordinary global lock refuses competing CLI/MCP owners before opening an
  engine. This policy binds `OpenGlobal` and its callers, not arbitrary native
  Dolt sessions or raw `New` calls with the same directory. Existing repository
  layout, native bootstrap, migrations and lock implementation remain unchanged.
- `localdolt/localdolt.go` adds the calling repository policy pointer; global
  writes recheck enablement and use that repository's deny-list. Ordinary
  stores keep their prior policy, commit ordering and result semantics. New
  `global_promotion.go` captures committed fixed-schema rows under proposalMu
  using existing immutable interop reads, then copies one live fact/decision
  with a fresh ULID and exact nullable fields. Source id/commit enter the result
  and real user-authored commit; no host-root metadata is added. Existing live
  fact keys refuse promotion; human FactAdd keeps its guarded upsert. Decision
  duplicates are retained and their colliding ids reported. SQL computes title
  equality with its normal collation in a projection: the pinned optimizer
  misuses FULLTEXT for WHERE equality even with IGNORE INDEX. No new index is
  needed at memory scale. Superseded-row promotion refuses dangling cross-scope
  links and names the live replacement. These rules bind PromoteGlobal, not
  arbitrary Commit calls, imports or reviewed lanes. Existing proposal/review
  behavior, including terminal global acceptance refusal, remains unchanged.
- `localdolt/human_memory.go` adds optional source-capture/collision fields to
  HumanMemoryResult and global-only decision collision reporting. Its six human
  methods retain attribution, validation, mutex, clean/merge/schema/deny checks,
  upsert/supersession rules and confirmed result-plus-error behavior.
- `document_file.go` reuses the rooted config replacement for global enablement
  and default-doc flags. Opened configuration identity, regular/reparse and
  owner-alias checks precede its content read. The raw reader is shared but
  ReadGlobalConfig decodes only [global], preserving disabled recall's prior
  independence from unrelated document settings. Other TOML tables survive as
  before. Global document sources use calling-repo policy and protect both
  known owner credential identities; local sources retain their existing check.
  `documents.go` retains hashes, ids, scoped reads/removal, replacement chunks
  and commit behavior. Global writes require user; every successful global add
  flips only its calling repository's default docs, even when shared content is
  unchanged. Repository first-document/opt-out behavior remains. These policies
  bind these document/config helpers, not every file reader. Existing final
  check/open and config-replacement intervals are not filesystem compare-and-swap.
- New `localdolt/recall_snapshot.go` captures sources, lexical hits, semantic
  text and optional blame while serializing cooperating writes, and refuses a
  changed foreign main. It claims one commit per scope, not an atomic pair of
  databases. `store/store.go` adds only its typed capture records. Existing
  Store operations and schema stay intact. `localdolt/retrieval.go` and
  `embedding.go` exclude tasks only for OpenGlobal stores; ordinary committed
  source/FTS/provenance readers and semantic-text shapes remain unchanged.
- New `retrieval/global.go` scopes identities internally before existing scoring,
  reads each local vector store with CurrentVectors, retains both equal-id/key
  hits, and restores the original ids with scope/snapshotCommit metadata.
  `retrieval/recall.go` reuses one query embedding, candidate map, score/floor/
  age/stale/superseded/accepted filter and rerank pass. Per-scope default-doc
  inclusion precedes candidate selection; default docs still require rerank.
  Global stale-vector warnings name the proper global rebuild. Ordinary Recall
  and Run keep their output and algorithm apart from optional omitted metadata.
  `scopedrecall/recall.go` is the new shared CLI/MCP policy/open/close seam:
  disabled goes through prior Run; enabled holds the global lock through capture,
  local vector reads and inference. Missing state/lock/close errors stay visible.
  Only facts/decisions/docs enter global recall; tasks, notes, narratives, code
  and archives never become global corpora. Inference/hash rules and both golden
  corpora/thresholds remain unchanged; vectors never enter Dolt or sync.
- `storeipc/operation.go` adds exactly two read-only allow-list operations and
  Backend methods, capture_promotion/capture_recall. New `storeipc/global.go`
  checks UTF-8 before encoding and submits each capture once. Existing owner
  authentication, no-fallback/no-replay, loss/late error handling and all write
  operations remain; no global promotion write or accepting MCP path is added.
- New CLI `global.go` owns enable/disable/status, wraps existing init/clone at
  the checked global location, and supplies explicit global flags/path/open
  selection. `root.go` adds that family. `lanes.go` keeps its existing route
  unless a command explicitly enables the new flag. `human_memory.go` adds
  promotion and scoped human flags/result notices; `doc.go` adds scoped flags,
  remove alias and config notice, preserving a confirmed config-only flip after
  later close/output failures too. `index.go`, `remote.go`, `repo.go` and
  `transfer.go` reuse existing operations with global selection; their ordinary
  routing, authentication, result/close contracts and conflict behavior remain.
  `recall.go` and MCP `tools.go` call the shared seam; CLI adds scope labels.
  All twenty-two MCP registrations, inputs, note/proposal/review lifecycle and
  discovery/cache behavior remain. The count still binds RegisterTools alone.
- New global tests in CLI/localdolt/retrieval/storeipc/MCP cover real isolated
  replicas and local remote history after reopening, three-repo config/docs,
  human/promoted fields/nulls, collisions, equal ids, fallback/filters, disabled
  equivalence, missing/corrupt/newer stores, locks, credentials and late/lost
  captures. The golden-tagged CLI global model test copies model artifacts to
  an isolated home and verifies real rebuild/recall. Existing doc/host-template
  tests update only now-shipped flag/template expectations. No test changes
  production source protection, frozen golden assertions or host acceptance.
- Three new global skill templates for Claude/Codex/OpenCode plus one
  `opencode.json` command expose real workflows and the preserved human gate.
  Existing core/locator templates and host registrations remain. Server
  instructions, this record, PRD §10 and the new guide retain matching
  before/after scope; no dependency or durable migration is introduced.

Before integration with #145, the #146 branch still inherited the older native
document result handling, which could discard an observed commit on a late
error. After the normal merge of landed #145/#147, global DocAdd/DocRemove reuse
the same docAddFinalize/docRemove and nativeCommitResult seams: observed hashes
and document/chunk identities survive native finalization or cancellation;
unobserved results retain ErrCommitUnknown and never imply rollback or replay.
Global add reports an unfinalized `[global]` default-doc setting even when other
repositories already populated the document table. Its manual remedy names the
calling repository; a failed native operation never attempts config finalization.
The repository first-document behavior remains. New global_documents_test.go
exercises real native add/remove commits, cancellation before versus after hash
observation, reopened rows/history and independent three-repository config-only
progress. Existing CLI config-only close/output evidence and the global corpus
tests remain. The merge preserves #145's NoteCommits/session-render/unknown-group
behavior, raw/typed owner results, all wrap-up templates and #147's hub code,
runbook and CI lane. Root registers both global and hub; no hub/runtime/credential
policy, dependency, schema or frozen golden assertion changes in this integration.

Before #146's first review correction, global enable/disable wrote the config
before resolving global paths and could discard Changed on a later read/close
or output error. After it, the shared CLI command checks config and global paths
first and preserves confirmed changes through its deferred report, including the
known enabled value and a config-inspection remedy in JSON/human errors. A
preflight refusal leaves config untouched. The private setter constructor seam
only lets the CLI regression inject failures after a real config replacement;
SetGlobalEnabled and the rooted writer retain their existing contracts. The
regressions cover both commands, human/JSON output, invalid/linked global homes,
late setter close, follow-up read and output failures. This reporting boundary
binds these CLI toggles, not every config writer; global data, docs, recall,
native late results, schemas and MCP registrations remain unchanged.

**Confirmed outcomes and session rendering (issue #145).** Before this delivery,
#139 preserved native finalization hashes but the older direct/document/raw-owner
consumers could discard them, and MCP could retry a committed note batch. After
it, confirmed row identities and hashes survive late failures through the reached
application, CLI and authenticated owner paths. Full structural blast radius:

- `internal/store/store.go` makes the result-plus-error obligation explicit for
  every `Store.Commit` consumer and adds `ErrCommitUnknown`. A nonempty hash
  confirms durability; an unobserved native/owner result does not prove rollback.
  `localdolt/localdolt.go` retains hashes assigned before result-close failure,
  preserves populated results through outer rollback/finalization, and reports
  commit-connection close errors. `nativeCommitResult` classifies errors after
  attempted DOLT_COMMIT without an observed hash as unknown; earlier validation,
  deny/config, clean-guard and statement failures retain their prior refusal
  behavior. This classification binds `commitTx`'s native result seam, not every
  SQL error or independent merge procedure. SQL values remain bound and the
  existing `proposalMu` serialization is unchanged.
- `memory/memory.go` preserves results through `Lanes.write`, `AddTask`,
  `CompleteTask`/`BlockTask` via `setTaskStatus`, `LogNote` via
  `LogNoteWithProvenance`, `CommitNotes`, `RecordCommand` and `SetNarrative`.
  `ConfirmedWriteError` names confirmed hashes and inspection remedies only when
  a hash exists; it does not classify arbitrary errors. Command read-back failure
  returns only its certain kind plus the commit/error; zero-value totals are not
  authoritative on that failed response. Command upsert/read-back ordering,
  normalization, raw/canonical attribution, note provenance, deny declarations,
  batch RequireClean and committed readers remain unchanged.
- `localdolt/documents.go` retains add/remove identity, chunks and hash before
  testing the commit error. A first-document late commit error leaves default
  recall config unfinalized and names the manual config remedy; do not replay
  ingestion to repair it. Existing `documentFinalError` also retains connection
  errors. The private `docAddFinalize`/`docRemove` finalizer seams default to the
  same native transaction finalizer; exported arguments, pinned reads, file
  protection, chunking, hash no-ops/replacement and configuration policy remain.
- Other native-result consumers are inventoried rather than given new policy:
  `humanMemoryWrite` and interop main/staging already retained confirmed effects;
  ordinary `stage`/`stageOnBranch` still use a result hash for cleanup only with
  no staging error, so late failure cannot broaden branch deletion. Migration
  `applyMigration` still discards only uncommitted residue on failure, preserves
  committed history, and uses inspection/re-init plus `ensureMigrationTag` for
  missing tags. A late migration is not relabeled rolled back. Review/pull's
  independent native merge procedures retain their existing boundaries.
- `storeipc/{storeipc,client,owner_store}.go` carries raw Commit's optional error
  and unknown fields, preserving confirmed hash/RowsAffected with an error.
  Unknown native outcomes retain the marker over an authenticated reply; a lost
  reply is unknown too. Pre-commit owner refusals keep HTTP status errors.
  `operation.go` preserves typed command-record results on late errors and uses
  the optional `Config.Render` callback for owners with a session queue, defaulting
  to `Store.Render` otherwise. Existing document result envelopes, authentication,
  argument decoding, explicit operation allow-list, one-submit/no-fallback and
  no-replay rules remain. No new IPC operation or caller-selected render path.
- `cmd/memdolt/lanes.go` retains direct results until its selected store closes,
  then emits compatible success fields or confirmed fields with an optional JSON
  error and nonzero result. Output failure names the commit and inspection remedy.
  Its lane-specific help changes only task/note/command/narrative writers;
  `opencode.go` retains verified note results through close/output failures while
  preserving verify-before-open and exact provenance. `doc.go` updates help;
  its existing result/close/output reporting remains. `render.go` updates help;
  result and per-file reporting remain. Reads/review/global exclusion are unchanged.
- `mcpserver/tools.go` retains task/command typed results on protocol tool errors;
  a Go handler error still refuses an unconfirmed write. It attempts every actor
  group, removes each confirmed group before the next, and reports its hash even
  if another group fails. Known uncommitted groups retain explicit-render and
  orderly-shutdown retry. Unknown groups retain IDs/errors for inspection only,
  block session render and are never resubmitted; end that session and inspect
  before starting a fresh one. New notes still refuse after any flush error.
  Timer errors reach the next render once and remain in the shutdown error;
  stale timer callbacks cannot consume another timer or replay a flushed group.
  `Close` retains prior/final failures, attempts eligible groups with the existing
  one-minute limit and discards all remaining groups, including unknown groups.
  A crash still loses process memory. These queue rules bind Toolset alone.
- `mcpserver/render.go` adds `Toolset.Render`: under its queue mutex, stop the
  timer, flush with the existing one-minute bound, and call `Store.Render` only
  after a successful flush. A flush failure reports `notes-failed` with no file
  publication. `cmd/memdolt/serve.go` wires the same callback before publishing
  IPC, preserving protocol/pending-work/toolset/IPC/store shutdown ordering.
  Standalone `localdolt.Store.Render` still captures committed main without a
  queue or memory commit; safe per-file publication and proposal exclusion remain.
  Session render's queue mutex is separate from the unchanged store mutex.
- The server instructions, three wrap-up templates and PRD §§3.1/5.2/11/16
  preserve matching before/after records, approval and identity gates. New
  localdolt/MCP/CLI confirmed-result tests plus updated MCP render and production
  serve tests and `storeipc/confirmed_result_test.go` exercise actual native
  finalization/cancellation, refused roots,
  read-back/close/output errors, authenticated results, render ordering and
  reopened rows/history. `tests/soak/roles.go` now records a confirmed hash as
  committed even with an error; its ledger already records both. An explicit
  unknown marker is indeterminate as cancellation already was. Its workload
  and crash-audit policy remain.
  No dependency, durable schema, new CRUD/global operation, polling service,
  separate batch implementation or full M5/M6 completion is introduced.

Before #145's review-cycle correction, a successful `flushLocked` removed its
groups but discarded their IDs/hashes; a later render config/snapshot/file error
could therefore report only file effects despite committed notes. After the
correction, the same loop returns a note-ID-to-hash map alongside its error.
`Toolset.Render` preserves that map as optional `render.Result.NoteCommits`
(`noteCommits` in JSON) through every later failure. It describes only notes
committed by this invocation, not a cumulative session log or SourceCommit.
`render.Result.NoteCommitError` adds note/history inspection evidence only when
that map is populated. Timer/shutdown error reporting reuses the same helper;
their removal, retry, unknown-group and discard rules remain unchanged.
`cmd/memdolt/render.go` reports the map in human/JSON output and retains its
evidence on close/output failure. Existing MCP and authenticated operation
envelopes carry it unchanged; `OwnerStore.Render`'s lost-reply remedy now also
names note/history inspection without claiming unobserved effects. The pure
renderer still writes no memory and retains all snapshot/file guarantees.
New `cmd/memdolt/render_notes_test.go` reproduces production MCP and live-owner
CLI refusal after successful flush, verifies close/output errors and reopened
rows/history, and proves repeated render adds no note effects. Existing MCP
render tests now cover snapshot and post-render errors with actual committed
notes; CLI/owner render tests retain standalone and lost-reply assertions.
Help, server instructions, wrap-up templates and PRD record the same boundary.

**Private native Linux hub (issue #147).** Before this delivery, `hub
init|status` and managed startup were planned; PRD §13.1's SQL-bind-only
example did not constrain Dolt's independently wildcard remotesapi listener.
After it, the explicit hub CLI generates six reviewable nonsecret artifacts
and inspects a selected deployment. Managed startup requires Linux, systemd,
nftables, both private interface addresses and native Dolt exactly 1.88.1.
The complete before/after, native credential/bootstrap instructions, measured
limits and structural inventory are in [the hub runbook](docs/hub-deployment.md).
This does not install or change the user's hub. Complete structural blast radius:

- New `internal/hub/config.go` owns the independent manifest, literal input
  validation and generated YAML/systemd/nftables/SETUP artifacts. All generated
  interpolations pass `Config.Validate`; this restriction binds these artifacts,
  not general shell/SQL/unit code. Repository configuration/topology stays.
- New `files.go` and `path_windows.go`/`path_other.go` own rooted hub reads and
  create-only output. Exact existing bundles are unchanged; foreign, modified,
  partial, linked/reparse/hard-linked and protected destinations refuse without
  replacement. A failed new write reports complete files and may leave a partial
  directory for inspection. There is no overwrite or automatic cleanup mode.
  Privileged deployment additionally requires root-owned non-writable ancestry
  and exact bundle bytes. Foreign filesystem writers and the final check/open
  interval are not coordinated. Existing renderer/code-index/owner checks stay.
- New `boundary.go` verifies only the actual generated `inet memdolt_hub` table,
  hook and four ordered rules from numeric nftables JSON. IPv4 and IPv6 ingress
  to both TCP ports is restricted to loopback or the selected private interface,
  allowed source prefix and destination. Unknown/extra/dormant/missing semantics
  fail closed. Other tables/traffic remain; no ruleset/table flush occurs.
  New `check.go` owns status, read-only preflight, native release and bounded
  address checks. Executable release is distinct from remotes wire metadata.
  Probe errors withhold native output; no password is accepted or diagnosed.
  The native version probe uses an owned temporary home/cwd with native metrics
  and update checks disabled, preserving strict full-line version parsing and
  the operator's configuration. Native 1.88.1 still constructs its event emitter
  before selecting NullEmitter; the probe owns its exact eventsData/dolt.lock
  artifacts too. Known temporary files are removed; unexpected
  residue and cleanup failures are reported. Nft probes use no temporary home.
- The generated root boundary oneshot validates files, applies only its newly
  created table with CAP_NET_ADMIN and checks the applied result. The server
  binds to it and the chosen private-network service. Every start runs full
  privileged read-only preflight, then unprivileged version/credential-file/
  bounded address readiness, then one dedicated-user native Dolt process with
  no capabilities. `ready` also verifies the effective UID/GID match the configured
  nonroot account/group; root aliases refuse. This check binds `ready`, not status
  or privileged preflight. Generated units/probes disable native event flushing.
  The files-only pre-application check never asserts enforced protection.
  Protection remains on stop. The guard is a startup snapshot, not a monitor
  against later privileged firewall changes; direct Dolt invocations do not
  inherit it. Distribution nftables.service with flush-ruleset config is not used.
- New `cmd/memdolt/hub.go` provides `init`, `status`, `preflight` and `ready`
  with explicit paths/options and human/JSON results. `root.go` adds that family;
  all prior CLI/store/clone/transfer/merge/authentication/code-index behavior and
  22 MCP registrations remain. Status checks local TCP reachability, not process
  identity, credential correctness or physical off-network denial. Non-Linux
  startup/live inspection explicitly fails with unknown unobserved checks.
- New hub/CLI tests validate native YAML using the existing pinned parser and
  cover hostile inputs, preservation, links, nft policy weakening, versions,
  bounded readiness and honest platform reports. `tests/hub/verify_linux.py`
  and its Dockerfile add isolated real-Linux enforcement/native-startup evidence
  with a published-checksum-verified Dolt 1.88.1, disposable container and three
  namespaces. No runner-host firewall/account/service is changed. Unit grammar
  and actual ordered commands are tested; installation under PID 1 is not claimed.
  `.github/workflows/ci.yml` adds `Test (hub ingress)` without renaming protected
  contexts or removing ordinary/golden gates. No Go dependency or schema changes.
- Help, the new runbook and PRD §§11/13/16 retain the old SQL-bind-only example
  and correct its perimeter claim. Native users/grants remain the credential
  boundary: remote read needs global CLONE_ADMIN; write needs broad SUPER, not
  database-scoped remote isolation. Topology/project identity, backup/retention
  and physical two-client/off-network acceptance remain separately tracked.

**Memory interoperability (issue #140).** Before this delivery, PRD §15
described import-from-memhub and JSON interop but neither CLI command existed.
After it, `export <bundle.json>`, `import <bundle.json>` and
`import --from-memhub <export.json>` ship with `--dir`/`--json`.
`docs/migration.md` specifies both distinct formats, nullable fields,
confidence/provenance, model limitations, files, partial progress and retry.
The user's own migration remains optional and unperformed. Full blast radius:

- New `internal/store/localdolt/interop_format.go` owns native v1 bundle/row/
  proposal types and strict JSON, version, fixed-field, ULID, null, relationship
  and ordinary-proposal validation. It selects durable memory columns from
  existing `transferTables`, excluding generated live_key, meta identity,
  documents and local data. Every non-NULL cell is a lossless string. Existing
  schema, evidence/alternatives and command-kind keys remain. This grammar
  binds interop, not arbitrary SQL or other Store operations.
  decodeInteropJSON reuses #137's ValidateJSONUnicode before tokenization;
  invalid UTF-8/lone surrogate escapes cannot become replacement characters.
  Existing pull/owner Unicode checks remain. Exact member checks apply to
  format structs, whose case aliases encoding/json otherwise accepts; opaque
  provenance keeps its case-sensitive JSON keys. Native cells are checked
  before export marshaling too. No second Unicode scanner is introduced.
- New `interop_legacy.go` consumes actual memhub v0.2.0 v1 fields plus only
  v0.2.2's nullable notes. Integer IDs become ULIDs only on ID-bearing target
  tables; commands map to kind. Supersession references are mapped, exact
  note/task text and nullable provenance retained, and confidence/host-root
  identity explicitly omitted. Neither schema has a structured task-link
  column. Duplicate command kinds, pending legacy existing-row supersede,
  unsupported payloads and pending global targets refuse before writing.
  Pending facts/decisions remain proposals; old statuses/writes_log become
  annotated counts, never replayed actions or fabricated commit history.
- New `interop.go` owns Store.ExportMemory/ImportMemory, captured main/proposal
  heads and immutable rows/diffs, destination/schema/collation preflight, full
  deny-scan, bound writes, source digest/count note and confirmed progress.
  Both share proposalMu with direct/proposal/review/render/transfer mutations;
  ordinary ordering remains. Foreign Dolt sessions, other reads and migrations
  retain their prior boundaries. Export does not flush MCP notes. Import
  requires current initialized clean empty durable memory and no proposal
  branches, retaining documents/config/derived/render artifacts and history.
  Before memhub migration, the legacy importer offered force-wipe and removed
  target writes_log; after #140 this memdolt surface offers neither. Changed
  main is one actual human import commit; no model load/index rebuild occurs
  inside import. These restrictions bind the two named interop methods.
- `propose.go` extracts private stageLocked while ordinary callers still use
  the locked stage wrapper. Interop alone supplies a prevalidated proposal ID
  and current-importer commit author override. Source actor/time stay metadata.
  Interop rechecks complete before-images on the just-cut proposal branch,
  catching a foreign main change before that cut without overwriting its row.
  It retains/returns a confirmed imported branch on late errors; ordinary
  staging keeps its existing cleanup/residue behavior. Expected-commit review's
  no-delete policy and other CLI cleanup behavior remain unchanged.
- New `internal/render/bundle.go` reuses existing rooted file preparation,
  regular-file/identity/reparse checks, sync, native replacement and owned
  temporary cleanup. The renderer's configuration, marked Markdown, backups,
  pair behavior and render lock remain. Bundle APIs require an existing local
  parent outside protected metadata, check actual opened credential aliases
  through checkDocumentOwnerFile, refuse registered document sources and use
  a separate per-output export lock. Existing output needs a supported native
  header; preparation failure preserves it. No export backup, foreign-writer
  CAS or stronger directory-entry crash durability is promised. Import reads
  one selected file and opens no metadata pointers. These file restrictions
  bind bundle APIs, not every renderer destination or general file operation.
- `storeipc/operation.go` adds Backend methods and explicit export_memory/
  import_memory operations; owner_store.go submits each complete operation
  once and retains populated results/errors. Lost replies report unknown and
  require inspection. Authentication, verified-owner selection, existing
  operations and no-fallback/no-replay rules remain. No import/export MCP
  mutation tool or new render-tool path/query override is added.
  Interop's typed owner methods check paths before JSON marshaling; the
  existing operationArgs Unicode guard also precedes raw argument decoding.
  Other typed owner methods retain their existing validation boundaries.
  After integrating #138, checkDocumentOwnerFile retains its signature and
  delegates to layout.CheckOwnerSource; interop inherits that opened-file
  check unchanged. Locator/tokenizer behavior and all twenty-two MCP tools
  remain as delivered by #138; import/export adds no MCP registrations.
- New `cmd/memdolt/interop.go` and additive root.go registrations provide
  help, existing-store/direct/owner routing and one human/JSON result on
  reached-operation failures too. Previous commands remain. Main hash/created
  proposals are confirmed effects; mappings/counts also describe the planned
  suffix. Close/reporting failure preserves confirmed progress.
- New localdolt/CLI/storeipc interop_test.go, renderer bundle_test.go, CLI
  interop_model_test.go and synthetic localdolt/testdata/memhub-v1.json exercise
  real legacy/native/direct/owner round trips, nulls/text/mapping, history after
  reopening, CLI review, malformed/refused/denied data, opened credentials,
  file failures, pinned export, lost replies and late partial progress. The
  golden-tagged model check rebuilds a synthetic import and uses real CLI
  hybrid recall. Existing golden data/assertions remain. Tests change no
  runtime path policy: their temporary roots are canonicalized, as existing
  renderer fixtures do, so macOS's /var alias is not mistaken for a permitted
  bundle path. Explicit link/refusal tests still exercise the production guard.
  New docs/migration.md and PRD §§12/15/16 preserve the
  matching before/after and optional converge-first/low-stakes/one-week-soak/
  old-state-retention runbook. No dependency, durable migration, global backend,
  full M5 or physical hub acceptance is implied.

Before #153, the #140 round-trip contract above had a cold-process exception:
re-exporting an imported pending decision could fail at column 12 with
`context canceled`; in-process tests shared Dolt's warmed node cache. After
#153, `repoDiffRows` materializes its CAST cells with CONCAT under the caller's
query context, preserving exact NULLs and SQL ordering after the diff iterator
closes. This correction binds that shared export/status helper, not all SQL
readers. Existing immutable snapshots, schema/path/review guards, cancellation,
and import/proposal semantics remain. The [migration guide](docs/migration.md#re-export-after-reopening-issue-153)
records the proven cause, complete touched-element inventory and fresh-process
direct/owner regression; no dependency, migration or frozen golden changes.

Before #159, the #140 tagged-export claim above and PRD §15's supported source
schemas 1-24 had a numeric-only header exception: `decodeMemhubExport` parsed
only numeric strings, and the synthetic fixture's `"24"` masked the actual
tagged exporter's migration identifier. After #159, `supportedMemhubSchema`
retains numeric 1-24 (including prior leading-zero/optional-plus spellings) and
admits only the 24 exact tagged names, through `0023_session_transcripts` and
`0024_session_note_provenance`. The original declaration stays in the genesis
annotation; unknown/malformed/newer identifiers still refuse before memory/ref
changes. This check binds the legacy decoder and `Store.ImportMemory` paths,
including the authenticated owner, not native headers or all Store writes.
The [migration guide](docs/migration.md#tagged-source-schema-headers-issue-159)
records every touched symbol/file, retained behavior, tagged provenance and
fresh-process synthetic direct/owner import/reopen/export evidence. Its existing
command/supersession/global refusals remain. An unchanged real export now clears
the header check through both routes but still refuses duplicate command kinds
without source/destination changes; explicit source command reconciliation and
the optional real migration remain outstanding. No review acceptance, force-wipe,
dependency, migration or frozen retrieval golden change is introduced.

**Local code-index delivery (issue #138).** Before this delivery, `code`,
`locate`, `eval locate` and the real MCP `locate` tool were deferred. After
it, `memdolt code index|status|rm`, `memdolt locate <query>` and `memdolt eval
locate` accept `--dir`/`--json` and operate only on local tracked source plus
`.memdolt/code_index.sqlite`. No Dolt open, migration, owner routing, memory
write, proposal change, export or sync occurs. Existing `index status/rebuild`
still manages committed-memory vectors in `.memdolt/embeddings.sqlite`.
The complete structural blast radius is:

- New `internal/codeindex/grammar.go` pins the seven real grammar role sets;
  `chunker.go` ports top-level items, impl/type methods, nested containers,
  excised member bodies, Go pointer/generic receivers, JS declarators,
  Python decorators/docstrings and each language's module docs. TSX uses its
  actual dialect. Parser, tree and cursor resources are closed; no query
  resource is allocated. Native failures are visible. A successful parse
  without recognized items uses the tagged 50-line/4000-byte fallback, while
  oversized AST symbols remain whole. LF normalization also precedes parsing
  so a Go comment ending between CR/LF cannot retain a stray CR. These rules
  bind `ChunkFile`, not every document or memory chunker.
  Before the review-cycle lifetime fix, those closes did not release the
  pinned binding's separate `ParseOptions` registration: its unmatched
  `pointer.Save(options)` retained a C allocation, callback and request
  context for every parsed file. After the fix, `ChunkFile` passes nil options
  through the existing input-callback API, whose registration and C strings
  the binding releases; no progress/options registration is created.
  Cancellation is checked at entry, native input requests, immediately after
  native return and after the AST walk. A partial tree from canceled input
  is closed before returning the cancellation error. Native work between
  input callbacks is no longer periodically interruptible; cancellation can
  wait for that work. This boundary binds `ChunkFile` and its refresh callers
  (CLI/MCP/eval), not all parsers or fallback helpers. `chunker_test.go` adds
  a request-context lifetime regression and canceled-input refusal check.
  The unchanged 32-call/one-MiB-context reviewer probe reproduced 0/32
  finalizations and 33,558,664 retained bytes before the fix, then 32/32
  finalizations and 5,144 retained bytes after it. No binding/grammar pin,
  chunking/scoring rule, source protection, corpus or golden matcher changes.
- New `config.go` reads `[code_index]`'s independent 0.5/0.5 fusion weights
  and 0.90 top-level tests/benches/examples penalty. Only `[retrieval]` mode
  (default FTS) and pool size are shared as in the tag; memory scoring,
  rerank toggles/floors and stale/superseded behavior remain independent.
  Memhub's same-named deny setting used globs; memdolt retains its existing
  regex contract and scans code path/content through `denylist.Compile`.
  The tagged default secret paths are fixed code exclusions, even with
  `patterns = []`; their opt-out behavior differs from memhub. A custom glob
  `private/**` becomes regex `(^|/)private/.*$`; `*.generated.go` becomes
  `(^|/)[^/]*\.generated\.go$` in a TOML literal string. No second setting,
  glob dependency or byte-compatible-TOML claim is introduced. Existing
  `denylist.Load` and memory-write enforcement are unchanged.
- New `files.go` and `path_windows.go`/`path_other.go` own canonical root
  discovery, argument-based `git ls-files -z`, sorted tracked paths,
  source exclusions, rooted reads and link/reparse checks. Paths/rows cannot
  select absolute, traversing, stream or protected metadata sources. Every
  source read through `openSource`, including no-refresh snippets, checks
  opened identity before content. Unreadable/denied/deleted/binary replacements
  lose stale chunks/vectors; binary is invalid UTF-8 or NUL. Counter meanings,
  the tagged millisecond mtime/size fast path, content hashes and HEAD-only
  reporting are documented in [the code locator guide](docs/code-locator.md).
  Metadata-preserving edits can remain unseen. These restrictions bind code
  indexing/locating, not every filesystem reader.
- New `schema.go`/`index.go` own the separate SQLite schema, refresh,
  read-only status, vector validation/backfill and bounded removal. An
  application ID, regular/single-link file and known schema are required
  before an existing index is disposable. Incompatible derived schema resets
  safely; unknown occupants, unrelated schema and journal residue refuse.
  No recursive deletion is used. DELETE/FULL journaling replaces the tagged
  WAL/NORMAL choice here so status creates no WAL/shared-memory sidecars.
  Chunk/FTS changes commit together before vectors; failed inference preserves
  that complete text index and reports failure. Hash/model/dimension/length,
  finite/nonzero vector and vector-hash checks prevent stale/corrupt vectors
  from counting as current. Missing/invalid vectors backfill on refresh.
  A separate exclusively created `code_index.lock` covers cooperating
  refresh/query/remove operations; contention refuses. Crash residue needs
  inspection with operations stopped, without automatic PID/stale cleanup.
  SQLite uses checked pathnames; foreign filesystem writers are not locked,
  and final check/open or check/remove intervals are not compare-and-swap.
  Existing Dolt-owner, renderer and embedding-side-store locks are unchanged.
- New `locate.go` owns FTS5/vector union, exact tagged score normalization,
  deterministic ties, optional reranking and bounded disk snippets. `Locate`
  returns at most six lines/400 characters per hit, including an ellipsis;
  this bound does not restrict internal `ChunkFile` bodies. Fusion and runtime
  reranking have no floor. CLI-only no-refresh skips Git/refresh and retains
  old ranks/line metadata; all read guards remain and snippets are current
  disk text. New `eval.go` preserves the version-1 golden format, substring
  matchers, post-floor rank semantics and Recall@1/@K. Its optional floor is
  harness-only; default nonsense leakage is reported, not silently filtered.
- New `internal/layout/source.go` extracts the existing opened-file owner
  check as `CheckOwnerSource`. `localdolt/document_file.go`'s existing
  `checkDocumentOwnerFile` delegates to it, retaining every `DocAdd`'s
  unconditional credential-alias refusal. Code source/config/header reads
  now share the check without opening Dolt. Only known repository `server.pid`
  identity is protected, not arbitrary copied secrets or every reader.
  Missing metadata still permits ordinary direct use; verification failures
  still refuse. The owner file is opened only for identity, never content.
- `internal/embedding/tokenizer.go` adds the model's missing 512-token limit.
  Before it, oversized source/memory input could reach ONNX with more than
  512 positions and fail; afterward `encodeSingle` and `encodePair` implement
  fastembed's right-side LongestFirst truncation including two/three BERT
  special tokens. Sugarme's pair-truncation loop decrements both lengths,
  so the fix trims its already-encoded arrays instead. All four callers,
  `Engine.Embed`, `EmbeddingTokenIDs`, `RerankerTokenIDs` and `Engine.Rerank`,
  share that boundary; short-input tokenization, NFD compensation, model
  verification, native lifecycle and recall scoring/config remain intact.
  Stored source/chunks/snippets are not truncated to the model limit.
- New `cmd/memdolt/code.go` wires these local operations, flags/help and
  human/JSON reports through `root.go` and `eval.go`; no existing command's
  store routing or output contract changes. New `mcpserver/locate.go` accepts
  typed query/limit/rerank input, always refreshes, and uses no Dolt backend.
  `tools.go` adds it to the previous nineteen real registrations. The exact
  twenty-tool count binds `RegisterTools` at this delivery, not arbitrary SDK
  servers. Attribution, cache hints, notes, review and shutdown remain.
  After integrating #137, its twenty-one landed tools, including `repo_pull`
  and `repo_push`, remain and `locate` makes twenty-two. That combined count
  still binds `RegisterTools` alone. The approved transfer/elicitation logic,
  provenance restrictions, `JSONUnicodeReader`, original stdin closer and
  legacy negotiation remain unchanged. The shipped `serve` input guard now
  also precedes locator requests; standalone `New` or arbitrary transports
  retain their previously documented boundaries. The existing tools/serve/
  host-template tests preserve both sets of assertions with combined names
  and counts. No locator, inference, corpus, credential or callback-lifetime
  behavior changes in this integration.
  `instructions.md` now names an implemented locator while preserving routing
  and durable-write rules.
- Six new locate/eval-locate templates across Claude/Codex/OpenCode plus two
  `opencode.json` command entries expose those real workflows. The three core
  skills, coexisting registrations, review gates and provenance behavior stay.
  `host_templates_test.go`, `tools_test.go` and `serve_test.go` retain their
  prior assertions with the applicable real-name/count additions. New code,
  CLI/MCP and tokenizer tests cover AST behavior, lifecycle, safety, independent
  config, failure/concurrency preservation and native input limits; platform
  tests exercise actual unreadable-source handling. Tests add no runtime rules.
- `go.mod`/`go.sum` add only official go-tree-sitter
  `v0.24.1-0.20251112183152-c9492002f76e`, C# 0.23.5, Go 0.23.4, Java 0.23.5,
  JavaScript 0.25.0, Python 0.23.6, Rust 0.24.2, TypeScript 0.23.2 and
  required `mattn/go-pointer` 0.0.1. Existing selected versions are unchanged;
  tidy also records upstream binding-test grammar sums, not additional shipped
  grammars. The two existing mandatory build settings remain necessary.
- New `tests/golden/locate_test.go`, unchanged copied golden JSON and frozen
  JSON corpora/license/notice artifacts retain the real benchmark provenance.
  Rust full Recall@3 is 18/18 on corresponding commit `4606934`; the original
  query bytes are identical there and at v0.2.0. Later `8f25568` moved cosine
  helpers but left a stale expected path, so the complete tagged corpus
  honestly measures 17/18 in a separate diagnostic, also preserved. Polyglot
  is the exact tagged six-file fixture and passes 17/17. Both no-floor runs
  leak two probes; separate harness rerank floor 0 rejects both on each corpus
  while losing true matches (15/18 Rust, 11/17 polyglot). The full OIDs,
  hashes and reproduction commands are in the [fixture notice](tests/golden/testdata/locate/NOTICE.md).
  `.github/workflows/ci.yml` adds the exact required `TestLocateGolden`
  command to protected Ubuntu CI beside the unchanged retrieval/scale gate,
  reusing only checksum-verified model cache entries.

This record, the guide and PRD §§8.4/9/11/16 preserve the before/after scope.
Git-ingested history/`search file:`, global memory, public memory commands and
remaining M5 parity stay separately tracked; no full-M5 claim is made.

**Trusted human repository memory (issue #139).** Before this delivery,
facts and decisions had schema, reviewed agent proposals and committed readers,
but their ordinary human CLI commands remained in the unshipped CRUD/parity
work. After it, `fact add <key> <value>`, `fact list`, `fact verify <id-or-key>`,
`fact supersede <old> --by <new>`, `decision add <title> --rationale <text>`,
`decision list`, `decision set-summary <id> <summary>` and
`decision supersede <old> --by <new>` ship for initialized repositories.
Every command supports `--dir`/`--json`. Writers accept the existing `--actor`
spelling but require its normalized identity to be `user`; agents still
propose and humans review. This is the trusted CLI boundary, not proof that
an OS process is a person. Source labels and remote SQL users grant no authority.

Fact add requires a dotted key without empty segments. It inserts a fresh
ULID when no live row exists, or updates that live row's value/source/kind/
evidence/verified_at while retaining id and created_at. Omitted kind/evidence
clear to NULL. Whitespace-only kind/summary becomes NULL; nonblank text keeps
its whitespace. Source defaults to `user` and retains the tagged vocabulary
`user`, `git`, `observed`, `agent:<id>`, `user+agent:<id>` with lowercase agent
identifiers. `--evidence` and decision `--alternatives` write the actual schema
fields; confidence remains removed. Fact verification changes only verified_at.
Fact verify/supersede resolve an unambiguous exact id or key across live and
superseded rows; historical key ambiguity requires explicit ids. Supersession
links existing same-kind rows, preserves both and refuses self-links, cycles,
missing replacements and corrupt replacement chains. Decision supersession
also sets the old status to `superseded`. No deletion or resurrection occurs.

CLI fact lists retain superseded rows, support literal dotted `--prefix` and
`--limit`, and use recall's configured stale horizon. CLI decision lists default
to all statuses, matching memhub v0.2.0's dispatcher; `--status active` filters
them. MCP keeps its prior active default and ten-row default limit. Decision
`--summary`, `--source`, `--alternatives` and `--evidence` remain readable after
supersession. Lists show committed main only. Changes to value or summary make
old vectors fail the existing current-source hash check; `index status/rebuild`
repairs the local derived index. Render captures current committed text and
retained superseded rows. Identical summaries/links and same-second verification
report `unchanged` without an empty commit; DATETIME precision is one second.

The complete structural blast radius is:

- `cmd/memdolt/root.go` adds only the two command families. New
  `cmd/memdolt/human_memory.go` owns their flags, help, validation, shared reads,
  existing-store/direct/verified-owner choice, close-before-output lifecycle
  and confirmed-result reporting. Previous children and their behavior remain.
- New `localdolt/human_memory.go` defines `FactAddOptions`,
  `DecisionAddOptions`, `HumanMemoryResult`, and the six explicit Store mutations.
  `humanMemoryWrite` validates human attribution, shares `proposalMu`, refuses
  dirty main/active merges, reuses `validateTransferSchema` including the exact
  STORED live_key expression, scans new text/source/raw and canonical actor/
  persisted links, and uses one guarded `commitConn` operation. Source/key/text
  widths, UTF-8 and SQL/config/scan failures refuse. Read checks precede writes;
  a changed captured main refuses before commit. Foreign Dolt sessions do not
  share the mutex, so the final external-writer interval is not coordinated.
  These human restrictions bind those six methods through `humanMemoryWrite`,
  not every Store mutation or source column. Existing proposal, contradiction,
  elicitation, transfer and direct-lane rules remain.
- `localdolt/documents.go` renames the former `documentConn` to
  `initializedMainConn(ctx, purpose)` for document and human callers. The four
  document methods keep their existing current-schema, immutable hash,
  committed-snapshot, file/config and mutation behavior; diagnostics name the
  caller's purpose. No migration or ordinary `Open` behavior changes.
- `localdolt/localdolt.go` retains statement validation, deny-list, opt-in clean
  guard and transaction execution. Before #139 its `Commit` comment claimed
  every failure left nothing behind and `commitConn` discarded a hash after
  outer transaction finalization failed. After it, `commitConnFinalize` retains
  the real DOLT_COMMIT result plus an error naming its hash: the pinned Dolt
  procedure has already persisted before `sql.Tx.Commit`. Earlier failures
  still invoke rollback. This result contract binds `commitConn`/`Store.Commit`;
  before #145 older document/lane/raw owner wrappers still discarded structured
  results on error, and `CommitNotes`/`Toolset` retry bookkeeping was deferred.
  After #145 the confirmed-outcome record above completes those consumers and
  queue rules; the original human-only mutation boundary remains.
- `localdolt/propose.go` keeps `ProposeSupersede` fact-only; before #139 its
  comment said no decision lane requested supersession, after it the new human
  lane does. `stage` uses a returned hash for automatic cleanup only without a
  staging error, preserving the prior expected-head refusal and retained
  proposal residue after a late failure. Its private finalization test hook
  is nil in production. Successful staging/review guards remain unchanged.
- New `memory/records.go` moves the former MCP fact/decision records and list
  queries into `ListFacts`/`ListDecisions`. Existing columns, nullable display,
  ordering, literal-prefix escaping and staleness behavior remain.
  `retrieval/recall.go` keeps `FactIsStale` as a wrapper over the same comparison
  now in memory; `sourceAge` uses it too. Caller-side configuration remains
  independent, avoiding a memory-to-retrieval dependency; ranking is unchanged.
  `mcpserver/tools.go` delegates its two readers to them with its existing
  defaults. All nineteen registrations, schemas, attribution and staged writes
  remain; no human mutation or supersede-as-accept tool is added.
  After integrating #137, those nineteen tools remain alongside `repo_pull`
  and `repo_push` (twenty-one total); the human-only mutation boundary remains.
- `storeipc/operation.go` extends `Backend` and its explicit allow-list with
  six typed human operations. New `storeipc/human_memory.go` owns their typed
  arguments and OwnerStore methods, rejects invalid UTF-8 before JSON can
  rewrite it, submits each mutation once and retains confirmed results with
  errors. Probe/authentication failures never fall back, and a lost reply
  means unknown outcome: inspect fact/decision lists before retrying. Existing
  operations, authentication, actor propagation and cancellation remain.
- New human-memory tests in CLI, localdolt, storeipc and MCP exercise direct/
  owner lifecycle, fields/history/provenance, chain and dirty/proposal refusals,
  deny/config errors, late/unknown/output failures, concurrent upserts, real
  transaction finalization, MCP read/agent boundaries and production vector
  invalidation/retrieval/render behavior. They reuse existing fixture helpers;
  no tests change production policy or claim real-host/global acceptance.
- PRD §§3.1/11.2/12/16, server instructions and the three wrap-up templates
  record this human-versus-agent boundary. Their approval, proposal and note
  lifecycle obligations remain. No dependency, durable migration, global flag,
  global promotion, top-level history/status/stats or full M5 acceptance ships;
  existing `repo status` remains distinct and the remaining parity matrix is
  follow-up work.

**Committed render delivery (issue #133).** Before this delivery, render was
deferred: there was no `memdolt render` command or registered `render` tool,
and the core wrap-up templates explicitly omitted that step. After it,
`memdolt render [--dir <repository>] [--json]` and the real typed MCP `render`
tool generate `PROJECT.md` and `PROJECT_LEDGER.md` from one committed `main`.
This does not replace this repository's memhub Session Continuity instructions
above or invoke memhub. The complete structural blast radius is:

- New `internal/render/render.go` owns `Run`, the coherent capture, formatting
  and result. Every category and the real `DOLT_LOG` walk uses the captured
  immutable hash, not a moving `main`/`HEAD` or working set. The pinned driver
  panics preparing `AS OF ?`; `capture` therefore validates the exact
  32-character Dolt hash grammar before building that revision literal.
  Other SQL values remain bound, and the table/column/order list is fixed.
  This literal rule binds `capture` alone, not arbitrary queries. Source/row
  close failures abort before file preparation. The marker and source schema,
  commit and generation time identify each view. Latest state/architecture,
  ten newest notes with raw/canonical actor and optional provenance, all
  decisions, ordered tasks, and facts including stale/superseded rows remain
  visible; bodies, evidence, summaries and alternatives retain their text.
  ULIDs replace memhub integer references. Activity is up to fifty reachable
  commits in thirty days with real author/email/message/date. No render commit,
  fabricated writes_log, transcript archive or token accounting is added.
- New `internal/render/files.go` owns the independent render config reader,
  output checks, preparation, backups, rooted replacement and temporary cleanup.
  `[render].output_dir` defaults to `.memdolt/rendered`; relative paths must
  stay under the canonical repository, and explicitly configured absolute local
  paths are allowed. Network/device paths, symlink/reparse traversal, protected
  `.git`/`.memhub`/`.orchestrator` paths, and `.memdolt` paths outside its
  `rendered` subtree are refused. Configuration supplies the optional
  `project_name` and positive `[retrieval].fact_stale_after_days`; absent values
  use the repository basename and the existing ninety-day default. Invalid
  TOML/render keys and unsafe destinations fail visibly. The existing retrieval
  and deny-list config readers retain their independent behavior.
  New `path_windows.go` checks all reparse attributes; `path_other.go` checks
  symlinks. Directory handles and identity checks confine reached paths.
- `writeFiles` refuses existing unmarked same-name user files, prepares both
  complete files plus any original backups before replacing either, and retains
  complete backups in `.memdolt/backups/rendered`. It uses `os.Root.Rename`
  on sibling files, with the reached platform's guarantees; the pair is not
  transactionally atomic and directory-entry crash durability is not promised.
  On Windows the installed Go implementation reaches `NtSetInformationFile`
  replacement and its native compatibility fallback. Each confirmed replacement
  and complete backup survives in the result even when later work fails.
  Other user files remain untouched. The output's exclusively created
  `.memdolt-render.lock` refuses overlapping cooperating generations, including
  different owners configured for the same directory. It is separate from the
  unchanged store advisory lock and has no automatic stale/PID protocol: after
  a crash, stop all renders and inspect files/backups before removing residue.
  Only this render's verified temporary artifacts are cleaned up. Foreign
  writers are not coordinated; identity/content checks detect changes before
  replacement but cannot provide a filesystem compare-and-swap against a change
  in the final interval. These file restrictions bind this renderer alone.
- New `localdolt/render.go` implements `Store.Render` under the existing
  `proposalMu`, sharing direct/proposal/transfer mutation ordering. Before
  #133 that mutex excluded render because it did not exist; after it the whole
  render operation participates. Other reads, migrations and foreign Dolt
  sessions retain their prior boundaries. Pinned reads still identify one
  snapshot if a nonparticipating writer moves main. No memory, refs, dirty
  rows, note batches or history are changed by `Store.Render` itself. Before
  #145 this also described the MCP/owner entry points; afterward their session
  wrapper flushes notes first, as recorded above.
  Issue #131 subsequently adds repository status to the same mutex; render's
  snapshot/file ordering and preservation behavior remain unchanged.
- `storeipc/operation.go` adds `Backend.Render` and one explicit `render`
  operation; `owner_store.go` submits the entire operation once and preserves
  results with errors. No output path or raw query is accepted for rendering.
  Lost replies say the outcome is unknown and require inspection before retry.
  Authentication, verified-owner selection, cancellation, existing operations
  and their no-replay boundary remain intact.
- `cmd/memdolt/root.go` adds the command while retaining all previous children.
  New `cmd/memdolt/render.go` reuses existing-store preflight and direct/verified
  owner selection, closes before output, and exposes written files/backups and
  errors in human/JSON reports. `RequireExistingTransferStore` now also serves
  render; its existing callers retain their behavior and ordinary `Open` still
  creates missing stores. Render itself never initializes or migrates.
- `mcpserver/tools.go` adds `render` to the existing sixteen registrations;
  new `mcpserver/render.go` uses an empty typed input and a typed result, retaining
  structured file effects even on a visible tool error. `New`'s modern/legacy
  agent attribution, static cache hints, review behavior and note lifecycle
  remain. Before #145 render did not flush queued notes; only the deadline or
  orderly shutdown did. After #145 the session entry points flush first, while
  standalone Store.Render retains this original committed-view rule.
- New `render/files_test.go`, `localdolt/render_test.go`, `cmd/memdolt/render_test.go`,
  `mcpserver/render_test.go`, `storeipc/render_test.go`, and the shared synthetic
  `render/testdata/memory.sql` exercise content, committed/dirty/proposal
  isolation, immutable history, preparation/backup/replacement/finalization
  errors, routing, lost replies, concurrency and file preservation. Existing
  `tools_test.go` and `serve_test.go` retain their checks and include render in
  discovery. `host_templates_test.go` allows that implemented tool and checks
  its workflow boundaries; no test changes production behavior.
- The three core wrap-up templates add render after approved writes, retaining
  their before/after deferral record and approval/provenance gates. Claude/Codex
  before #145 explicitly reported the queued summary absent until its deadline
  or shutdown flush. After #145 explicit session render flushes it first.
  OpenCode's verified CLI summary is already committed. PRD
  §§5.2/11.1/11.2/11.3/11.4/12/16 record the same read/file/owner boundaries.
  Existing `.gitignore` already keeps default output and backups local; custom
  destinations need an operator-managed ignore rule. No dependency, durable
  migration, unrelated backend, full M5 parity, global memory or gated workflow
  is added.

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
    Before #145 "failures" here included committed and unknown groups. After
    #145 confirmed groups are removed and unknown groups are inspection-only;
    only known uncommitted groups retry, as specified in the #145 record above.
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
    `PendingProposals` and `ProposalDiff` remain reads. At issue #106, direct-lane
    commits still moved `main` independently; issue #127 adds them and transfers
    to this mutex as recorded below. A foreign Dolt session does not share the Go
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
- Clone an existing store: `memdolt clone <remote-url> --dir <repository>
  [--user <sql-user>] [--json]` acquires committed `main`, its history and
  provenance into `.memdolt/dolt/memory`, registers `origin`, and reports
  `store`, `mainCommit`, and `schemaVersion` only after validation and close.
  Use an explicit absolute HTTP/HTTPS remotesapi URL including its database
  path, or an absolute `file:///` URL (`file:///C:/path` on Windows). Spaces
  in file URLs must be percent-encoded; encoded percent signs are refused
  because the pinned parser can decode them twice. URL userinfo, queries,
  fragments, unsupported schemes and invalid ports/users are refused before
  transfer, without echoing rejected credentials. `--user` accepts 1–32 ASCII
  letters, digits, dots, underscores and hyphens; only the process environment
  `DOLT_REMOTE_PASSWORD` supplies its password. Omit `--user` for anonymous
  access; file remotes do not accept it. No personal Dolt credential file is
  loaded for clone authentication. Remotesapi belongs on the private network
  described in PRD §13.1.
  Destination paths containing `?` or `%` are refused because the pinned
  embedded path parsers cannot round-trip them reliably.

  Stop any owner first. Clone takes the established ownership lock and refuses
  an existing database or nonempty data directory before remote contact.
  Managed-path symlinks are refused; adjacent configuration, rendered files,
  and derived side stores are preserved. All clone artifacts stay in the
  destination, including a small empty Dolt bootstrap configuration under
  `dolt/.clone-home/`. Clone performs no automatic artifact deletion. A failed
  transfer, invalid remote, missing `main`, validation failure or close failure
  returns nonzero with a remedy and retained destination. Inspect retained
  artifacts and choose a fresh `--dir` to retry. An older initialized schema
  is retained for explicit `memdolt init --dir <repository>`; a newer schema
  requires a newer memdolt binary. Clone never creates a genesis commit,
  migrates memory, rewrites remote history or retries the clone operation.
  Dolt may retry individual remote reads/downloads internally. Progress is
  suppressed in both output modes; JSON stdout is one success object or no
  success object on failure.

  Before the issue #125 cycle-1 fix, the containment claim above missed the
  source opener: `GetRemoteDBWithoutCaching` reached Dolt's
  `FileFactory.CreateDbNoCache`, which created a missing `oldgen/` in a file
  source even when the empty source was then refused. After the fix,
  `openCloneRemote` bypasses that writable factory for file URLs, resolves the
  source path, opens existing NBS files with `NewLocalStore`, and combines an
  existing `oldgen` and ghost reader only when `oldgen` is present. It never
  initializes a source directory. `CloneRemote` uses only reads on these
  source handles and close releases their readers. This restriction binds
  `openCloneRemote` and the clone flow that calls it; `NewLocalStore` itself
  is still write-capable, and other Dolt factory callers retain their behavior.
  HTTP/HTTPS still uses the existing authenticated remote opener. Source
  preservation tests compare names, bytes, modes and modification times after
  success and refusal, including a valid source without `oldgen`; ordinary
  reads may update access times through the OS.

  **Before issue #125, memdolt had no clone command and transfers remained
  unshipped after issue #123. After it, only clone bootstrap ships.** The
  complete structural blast radius is:

  - `cmd/memdolt/root.go` registers `newCloneCommand`; every previous command
    remains. New `cmd/memdolt/clone.go` owns flags, help, empty-user refusal and
    success rendering through the unchanged `emit`. This output ordering
    binds clone alone; existing commands' output and close behavior remain.
  - New `internal/store/localdolt/clone.go` owns `Clone`, input validation,
    exclusive destination reservation, transfer, redacted diagnostics and
    post-transfer inspection. `cloneTransfer` reuses the pinned driver's
    underlying `CloneRemote` engine. Its SQL `DOLT_CLONE` wrapper would
    recursively delete the destination after a failure; memdolt never calls
    that wrapper. The environment is prepared without a genesis commit and
    closed with singleton caching disabled. The existing DSN builder checks
    destination compatibility. `inspectClone` reads the already-open committed
    root, `cloneSchemaVersion` uses Dolt's table iterator and SQL string
    unwrapping, and the existing version guard is reused. It requires core
    tables plus task, note/provenance and proposal columns; it is not a
    comprehensive schema/constraint audit. It never opens another engine,
    which would reinstate the native environment's temp-file sweep. Those
    stricter
    bootstrap checks bind `Clone` alone; `Store.Open` still creates a missing
    database, `Migrate` remains explicit and idempotent, and the direct lanes,
    review gate, offline repo status and sixteen MCP tools remain unchanged.
    Its source path now passes through `openCloneRemote` and the shared
    `cloneFilePath` decoder as described above. File sources avoid the reached
    factory's initialization; network authentication, origin registration,
    transfer, inspection, destination cleanup policy and output still hold.
  - New `internal/store/localdolt/clone_fs.go` contains environment writes and
    disables environment deletion/moves, including failed initialization's
    recursive cleanup and the old-temp-file sweep. This policy binds the
    filesystem supplied to this clone environment, not every Dolt filesystem
    or store. The NBS engine still manages its own transfer files. Ownership
    excludes cooperating memdolt processes; it does not coordinate foreign
    Dolt sessions. `cloneMu` serializes clone calls because Dolt's progress
    writers are global; it does not serialize every Store operation.
  - `go.mod` promotes the already-selected Dolt and go-mysql-server modules
    and the synthetic authentication tests' gRPC module to direct requirements.
    No dependency version, graph, or `go.sum` changes. The new `clone_test.go`
    files in `cmd/memdolt` and `internal/store/localdolt` add embedded
    push/clone/reopen, exact main/history/authorship/row/null-provenance,
    refusal, cleanup, schema, close-error and CLI-output checks. Synthetic
    loopback gRPC proves `--user` plus environment password reaches Basic
    authentication, visible authentication refusal/redaction, cancellation,
    and retention of a concurrently introduced foreign file. A separate
    injected initialization failure proves environment cleanup retains its
    foreign file; committed-root inspection and close also preserve an old
    foreign temp file and its timestamp. These are filesystem-transfer and
    local synthetic transport
    checks, not real-hub authentication or two-machine network acceptance.
  - This record and PRD §§5.2, 11.2 and 16 preserve the old transfer/lifecycle
    statements and the new bootstrap scope. No configuration editor,
    application push/pull, remote status/diff, conflict dialog, hub setup,
    topology backend, or full M4 acceptance claim is added.
- Transfer main: `memdolt push [remote]` and `memdolt pull [remote]` accept
  `--dir`, `--user`, and `--json`, defaulting to configured `origin`.
  **Before issue #127, issue #125 shipped only clone bootstrap and application
  push/pull remained deferred. After #127, main-only push and validated
  fast-forward pull ship; automatic merge and conflict resolution remain
  deferred.** At #127 no remote editor, URL/branch/refspec operand, force,
  prune or all-branches option was added. Before #129 the setup remedy was:
  stop the owner to configure a remote with `dolt remote add <name>
  <absolute-url>` in `.memdolt/dolt/memory`, or clone into a fresh repository.
  After #129, use the owner-aware `memdolt repo remote add` and `list` commands
  below. The transfer operand/force/prune restrictions still hold. Missing
  stores are never created or migrated.

  Push captures committed main and publishes only that immutable hash to
  remote main, creating it or advancing by fast-forward. Pull fetches main
  into a tracking ref and checks ancestry, the exact committed schema/column
  shapes and changed text before `DOLT_MERGE('--ff-only', hash)`. Equal or
  contained remote history is unchanged; divergence or incompatible metadata
  refuses. No merge, genesis or migration commit is made. Both refuse dirty
  main and active merge/conflict states. Pending proposals remain local.
  Local tags and their metadata are preserved; pull never follows remote tags
  or prints fetch progress, including through the live MCP owner's stdout.
  Fetched objects/tracking refs may remain after refusal; inspect local and
  remote main before retrying. Lost responses mean unknown outcome, without
  resubmission. Confirmed promotion remains reported if a later step fails.
  Success identifies operation, remote, captured local hash, remote hash,
  resulting local hash, changed/current status; close/output failures remain
  visible. Derived indexes, rendered files and adjacent config stay local;
  use existing index status/rebuild after imported text changes.

  Configured URLs use clone's explicit HTTP/HTTPS remotesapi and absolute
  file-URL restrictions, including credential/query/fragment/port refusal.
  The one native Windows `file://C:/...` stored spelling is restored to
  `file:///C:/...` and native decoded file-path spaces are re-escaped before
  validation. Remote names cannot select flags, refs
  or destinations. Only a validated stored SQL username is accepted in remote
  parameters; explicit `--user` overrides it, and neither means anonymous.
  Passwords come only from `DOLT_REMOTE_PASSWORD` in the executing process.
  Restart a running owner with the desired environment; caller passwords are
  never sent through IPC. No personal Dolt credential is loaded for transfer,
  and password/plaintext/URL-encoded/Basic forms are redacted from diagnostics.

  The complete structural blast radius for #127 is:

  - `cmd/memdolt/root.go` adds the two commands. New `transfer.go` owns their
    flags/help, existing-store check, existing direct/owner selection, complete
    typed operation and close-before-success rendering. Existing command
    behavior remains; `repo.go` changes only its formerly deferred push/pull
    help statement. Its offline status behavior and output remain unchanged.
  - `localdolt/transfer.go` adds `Push`, `Pull`, their options/results, remote
    preflight, clean-state check and immutable promotion protocol. These
    restrictions bind these transfers alone, not arbitrary native Dolt calls.
    `RequireExistingTransferStore` prevents their CLI callers from invoking
    `Open` on a missing store; ordinary `Open` still creates, `Migrate` remains
    explicit and clone keeps its existing exclusive bootstrap lifecycle.
  - New `localdolt/transfer_engine.go` registers one private external SQL
    procedure, `memdolt_transfer`, at process initialization. Only an
    unexported context capability from `runEngineTransfer` can invoke it;
    ordinary SQL and IPC Commit requests fail closed. It obtains the existing
    owner's session DbData and reuses pinned push/fetch actions, closing fresh
    remote handles. It does not open another local engine. Native SQL push/fetch
    could inherit personal credentials and their stored username could override
    `--user`; the new transfer path constructs a fresh remote with the selected
    username and clone's credential-free dial provider. Native Dolt procedures
    retain their own behavior. Pull reuses `openCloneRemote`, preserving file
    sources including absent oldgen; symbolic links inside selected file
    remotes are refused. No recursive cleanup or unrelated deletion is added.
    Before the issue #127 cycle-2 fix, `transferProcedure` called
    `actions.FetchRefSpecs`, whose non-shallow fetch also reached
    `FetchFollowTags`: it could replace local tag hashes and metadata and add
    remote-only tags before candidate validation, while `cli.Println` wrote
    unsolicited newlines to direct CLI or live MCP owner stdout. After the fix,
    the pull branch calls `FetchRemoteBranch` only for `refs/heads/main` and
    `SetHeadToCommit` only for the selected `refs/remotes/<remote>/main`.
    No tags are followed/replaced and no fetch progress is printed. These
    restrictions bind memdolt's pull path through `transferProcedure` alone;
    native Dolt fetch/tag operations and clone retain their own behavior.
    Captured push, file-source preservation, ancestry/schema/text validation,
    separate fast-forward promotion and retained-fetch remedies remain intact.
  - New `localdolt/transfer_schema.go` owns the maintained `transferTables`
    contract: exact application table/column sets, types, nullability and primary
    columns, plus explicit text coverage. The strict 32-character Dolt hash
    check precedes revision-identifier construction; SQL values stay bound.
    This is not a comprehensive index/constraint audit. Push scans persisted
    snapshot text; pull scans only added/changed column values versus captured
    local main. Coverage includes facts, decisions, tasks, notes and all five
    provenance fields, commands, both narratives, documents/chunks, proposal
    metadata and meta. Scan/config/read failures refuse upload or promotion.
    Unchanged local history is not rescanned by pull; fetched history can
    contain historical matching text. Existing write/review scanners remain.
    Before the issue #127 cycle-1 fix, this check omitted generation mode and
    expression while exempting `facts.live_key` from scanning as derived. A
    writable replacement or changed generated expression could therefore carry
    denied text through push and pull. After the fix, `transferLiveKey` parses
    `SHOW CREATE TABLE` at the exact captured/candidate commit with the pinned
    SQL parser and requires STORED generation plus the complete canonical
    `IF(superseded_by IS NULL, key, NULL)` expression before granting that
    exemption. Read/parse/shape failure refuses transfer without echoing the
    remote DDL. This added restriction binds `facts.live_key` in `Push`/`Pull`
    alone, not every generated column, clone inspection, migration or review.
    Existing column checks and scanners remain; no comprehensive index audit is
    added. The transfer regression now verifies upload and promotion refusal
    for writable, changed-STORED-expression and VIRTUAL fixtures, preserving
    remote files and local main.
  - `localdolt/localdolt.go` adds `Commit` to the existing `proposalMu` boundary.
    Before #127 only stage/accept/reject/expiry shared it; afterward those plus
    direct commits and complete transfers serialize on one owning Store.
    Their validation, attribution, batching, commit and review semantics still
    hold. At #127 reads and migrations were not added to that mutex; #129 adds
    remote configuration reads/writes alone. Other reads and migrations remain
    outside, and it does not coordinate foreign Dolt sessions. Transfer
    network latency therefore delays memdolt mutations; the existing ownership lock still excludes other
    cooperating memdolt processes.
  - `storeipc/operation.go` and `owner_store.go` add explicit typed push/pull
    operations and preserve populated result-plus-error responses. Token
    authentication, verified-owner routing, cancellation, no transfer timeout,
    bound arguments and single submission remain. The allow-list and private
    capability restrictions bind their named seams, not every SQL procedure.
  - `localdolt/transfer_test.go`, `transfer_auth_test.go`, `storeipc/transfer_test.go`
    and `cmd/memdolt/transfer_test.go` add local synthetic production round trips,
    history/data/provenance comparisons, refusal/preservation/interleaving,
    authentication/redaction/cancellation and lost/post-success-response checks.
    The cycle-2 regression in `cmd/memdolt/transfer_test.go` runs successful and
    denied pulls in subprocesses, directly and through a real `serve` process
    verified by authenticated IPC. It compares every local tag's name, commit
    hash, tagger, email, timestamp and message, including a conflicting remote
    tag and a remote-only tag that must remain absent. It checks raw CLI stdout
    (one JSON line on success, empty on refusal), empty owner stdout when no
    MCP input was sent, clean owner shutdown and reopened main/working-set state.
    These change no shipped behavior and establish no real-hub, two-machine,
    cross-version or full-M4 acceptance. This record and PRD §§5.2/11.2/16
    preserve the phasing. No dependency, migration, MCP tool, remote editor,
    conflict dialog, topology backend or hub deployment changes.
  **Divergence delivery (issue #137).** Before this delivery, the #127 record
  above refused all divergence and made no merge commit, while #131 only
  previewed it. After #137, `pull` retains push, fast-forward and contained
  history behavior and auto-merges compatible independent committed changes
  in one attributed commit whose ordered parents are the captured local and
  remote hashes. Dirty/staged/active-merge states still refuse. No migration,
  competing owner, proposal/tag publication, or source/artifact rewrite is added.

  `memdolt pull --json` returns a visible `conflicted` report and nonzero exit
  for actual conflicts, with complete base/ours/theirs/merged nullable rows,
  native row blame and reachable commit author/email/date/message. Main and
  both roots are restored before output or human interaction. Use
  `memdolt pull [remote] --resolve <file>` (or `-` for stdin) to submit
  `localCommit`, `remoteCommit`, and exactly one `choices` entry per conflict.
  A data choice names `conflict` and `take=ours|theirs|manual`; manual supplies
  every writable column in `row`, including nulls, omitting generated
  `facts.live_key`. A live-key choice uses `take=winner|manual`, a displayed
  `winner` ID, and the full winner row for manual values. Losers receive
  supersession links and remain present. Identity/provenance changes, duplicate
  JSON keys, unknown fields, incomplete choices, coercion/truncation and stale
  local or fetched remote heads refuse before promotion. A done task requires
  an explicit `reopen=true` choice to become open/blocked; choosing deletion
  cannot erase a completed task. No timestamp heuristic chooses a final row.

  Supported data conflicts are same-primary-key rows in facts, decisions,
  tasks, notes, commands, narratives, documents, chunks and proposal metadata.
  Chosen document deletion refuses if it would cascade into other chunks.
  Actual distinct-row uniqueness repair is restricted to `facts.live_key`:
  select an existing durable winner and retain every loser by supersession.
  The shared verifier clears attributed already-satisfied UNIQUE records on
  maintained tables without changing their rows. Unresolved document-path or
  chunk uniqueness, foreign-key/unknown constraint records, metadata/schema
  conflicts and unattributed records require Dolt inspection or compatible
  upgrade/repair. Every final merge requires empty data and constraint surfaces,
  all maintained UNIQUE/FK checks, and acyclic, non-dangling fact/decision
  supersession links. Unknown tables/columns or differing committed DDL refuse.

  `repo_pull` and `repo_push` are real typed tools. Compatible pull merges use
  the requesting MCP agent; human-confirmed conflict merges use `user`. MCP
  cannot supply an author, password, SQL, or direct resolution object. Modern
  clients receive one conflict form per round, with a single-use continuation
  after nine; genuine legacy clients receive all remaining conflicts in one
  form. No list is truncated and no prefix is promoted. Each opaque state has
  a two-minute expiry and is stored only as a hashed lookup in process-local
  SQLite, bound to repository/client/action, selected remote/user, exact heads,
  conflict snapshot, position and accumulated choices. Every state is consumed
  before interpreting its response; missing/forged/replayed/expired/mismatched
  states, cancellation, decline, restart, or pre-promotion storage/response
  failure cannot merge. Unsupported form capabilities or missing client identity
  return the CLI remedy. The final complete operation re-fetches and revalidates
  both heads; changing local main during review requires a fresh review.

  The pinned `DOLT_COMMIT` is the promotion point: it can advance main before
  `database/sql.Tx.Commit`. Every choice, deny-list and invariant check runs
  before that call. A returned commit hash remains reported if later SQL
  finalization, connection/output/bookkeeping or transport work fails; a lost
  reply reports an unknown outcome and never replays. No durable transaction
  spans human interaction. Capturing, preview rollback and final mutation use
  the existing owning Store `proposalMu`; foreign Dolt sessions remain outside
  that boundary. Fetch still retains only its objects and selected tracking
  ref, and never follows tags or mutates the source. Passwords remain solely
  in the executing owner's environment. Existing proposal contradiction/review,
  agent provenance, note flush, global exclusion and no-delete expected-commit
  acceptance policies remain intact.

  The complete structural blast radius for #137 is:

  - `localdolt/transfer.go` extends `TransferOptions` with attributed author and
    complete optional resolution, and `TransferResult` with base, conflicts,
    verified cleared records and remedies. `Push` retains its main-only scan
    and fast-forward publication; `Pull` adds divergence handling and exact-head
    resolution checks. `transferHooks` supplies deterministic verification seams.
    Existing preflight, remote credential selection/redaction, file-source
    preservation, no replay and result-plus-error boundaries still hold.
  - New `localdolt/pull_merge.go` owns `pullMerge`, the conflict/report/repair
    helpers and final application-invariant checks. These stricter merge rules
    bind divergent pull alone, not fast-forward pull, every Store write or
    proposal accept. Its `repoMergeTransaction` is shared with
    `repo_status.go`'s existing `previewRepoMerge`: status retains its read-only
    assessment, rollback verification and original constraint scope; it does
    not gain resolution or promotion. Native revision-qualified blame views
    change database context internally, so pull reads them after rollback and
    recreates the captured merge for final resolution. Fixed schema allow-lists,
    validated 32-character revision literals and bound values apply to all new
    pull queries. Existing `review.go` verification/merge helpers are reused;
    their proposal allow-list, contradiction and cleanup policies are unchanged.
  - New `localdolt/pull_json.go` shares strict operator JSON decoding between
    CLI files/stdin and MCP form strings. Those decoder restrictions apply to
    these operator inputs alone, not every JSON or arbitrary SQL caller.
  - `cmd/memdolt/transfer.go` adds pull-only `--resolve`, rooted regular-file
    reading, detailed help and conflict output. Push flags and direct/verified
    owner selection remain. Pull now prints structured confirmed effects on a
    later failure; ordinary refusals retain visible diagnostics. Root command
    registration remains unchanged. Existing `storeipc/operation.go` and
    `owner_store.go` carry the expanded typed options/results unchanged through
    their existing one-submit application operation and error envelope; no new
    raw operation, password field or competing engine is introduced.
  - `mcpserver/tools.go` adds two registrations to the nineteen existing tools.
    New `mcpserver/transfer.go` owns typed handlers, human forms, continuation
    and structured error preservation. The count of twenty-one binds
    `RegisterTools`, not arbitrary SDK servers. `elicitation.go` adds one
    pending-pull payload; `elicitation_state.go` stores it in the existing
    ephemeral SQLite row. Prior fact/review states, hashed single-use tokens,
    expiry, per-client attribution, note batching and shutdown still hold.
    No durable schema or dependency changes.
  - New `localdolt/pull_merge_test.go`, `storeipc/pull_merge_test.go`,
    `mcpserver/transfer_test.go` and `cmd/memdolt/pull_merge_test.go` exercise
    synthetic direct/owner/CLI/MCP merges, history/provenance, all supported
    policies and refusals, manual input, multi-round/legacy/capability paths,
    state failures, interleavings and lost/late results. Existing localdolt
    transfer/status and storeipc transfer tests replace only their obsolete
    independent-divergence refusal expectations. MCP `tools_test.go`, CLI
    `serve_test.go` and `host_templates_test.go` retain existing assertions and
    recognize the implemented transfer tools. Tests change no production policy.
    After #139 integration, `TestPullCLIIntegratesHumanFactAndDecisionWrites`
    creates both conflict surfaces through the real human CLI commands,
    resolves the complete pull directly and through a live owner, then reopens
    shared fact/decision readers and exercises human verification/summary/
    supersession. The generic confirmed-commit and failed-stage residue
    changes from #139 remain unchanged beside pull's own promotion boundary.
  - `mcpserver/instructions.md`, all three wrap-up templates, this AGENTS record
    and PRD §§5.2/6.3/11.1/11.2/16 document the same behavior and limits. The
    templates add guidance for transfers already requested by the operator;
    existing per-item approval and queued-note/provenance rules still hold.
    Hub deployment, version/two-machine acceptance, optional live-SQL topology,
    global promotion and full M4/parity completion remain separate.

  **Issue #137 review cycle 1.** Before this correction, `decodePullJSON`
  relied on `encoding/json`, which silently replaced malformed UTF-8 and lone
  UTF-16 surrogate escapes before validation; exact SQL readback therefore
  compared against already-altered text. After it, `pull_json.go`'s shared
  `ValidateJSONUnicode` runs before either decoder entry point can convert
  strings. `JSONUnicodeReader` supplies the same rune/escape check to streaming
  consumers, preserving bytes, framing, valid surrogate pairs and literal
  U+FFFD. JSON grammar still belongs to the existing decoder. The guard adds
  a fixed bufio buffer and at most one 12-byte encoded pair, with no new message
  size limit or full-message copy. Existing decoder/file/message buffering is
  otherwise unchanged. Already-rewritten text from a third-party client cannot
  be recovered or distinguished from a deliberately supplied replacement rune.

  `TransferOptions.ValidateText` now checks every typed Go string before
  `localdolt.transfer` executes or `OwnerStore.transfer` marshals it; invalid
  input refuses before submission, with no retry. `storeipc.operationArgs`
  validates raw Unicode before unmarshalling every typed operation that uses
  that helper, not just pull. Other decode paths and other operations' existing
  pre-marshal checks retain their scope. SQL binding, deny-list checks, owner
  credentials, exact heads and the #139 commit/failure-residue policies remain.

  `cmd/memdolt/serve.go`'s `newServeCommand` now constructs the SDK IOTransport
  with that guarded stdin reader and the original stdin closer; `stdioWriter`
  preserves StdioTransport's no-close stdout behavior. Before the correction,
  the SDK could replace malformed outer form-response text before tool
  middleware; afterward the actual stdio byte stream rejects it first. The
  SDK connection remains intact, including its private negotiation hook,
  legacy batch rules and cancellation. This wire boundary binds the shipped
  `newServeCommand` transport and explicit `JSONUnicodeReader` consumers;
  `mcpserver.New` alone and arbitrary transports injected into `runServe` do
  not acquire it. `runServe` retains protocol/pending-work/IPC/store shutdown
  ordering. A wire rejection ends that protocol connection; existing orderly
  note-shutdown behavior remains separate from a refused pull resolution.

  Before the correction, `writePullRow` froze actor/actor_raw but omitted
  session_id, agent_id, provider_id, model_id and variant. After it, all five
  schema-v4 provenance columns are immutable in manual choices too. Unchanged
  nullable metadata and an explicitly selected complete existing ours/theirs
  image remain supported. A new regression also exposed absent row images
  serialized as null while the generated MCP schema required objects. After
  the correction, `PullConflictRow` follows `RepoRowDiff`: absent base/ours/
  theirs/merged images are omitted, and every nullable cell in a present image
  remains explicit. Typed fallback, refusal and confirmed result-plus-error
  reports now validate for insert/insert and delete/edit conflicts.

  Added checks in existing localdolt/CLI/IPC/MCP pull test files reproduce both
  reported defects, compare main and working/staged roots on refusal, exercise
  every note provenance column, preserve valid text and sides, and send actual
  malformed protocol bytes after the test client's serialization. Existing
  `mcpserver/server_test.go` connection helpers use guarded real IO pipes;
  `cmd/memdolt/serve_test.go` covers the original closer, actual stdio modern
  discovery, genuine legacy initialization, a batch larger than 64 KiB, and
  the SDK's newer-legacy batch refusal. These fixtures alter no production
  policy. This record, PRD §11.2 and server instructions retain the exact
  before/after and scope; no dependency, schema migration or approval bypass
  is added.

- Configure repository remotes: `memdolt repo remote list` and
  `memdolt repo remote add <name> <absolute-url> [--user <sql-user>]` support
  `--dir` and `--json`. **Before #129, remote setup required native Dolt with
  the owner stopped as recorded above; after it, these two commands use the
  existing direct/authenticated-owner boundary.** List returns name-sorted
  entries (JSON `{"remotes":[]}` or `no remotes configured` when empty). Add
  reports the persisted name, URL and optional user only after confirmation
  and successful close. Close/output failures stay visible; confirmed
  persistence remains in later-failure diagnostics.

  Names, URLs and usernames obey the transfer contract. URLs are explicit
  HTTP/HTTPS remotesapi URLs including a database path or absolute file URLs;
  credentials, query/fragment, unsupported schemes/ports, ambiguous file-path
  encoding, option-like names and invalid users are refused without quoting
  rejected values. File remotes reject usernames. Configuration contacts no
  remote, requires no password or existing filesystem target, and never creates
  or migrates a store. Transfers alone use the executing process environment's
  `DOLT_REMOTE_PASSWORD`; owner clients never send a caller password over IPC.
  A named file target must exist by transfer time, as before #129.

  Only native Dolt remote configuration changes. Existing remote fields,
  committed main/history, working/staged memory, proposals, tags, derived
  indexes, rendered files and adjacent user config remain intact. Dirty memory
  is allowed. Occupied names and unsafe legacy URL/parameter data refuse;
  reads canonicalize only the established native file URL spellings in their
  returned report, without rewriting entries. No remove, replace, force,
  arbitrary parameter/refspec, remote status/diff, merge/conflict dialog,
  topology configuration or hub operation is added.

  The complete structural blast radius for #129 is:

  - `cmd/memdolt/repo.go` adds `remote` registration and broadens the parent
    help; `repo status` keeps its local-only behavior. New `remote.go` owns
    the two commands, operand/user preflight, existing-store check, one typed
    operation, close-before-output ordering and empty/sorted rendering.
    `runRepoRemote` alone owns that finalization ordering, not every CLI read.
    `transfer.go` changes only the obsolete native-Dolt-only setup help.
  - `localdolt/clone.go` extracts pure `validateRemoteURL` from
    `validateCloneRemote`; clone's user/password checks and URL acceptance
    remain. The pure helper also serves configuration and transfers, but does
    not replace their credential gates or validate arbitrary project URLs.
    `localdolt/transfer.go` reuses `sanitizedRemote` for stored URL/parameter
    validation and names the new setup remedy. Its selected username override,
    executing-process password requirement, main capture/fetch/promotion,
    schema/deny-list checks and source/tag preservation remain unchanged.
  - New `localdolt/remote.go` defines the password-free `Remote` shape,
    `ValidateRemote`, stored-data sanitizers, `ListRemotes`/`AddRemote` and
    private `memdolt_remotes` procedure. `RequireExistingTransferStore` now
    protects these CLI commands as well as push/pull; ordinary `Open` still
    creates missing stores. Both configuration methods share `proposalMu`
    with transfers/direct commits/proposal mutations and check committed
    main with `validateTransferSchema`, without requiring clean memory.
    Other reads/migrations and foreign Dolt sessions remain outside that
    mutex. The private procedure uses only the owning session's native
    filesystem/cache; ordinary SQL and owner Commit lack its unexported
    context capability. This restriction binds `memdolt_remotes`, not all
    SQL procedures; `memdolt_transfer` retains its separate capability.
  - `remoteProcedure` verifies path containment, loads native `RepoState`,
    refuses an occupied name or conflicting backup URL, saves the added
    `env.NewRemote` with Dolt's default fetch specification and only the optional
    SQL-user parameter, and reads back the exact stored entry. It uses native
    temporary-file/sync/rename saving, with native platform crash guarantees.
    Native `SessionStateAdapter.AddRemote` publishes its cache before saving;
    it retains that behavior. Memdolt's configuration seam instead publishes
    the cache only after confirmed persistence, including populated-result
    reporting when a later save-finalization error occurs. A successful list
    refreshes the in-memory remote view from validated native entries, so an
    earlier lost response/read-back can be inspected before transfer. These
    ordering/preservation rules bind this configuration seam, not native Dolt
    writers or foreign processes. It never removes memory/proposal refs or
    unrelated files.
  - `storeipc/operation.go` extends `Backend` and the explicit allow-list with
    typed list/add operations and result-plus-error preservation for add.
    `owner_store.go` validates add operands before encoding (including URL
    credentials), submits once, and directs unknown outcomes to
    `memdolt repo remote list` before retry. Token authentication, verified-owner
    routing, no competing open after probe/auth failure, cancellation and
    existing operation behavior remain. No password field or new timeout is
    introduced. The operation additions do not add MCP tools.
  - New `cmd/memdolt/remote_test.go`, `localdolt/remote_test.go` and
    `storeipc/remote_test.go` exercise production commands directly and through
    authenticated IPC, named file push/pull, synthetic stored-user transport,
    sorted/empty output, invalid/duplicate/missing/unsafe/schema refusals,
    memory/proposal/tag/artifact preservation, lock serialization, native
    save failures, finalization and lost/confirmed-error responses.
    `cmd/memdolt/transfer_test.go` updates only its setup-remedy assertion.
    These tests change no shipped behavior and use no personal credentials.
    This record and PRD §§5.2/11.2/11.3/16 describe the same phasing. No
    dependency, migration, topology backend or full-M4 acceptance is added.
- Inspect local repository state: `go run ./cmd/memdolt repo status` supports
  `--dir` and `--json`. Before issue #123 there was no `repo` command; after it,
  this local-only status reports the resolved store path, main commit, schema,
  main working-set table changes with staged/status values, and repo/global
  pending-target counts. It reuses authenticated owner routing and the existing
  pending-list reachability filter, so unchanged merged proposal residue is
  excluded. It refuses an absent database before opening, retains schema
  migration/upgrade refusals, and reports probe, read, output, and close errors.
  Direct opens may update ownership-lock bookkeeping but change no durable
  memory, proposal head, or working set. Only `cmd/memdolt/repo.go` owns this
  status behavior; existing commands and MCP discovery retain their contracts.
  No remote is contacted or assessed. After #123, remote status/diff,
  transfers, conflict dialogs, hub setup, and the remaining M4 acceptance
  obligations stayed pending. Issue #125 adds only the clone transfer above;
  issue #127 adds the bounded push/pull above; the other obligations remain
  pending (PRD §§11.2 and 16).

  **Before issue #131, the paragraph above described the default: status was
  always offline and lived entirely in the CLI. After #131, `memdolt repo
  status [remote] [--local] [--diff] [--user <sql-user>] [--dir <repository>]
  [--json]` defaults to configured origin, and `--local` retains that former
  offline behavior.** Missing origin gives an explicit `no-remote` local
  report and setup remedy; an explicitly missing named remote refuses.
  `--local` cannot be combined with a remote, `--diff` or `--user`.
  All prior local fields remain. The report adds selected remote, exact
  captured remote main and merge-base hashes, status and assessment. Status
  distinguishes `current`, `ahead`, `behind`, `diverged-mergeable`, `conflicted`
  and `diverged-unassessed`; dirty divergence explains the clean-main
  prerequisite. Ancestry/schema/read failures are visible refusals.
  `--diff` uses actual Dolt differences FROM captured local main TO captured
  remote main, sorted by table and row key, with added/modified/deleted rows.
  Values are SQL-rendered strings or explicit NULLs; a nonexistent before or
  after row image is omitted. Changed table definitions are included. Ordinary
  status omits row bodies. Missing/unvalidated remote data gets no claimed diff.

  The complete structural blast radius for #131 is:

  - `cmd/memdolt/repo.go` keeps `repo remote`, replaces status's scattered
    reads with one typed store operation and closes before success output.
    The CLI-local `readRepoStatus`, `repoStatusReport` and `repoTableChange`
    become `readLocalRepoStatus`, `RepoStatusReport` and `RepoTableChange` in
    localdolt; no other command's report or output ordering changes.
    Its former local fields and pending-target meaning remain; only status's
    default becomes remote-aware. The existing-store guard now reuses
    `RequireExistingTransferStore`, also retaining its managed-path checks.
    `openCommandStore` still fails closed on owner discovery/authentication;
    ordinary `Open`, `Migrate`, and other commands retain their behavior.
  - New `localdolt/repo_status.go` owns `RepoStatusOptions`, the shared report,
    `Store.RepoStatus`, immutable schema/diff reads and `previewRepoMerge`.
    `repoDiff`/`repoDiffRows` produce `RepoDiff`, `RepoTableDiff` and
    `RepoRowDiff` from validated revisions with bound values; their nullable
    row representation does not change review's `ProposalChange` format.
    `proposalMu` now also covers the complete status capture/fetch/preview/
    rollback, including offline capture. Before #131 it covered the mutations,
    transfers, remote configuration and #133 render described above; all retain
    their ordering and semantics. Other reads/migrations remain outside, as do
    foreign Dolt processes. Status may delay participating writes during fetch.
    It reads committed main's version and snapshots, preserves working/staged
    data and proposals, and never creates a merge/genesis/migration commit.
  - Status reuses `memdolt_remotes`' private owner capability to refresh
    validated native configuration, then `configuredTransferRemote`,
    `validateTransferFile`, `runEngineTransfer` and `redactCloneError`.
    Their prior URL/user/password, anonymous, explicit-user precedence,
    source-preserving opener, cancellation, credential redaction and absence of
    personal credential loading remain. The main-only fetch/tag/stdout restrictions
    previously bound the pull arm of `transferProcedure`; after #131 the same
    arm also serves status. It still updates only the selected tracking ref;
    fetched objects/ref updates may remain even after refusal. Push/pull
    promotion rules, clone and native Dolt procedures remain unchanged.
  - `statusSchema` reuses `validateTransferSchema`; `statusConstraints` and
    `statusUniqueKeys` additionally check the known unique/FK constraints.
    Only status owns that tighter constraint
    contract; transfer's existing column/generation checks remain unchanged.
    `previewRepoMerge` requires identical validated base/local/remote DDL,
    clean main and no active merge/conflict. It merges the captured remote
    hash inside a transaction, reads both conflict surfaces, verifies the
    known unique indexes and document FK against actual merged rows, and
    always rolls back, including on cancellation. It verifies main hash,
    working/staged roots and clean merge/conflict state after rollback.
    Rollback/restoration failures refuse and name inspection remedies.
    This preview never promotes or resolves user conflicts; unknown records,
    schema changes or unassessable ancestry refuse. The restriction binds
    this preview alone, not every Dolt transaction or review acceptance.
  - `localdolt/review.go` extracts `pendingProposals` for a caller-held
    connection, retaining `PendingProposals`' reachability/count semantics.
    It extracts `verifyMergeViolations` behind the unchanged review table
    allow-list; status supplies its own validated table/key list. Attribution,
    known-constraint verification and clearing only already-satisfied records
    remain. `requireConstraintHolds` adds an internal error identity for a
    proven duplicate without changing its check or existing diagnostics.
    Shared `surfaceSummary`, `readViolations`, `classifyViolations`,
    `rowsTouchedByMerge`, `uniqueIndexColumns` and `requireSurfacesEmpty`
    retain their existing read, attribution and final-verification behavior.
    Review still promotes only through its existing guards/commit protocol;
    status uses these helpers only inside a rolled-back preview. Status does
    not call contradiction inference or the promotion deny-list scanner.
  - `storeipc/operation.go` and `owner_store.go` add typed `repo_status` to
    `Backend` and the explicit allow-list. One request carries the whole
    operation; no caller password travels over IPC and no retry occurs after
    a lost response. Existing token authentication, cancellation, owner
    authority and other operations retain their contracts.
  - `mcpserver/tools.go` adds `repo_status` and new `mcpserver/repo.go` delegates
    its typed input/output to that same store method. Before #131 production
    registered seventeen tools: sixteen M3 tools plus #133's `render`. After
    it, those seventeen remain and `repo_status` is the eighteenth.
    Discovery, modern/legacy agent attribution, tool-list cache,
    notes, review and shutdown remain. This count binds `RegisterTools` alone.
    Global proposals are repository-local counts, never a global-store read;
    `repo_pull` and `repo_push` remain absent until their MCP operations ship.
  - New status tests in localdolt, storeipc, CLI and MCP cover production
    direct/owner/modern/legacy paths, exact diffs, conflicts/constraints,
    preservation/interleavings, malformed schemas and lost/cleanup/output
    failures. `transfer_auth_test.go` extends its synthetic authentication/
    cancellation cases to status. `repo_test.go` retains the offline fixtures
    using `--local` and adapts failure injection to the single operation;
    clone/transfer tests also explicitly select offline inspection. MCP tools
    and serve tests update exact registration expectations. Tests change no
    production behavior. PRD §§11.1/11.2/16 preserve the same before/after.

  Evidence is local synthetic filesystem/transport testing with the pinned
  driver. Derived/index/render/configuration files remain local and unchanged.
  No dependency, migration, conflict-resolution UI, hub deployment/auth setup,
  measured hub/client version acceptance, optional remote Store or full M4
  completion is added. The two-machine acceptance gate remains pending.
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

  Before issue #116, those self-clearing conditions were the only warnings.
  After it, the optional `mcp-registration-opencode` check also warns and exits
  zero when no supported parsed registration is found; it does not repair host
  config. The prior ownership, schema, and recall checks retain their behavior.
- Ingest repository reference documents: `memdolt doc add <file> [--title <title>]`,
  `doc ls` (`list` aliases it), `doc show <id-or-path>`, and `doc rm <id-or-path>`
  support `--dir` and `--json`; add/rm also accept the existing `--actor` flag.
  **Before issue #132, the documents/chunks schema and retrieval readers existed,
  but ingestion and `doc_add` were deferred. After it, these repository commands
  and the real typed `doc_add` tool ship.** No global flag, global document
  backend, code index, other deferred tool, or full-M5 completion is implied.

  Add reports created/updated/unchanged, metadata, ordered chunk breadcrumbs and
  the changed operation's commit; show includes bodies, and remove reports
  removed/not-found. Byte-identical SHA-256 input produces no commit or chunk
  replacement, including when only the requested title differs. Changed bytes
  retain the document ULID and replace its whole chunk set with fresh ULIDs in
  one transaction/commit. Removal deletes chunks and their parent in one commit.
  The existing unique path/index/FK schema is unchanged; a database collation
  collision between distinct filesystem paths refuses instead of overwriting
  another document. Source is always `user`, as §7.6 requires; the actual
  normalized caller authors the commit. Source/path/title never grant human
  review authority, and MCP `user` still means `agent:user`.

  Before the #132 missing-source identity fix, full-path canonicalization fell
  back to lexical cleaning when the source disappeared, losing still-existing
  directory aliases and making original-path show/remove return not-found.
  After it, `documentIdentityPath` resolves the nearest existing directory
  ancestor and reattaches only missing literal components. Its sole caller,
  `resolveDocument`, serves `DocShow` and `DocRemove` through both CLI and owner
  routes. Exact stored paths and ULIDs still work without a resolvable source;
  otherwise unexpected resolution errors remain visible. No alias registry or
  inference of deleted symlink targets is added. The deterministic CLI regression
  keeps the original aliased operand after leaf/parent deletion for direct and
  owner calls, and localdolt checks errors plus exact identities. These lookup
  changes do not alter ingestion confinement, owner-file protection or writes.

  The chunker ports memhub v0.2.0's actual rules: nonempty ATX headings of levels
  1–6, heading lines retained, ancestor breadcrumbs joined by ` > `, and an
  optional preamble chunk. LF/CRLF line endings become LF in chunks while hashes
  and byte counts use original UTF-8 bytes. Fences start with three backticks or
  tildes and close on three or more of the same family, even a shorter run;
  headings/blank lines inside them remain body text. Above the 2000-Unicode-
  character soft target, paragraphs pack greedily at blank lines outside fences;
  one oversized paragraph/fence stays intact. It is not a CommonMark parser.
  Empty/whitespace files are valid zero-chunk documents. Paths, titles and
  breadcrumbs must fit their existing 1024/512/1024-character columns; a single
  chunk must fit the 65535-byte TEXT body. Invalid UTF-8, unsafe paths, widths,
  deny/config failures and non-regular sources refuse before durability.

  Ingesting into an empty documents table enables
  `[retrieval] include_docs_in_default`, with an explicit result/notice when the
  setting changes. This is the tagged baseline's empty-table trigger, so removing
  every document resets it. Later unchanged/changed ingests and second documents
  preserve a deliberate opt-out. The existing retrieval modes, filters and scoring
  floors remain: default docs require a rerank to survive; explicit `doc_chunk`
  queries also work in FTS. Run `index status`/`index rebuild` after changes.
  Old vectors become orphaned and replacement chunks missing until rebuild;
  deleted/replaced chunks cannot masquerade as current vectors or source rows.
  The config flag and side-store are machine-local, not transferred with Dolt.

  MCP relative file paths and relative `[doc] allowed_dirs` entries start at the
  repository root. A source must resolve under the canonical root or a resolved
  allowed directory; unresolved entries grant no access. Canonicalization plus
  `os.Root` bounds the subsequent read against symlink escape. An existing invalid
  or unreadable config fails closed; an absent config means repo-only access.
  CLI paths are explicitly selected relative to the caller's working directory
  and may be outside those roots. Both surfaces scan path, title, source, raw and
  normalized caller, headings and content before committing. Config finalization
  rereads and preserves unrelated TOML values semantically, writes/syncs a new
  file and uses rooted rename. Comments/formatting need not survive. Detected
  concurrent config edits refuse; foreign edits in the final read/rename interval
  and stronger cross-platform crash guarantees are not claimed.

  **Before the #132 owner-credential fix,** root confinement and optional
  deny-list rules allowed `.memdolt/server.pid` when rules were absent or empty.
  The owner token could enter the immediate `doc_add` chunk response and durable,
  searchable memory. **After the fix,** both CLI and MCP refuse this store's
  known owner metadata path before opening it as a source, and
  `checkDocumentOwnerFile` verifies the opened source's identity against that
  protected file before `io.ReadAll`. Canonical paths, symlink aliases and hard
  links cannot bypass it. The already-open metadata directory bounds the check;
  the owner file is opened only for `Stat`, never content. Missing owner metadata
  permits ordinary direct use; link/type/open/stat/identity/close failures during
  required verification refuse without document, commit or config effects.
  Both compared identities come from opened handles, avoiding a negative Windows
  `SameFile` answer that could hide a later path-lookup failure.

  This protection binds `readDocumentFile` and thus every `DocAdd`, independently
  of `DocAddOptions.Confined` and configured regex rules. Ordinary CLI root freedom
  and all other deny-list semantics remain. It protects this store's known owner
  file and file aliases, not arbitrary copied secrets or every filesystem/SQL
  reader; it changes no existing stored rows, history or IPC token lifecycle.
  That is the #132 boundary. After #140, selected import-bundle reads and
  existing export-output reads are additional explicit callers of the same
  opened-identity guard. DocAdd behavior remains; readers that do not call
  the guard still inherit no automatic credential-file protection.
  The changed structure is `document_file.go`'s named-path/identity guard,
  `documents.go` passing the existing metadata handle, CLI help and MCP input
  description, new `mcpserver/doc_owner_test.go`, the added CLI/localdolt
  regressions, and this AGENTS/PRD record. Synthetic absent/empty-rule fixtures
  cover CLI and modern/legacy MCP refusals, real symlinks/hard links, no response
  content or persistence/config effects, protection-check errors and retained
  ordinary external-file CLI ingestion. No real user token is read by those tests.

  The complete structural blast radius for #132 is:

  - New `localdolt/documents.go` defines the document/options/result/chunk types
    and `DocAdd`, `DocList`, `DocShow`, `DocRemove`. Add/remove hold the existing
    `proposalMu` through reading, mutation and config finalization. Existing
    direct writes, proposals, review and transfers keep their own rules; foreign
    Dolt sessions and migrations remain outside that mutex. Document readers pin
    one immutable main hash and exclude dirty/proposal rows. The named
    `documentConn` checks current schema without migration and validates hashes
    before constructing revision-qualified database identifiers; all source
    paths/IDs/values stay SQL parameters. These restrictions bind the document
    methods, not every Store read or native Dolt writer.
  - New `document_markdown.go` owns only the tagged chunk/title rules above;
    existing code-index plans and retrieval text formatting remain unchanged.
    New `document_file.go` owns document argument/width validation, canonical
    source reading, `[doc]`/include-flag config decoding and first-document config
    replacement. Only `DocAddOptions.Confined` enables the MCP root restriction;
    production `Toolset.docAdd` always sets it, while CLI add leaves it false.
    Managed document configuration refuses linked directories/files. These are
    document-seam restrictions, not new guarantees for all filesystem readers.
  - `localdolt/localdolt.go` retains `commitConn`'s validation, transaction,
    deny-list and opt-in clean guard. Its guard diagnostic previously named only
    the session-note batch; it now names a guarded write because documents also
    opt in. Note batching and other `CommitRequest` behavior are unchanged.
    Existing schema/migrations, ULID helper, `RequireExistingTransferStore`,
    `clonePath`, committed retrieval/embedding readers, index status/rebuild,
    actor normalization and review gates are reused without behavioral changes.
  - New `cmd/memdolt/doc.go` owns flags/help, caller-relative path resolution,
    pre-open existing-store refusal, direct/verified-owner selection and output.
    `runDoc` alone owns its close/error/output ordering; a confirmed mutation is
    emitted with an error field and nonzero exit when a later step fails. A
    first-document config failure gives the confirmed ID/commit plus an explicit
    config remedy, never a replay. `root.go` only adds the command registration;
    every existing CLI command retains its prior behavior.
  - New `internal/mcpserver/doc.go` owns typed `file`/optional `title` input and
    document output; it retains confirmed data alongside `IsError` for a late
    failure. Before sibling integration, `tools.go` added its one real
    registration to the prior sixteen. After integrating #133, its seventeen
    tools including `render` remained and `doc_add` became the eighteenth.
    After integrating #131, its eighteen tools including `render` and
    `repo_status` remain and `doc_add` is the nineteenth.
    Discovery, cache hints, modern/legacy agent-only attribution, note batching,
    elicitation and shutdown behavior remain. The exact registered set binds
    `RegisterTools` alone, not arbitrary SDK servers.
  - `storeipc/operation.go` adds four explicit document operations to `Backend`
    and the allow-list, with result-plus-error mutation replies; `owner_store.go`
    submits each complete mutation once and names doc ls/show inspection after
    a lost response. Existing token gates, bound values, cancellation, visible
    owner failures and no-fallback routing remain. No new endpoint or timeout.
  - New `localdolt/documents_test.go`, CLI/MCP/storeipc `doc_test.go` files test
    the tagged cases, real committed lifecycle/index/recall, empty/concurrent
    identity, SQL constraint rollback, dirty/proposal isolation, path/config/
    width/deny refusals, direct/owner CLI, modern/legacy MCP, symlinks and
    lost/confirmed-late replies. `tools_test.go` and `serve_test.go` extend tool
    expectations; existing checks remain. This record and PRD §§7/8/11/16 add
    before/after phasing. No dependency, durable migration or global support is
    added, and these local fixture checks establish no cross-machine path parity.

  Document/render integration retains #133's renderer, IPC method, instructions
  and templates. Both complete operations share the existing `proposalMu`.
  The document CLI tests now exercise configured render output directly and
  through the live owner after first-document config finalization; the MCP
  tests exercise both tools on modern and legacy sessions. They verify the
  document commit is render's source and appears in real activity, while
  document metadata/chunks and hash no-op behavior remain unchanged. The
  renderer's category set remains #133's; no document-body section is added.
  After integrating #131, the same CLI/MCP checks exercise `repo status --local`
  (`local: true` over MCP) and default no-remote status against the document
  commit. Both preserve the stored document and main hash. `RepoStatus` retains
  #131's full operation, remote-aware default and explicit offline option;
  document add/remove, render and status share the existing mutation mutex.
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
- Verify OpenCode session metadata: `memdolt opencode session-info <current-session-id>`
  reads the API without a store; `memdolt opencode wrap-up-note <current-session-id>
  [text]` verifies first, then writes one note (stdin supplies omitted text).
  Both support `--json`; the writer also accepts `--dir`. Workflows must take
  the current ID from host context and never discover or guess another session.
  The CLI verifies a supplied ID against the API and cannot authenticate where
  the caller obtained it. Before issue #116, neither command, the committed
  registrations, nor the provenance migration existed. After it, the complete
  structural blast radius is:

  - `.mcp.json` and `opencode.json` add coexisting Claude Code and native V2
    OpenCode registrations for `memdolt serve`. OpenCode adds the plural
    `commands` map and a `skills` path array for the three core templates.
    These new artifacts replace no host config and install no binary or wrapper.
  - `cmd/memdolt/doctor.go` adds parsed repository/user JSON/JSONC detection at
    `mcp.servers.memdolt` or supported V1 `mcp.memdolt`; unsupported similarly
    named paths and malformed files do not count. This recognition rule binds
    `openCodeConfigRegistersMemdolt` and the doctor check that calls it, not
    arbitrary config readers. It checks for a registration object, not whether
    the configured process is runnable. Missing registration is an advisory as
    recorded above. The old lock, owner, schema, and recall checks still hold.
    Before the cycle-1 parser fix, struct decoding also accepted `MCP`/`Mcp`
    as the root despite the exact-path claim. After it, map lookups require
    the exact spelling of every path segment; unknown keys, including case
    variants beside a valid `mcp`, remain ignored. This correction binds this
    doctor reader alone, not every JSON reader in the repository.
  - `cmd/memdolt/root.go` adds `opencode` and retains every existing command.
    New `cmd/memdolt/opencode.go` verifies before `openCommandStore`, reuses its
    direct/authenticated-owner selection and current-schema gate, and reports
    store-close errors. New `internal/opencode/session.go` owns ID validation,
    argument-based invocation, Windows not-found-only fallback, typed response
    parsing, exact-ID comparison, and nullable field mapping. Those API checks
    bind `VerifySession` and the two commands that use it, not other CLI or MCP
    writes. They prove an API match, not host-context origin. The wrap-up actor
    is always `agent:opencode` with raw `cli`, never derived from API fields.
    Before the cycle-1 parser fix, struct decoding matched protocol keys
    case-insensitively: `data.ID` could override a mismatched actual `data.id`,
    and `DATA.ID` could satisfy missing lowercase members. After it,
    `VerifySession` uses exact lookups for `data`, `id`, `agent`, `model`,
    `providerID`, and `variant`, then decodes the selected strings with the
    existing type/null rules. Unknown fields, including differently cased
    aliases, stay ignored and cannot replace identity or metadata. The
    pre-store-open refusal applies to both CLI callers of `VerifySession`;
    unrelated JSON decoders, actor rules, and note writes remain unchanged.
  - `internal/store/schema.go` appends migration 4's five nullable note columns
    without editing migrations 1–3, their tags, or the migration runner.
    Existing rows retain their text, actor, raw actor, and timestamp with NULL
    metadata; repeated initialization still adds no migration or commit.
  - `internal/memory/memory.go` adds `NoteProvenance`, exposes optional metadata
    on `Note`, and extends `noteStatement` and `Notes` to write/read the five
    fields using bound SQL and the existing NULL/empty-string convention.
    `LogNote` delegates to `LogNoteWithProvenance` with empty metadata; its
    body normalization, actor, and one-note/one-commit behavior still hold.
    `PrepareNote` still prepares ordinary notes in memory. `CommitNotes` still
    validates the actor group and requires a clean working set, with the same
    batch message and author, and now carries optional metadata. Before this
    change these two commit paths scanned only text and raw actor; after it,
    all five provenance strings also enter `CommitRequest.Text`, so metadata
    can refuse an otherwise allowed note. `NoteProvenance.validate` rejects
    invalid UTF-8 and values over 255 characters without trimming metadata.
    Width checks and scan declarations bind `LogNoteWithProvenance` (including
    `LogNote`) and `CommitNotes`, not arbitrary SQL writers or every string
    column. These generic lanes do not independently verify API identity.
  - The existing `OwnerStore.Commit`/query transport carries the new columns,
    NULL arguments, and scan text without a new IPC operation. Authentication,
    single-submit/no-retry writes, and owner authority still hold. The shared
    `memory.Note` adds optional provenance fields to CLI JSON and the MCP note
    output schema; `log_session_note` accepts the same text-only input and its
    per-actor timer, retry/discard rules and shutdown ordering are unchanged.
    The sixteen registered tools and elicited review retain their contracts.
  - Nine new `templates/skills` files provide check-init, recall, and wrap-up
    for Claude Code, Codex, and OpenCode. They preserve human review of facts
    and decisions, distinguish queued MCP notes from durable CLI notes, and
    expose no deferred operation. OpenCode's host-context origin obligation
    remains separate from the CLI's API-match guarantee, also stated in help
    and PRD §§6.1/11.4. No previous workflow template is removed or replaced.
  - `cmd/memdolt/doctor_test.go`, `host_templates_test.go`, `opencode_test.go`,
    `internal/opencode/session_test.go`, and `localdolt/migrate_test.go` cover
    the new paths, exact metadata, pre-open refusals, supported config shapes,
    upgrade preservation/idempotence, direct/owner writes, and single/batched
    metadata deny-list enforcement. Existing review callbacks and checks are
    retained. These are deterministic fixture checks, not real-host acceptance;
    that separate session is issue #117. No dependency or deferred backend is added.
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
  ordering and also covers memdolt proposal staging, reject, and expiry. It did
  not cover direct-lane commits at that delivery. After issue #127, direct
  commits and transfers also share it; reads, migrations and foreign Dolt
  sessions remain outside it. The issue #106 blast-radius record above states
  the external-mutation and deletion behavior.

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
