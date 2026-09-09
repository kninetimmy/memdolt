# Catch up existing Memdolt memory

Use this procedure when resuming an existing repository from its selected
configured Dolt remote. It uses shipped CLI/MCP operations; there is no
`memdolt catch-up` CLI command. Read the procedure before acting. Honor the
user's existing authorization for this target and catch-up; do not ask again
for already approved fetch, pull or local refresh steps. Resolve missing
authority, target, remote or authorization before the affected operation.

## Select and inspect

Before #163 `[repo]` was ignored and identity was not enforced. After it,
inspect repository TOML and the committed identity in local repo status as
well as native remote entries. A TOML remote_url supplies default origin when
absent from the native list; conflicting defaults refuse. Local topology keeps
ordinary status offline, but an explicit remote requests remote inspection.
Live is unsupported. Existing unidentified stores require a separately approved
terminal `init --adopt-identity` with the owner stopped; never reassign identity
or use init to bypass a mismatch. Optional startup pull runs once before owner
publication; it does not prove freshness for this later catch-up or authorize
retrying an unknown result. Global identity and transfers remain separate.

1. Resolve the intended project's absolute root from the request and repository
   instructions. Establish its authoritative memory system. If memhub remains
   authoritative, preserve it and require an explicit Memdolt adoption/rehearsal
   scope before writing here; do not run both systems' catch-up workflows.
   Inspect `memdolt version` and the selected host binding. A missing workflow
   or CLI operation is an installation/version mismatch, not permission to
   install, upgrade, initialize or migrate memory.
2. Pass the absolute root as a separate `--dir <repository>` argument on every
   store CLI call, even from another cwd. For MCP, verify the intended server's
   live `status.dataDir` equals `<repository>/.memdolt/dolt`; the database is
   its `memory` child. A server name alone does not establish the target.
   Use argument-based process calls and structured tool inputs. Never build
   shell commands from paths, remote values, resolution text or credentials.
3. Inspect `memdolt repo status --local --dir <repository> --json` and
   `memdolt repo remote list --dir <repository> --json`. These stay offline.
   Record main/schema, clean/dirty changes, pending repo/global proposal counts
   and configured remote names/URLs/users. This is Dolt's working set, distinct
   from source-code Git changes. A missing, corrupt, newer, dirty, locked or
   otherwise refused store stops dependent work: report the actual error/remedy
   and let the operator finish local work or deliberately repair/upgrade it.
   Do not reset, reinit, remove locks, discard changes or bypass the owner with
   native Dolt. An existing owner handles supported CLI calls through verified
   authenticated IPC; no owner means direct access. A second MCP server is
   another competing owner, not a proxy.
4. Select the configured remote already named in the request/context; resolve
   any ambiguity before contact. Use that exact name throughout, not a URL,
   branch or refspec. Do not silently fall back to `origin` after a refusal or
   add/change remotes. With no remote or an offline-only request, report that
   remote freshness is unverified. Local context refresh can proceed only if
   requested and local inspection succeeded; it is not completed reconciliation.

Passwords come only from `DOLT_REMOTE_PASSWORD` in the process executing the
transfer. A live owner needs its own environment; the CLI never forwards its
password over IPC. Report the credential/connectivity remedy without reading
or printing secrets. A deliberate owner restart to change its environment is
separate from catch-up; do not force a restart or switch to a bypass connection.

## Fetch, assess and reconcile

Remote-aware status **fetches remote main**, even without `--diff`; it needs
transfer intent too. State the selected repository/remote and the authorized
fetch → pull if needed → local refresh effects, including render's note flush
below. Then use `memdolt repo status <remote> --diff --dir <repository> --json`
or the verified MCP `repo_status` with `remote` and `diff: true`. MCP
`repo_status` with `local: true` is the offline counterpart; do not combine
local mode with remote, user or diff. Inspect the captured hashes and complete
committed local-to-remote diff, whose direction is not a proposed overwrite.

| Observed status | Next step |
| --- | --- |
| `offline` / `no-remote` | No remote comparison. Report that limit; do not infer current. An explicitly named missing remote refuses rather than reporting no-remote. |
| `current` | Captured heads match; no pull needed. Continue local refresh. |
| `ahead` | Local history contains remote main; no incoming work. Continue local refresh and report unpublished local commits, without pushing. |
| `behind` | Run the authorized pull; expect a validated fast-forward, then inspect its actual result. |
| `diverged-mergeable` | Run the authorized pull; compatible divergence makes an attributed two-parent merge. Status preview itself rolls back and commits nothing. |
| `conflicted` | Run the authorized pull to obtain full conflict rows/blame, then use actual human choices below. |
| `diverged-unassessed`, `refused`, errors or unrecognized results | Stop reconciliation, report the assessment/remedy and inspect before continuing. Dirty divergence is not evidence of mergeability. |

Pull with `memdolt pull <remote> --dir <repository> --json`, or MCP `repo_pull`
with the same `remote`. Use the configured SQL user unless an explicit user
override was selected; keep that override consistent on status/pull/resolution.
CLI transfers have no `--actor` flag. An authorized merge of existing history
is not proposal acceptance. Pull validates schema and scans incoming changed
text/provenance; a status preview does not guarantee pull will pass. Captured
heads can change between calls, so inspect pull's own result. A successful
`changed` or `current` result permits refresh; a conflicted CLI result has JSON
plus a nonzero exit and is not success. MCP results can carry `isError`, an
embedded `result.error`, or pending forms/cursors; never treat those as a
completed merge. Fetched objects/tracking refs can remain after refusal.

For conflicts, show every displayed base/ours/theirs row and real commit/blame
provenance to the human. Catch-up authorization does not choose winners.
Never synthesize confirmation or fill human form responses on their behalf.

- **CLI:** capture pull's exact `localCommit`, `remoteCommit` and conflict IDs.
  Prepare a complete human-reviewed resolution object containing those two
  hashes and `choices`, then submit once with
  `memdolt pull <remote> --resolve <file> --dir <repository> --json`.
  `--resolve -` accepts the same JSON on stdin. Each data conflict needs
  `take: ours`, `theirs` or `manual`; manual `row` contains every writable
  column including nulls, omitting generated `facts.live_key`, and may not
  alter identity/provenance. Live-fact-key conflicts use `take: winner` or
  `manual` with the displayed `winner` row ID; losers are superseded, not
  deleted. Task done stays done unless the human chooses `reopen: true`.
  Follow the installed `pull --help` and returned remedy for unsupported
  schema/metadata/constraints, rather than fabricating a resolution.
- **MCP:** let the attributed host present real human forms. All choices stay
  pending until the complete validated merge commits as user. After nine modern
  forms, pass `nextCursor` back as `cursor` with the same remote/user within
  two minutes; it is single-use, not a commit or reusable approval. A genuine
  legacy client receives one complete form. Cancel/decline, expiry, restart or
  changed heads requires fresh review. Without supported forms/attribution,
  use the CLI human review path. Never invent request state or response tokens.

Neither path pushes, accepts pending proposals, performs direct fact/decision
writes, migrates memory or transfers the separate global replica. Only committed main
and its history, including committed documents, transfer. Pending proposal
branches and queued MCP notes are excluded. Models, vectors, code indexes,
rendered files and machine configuration stay local. Remote tags are not
fetched by status/pull; existing local tags stay intact.

## Refresh and read local context

1. After successful reconciliation (or an explicitly requested offline local
   refresh), run `memdolt index status --dir <repository> --json`. It compares
   committed memory sources with local vectors; it checks neither retrieval
   mode nor model artifacts. Inspect the existing `[retrieval]` configuration
   separately; a missing mode defaults to `fts`. Preserve the configuration.
2. If vectors are current, skip rebuild. For FTS-only work, missing vectors do
   not block text retrieval; report hybrid refresh as pending if relevant.
   Rebuild only when local vectors are wanted and authorized, and the required
   BGE embedding model, MiniLM reranker, both tokenizers and ONNX Runtime
   artifacts for the installed release are already present and verified.
   `index rebuild` lazily opens that engine when text needs embedding and can
   provision absent artifacts;
   `needsRebuild: true` is neither permission nor proof the artifacts exist.
   If availability/verification is uncertain, leave refresh pending and route
   model setup as a separate explicit choice. Do not trigger downloads as a
   check. When applicable, run
   `memdolt index rebuild --dir <repository> --json`, then inspect index status
   again. Do not run code indexing or `--global` refresh as part of this scope.
3. Explicitly render with `memdolt render --dir <repository> --json` or the
   verified MCP `render`. Explain that MCP and its live-owner CLI route flush
   that owner's queued notes before snapshotting committed main. Honor existing
   authorization covering that flush; if it excludes note commits, resolve
   that boundary before rendering. Standalone store render has no session queue
   and makes no memory commit. Pull itself never flushes notes or refreshes
   views. A note flush can leave local main ahead after reconciliation; it does
   not authorize a push. Read the returned source commit, `noteCommits`, written
   paths and backups. Failed flushing prevents file publication; the two output
   files are replaced separately, not as an atomic pair.
4. Read the reported `PROJECT.md` and `PROJECT_LEDGER.md` after successful
   rendering, plus `memdolt task list --dir <repository> --json` (MCP
   `list_tasks`) and `memdolt review list --dir <repository> --json` (MCP
   `list_proposals`). Inspect `state show` / `arch show` with the same target
   if context needs checking. Narratives are rendered context, not recall
   corpora. Do not reseed them, fabricate tasks/notes/command results or accept
   proposals to make catch-up appear complete. Report the immediate next work
   from the actual refreshed queue and context.

## Retain effects and stop on incomplete outcomes

Stop dependent operations after any failure, unknown reply or partial result.
Keep observed local/remote/resulting hashes, row IDs, `changed`, errors/remedies,
index outcomes, render `noteCommits` (note ID → confirmed hash), source commit,
written files and backups in the report. A captured remote hash alone does not
confirm promotion; report the operation's confirmed result. A later close,
output or render failure never negates an observed commit or published file.
Do not report catch-up complete while reconciliation or a required refresh is
unresolved; distinguish an intentionally skipped optional rebuild.

An unknown native result or lost owner reply is not rollback. Inspect the
selected local and remote main and affected rows/notes/history before any retry;
do not automatically resubmit the operation. Inspect partial render outputs
and backups before retrying. Confirmed note groups never replay; unknown groups
are inspection-only and block session rendering (end that session and inspect
before starting another). Only known uncommitted groups can retry on explicit
render or orderly shutdown, whose failures must still be reported. A crash can
lose process-local notes. Do not remove locks or restore backups automatically.

These authorization and sequencing obligations bind this host workflow. They
add no runtime enforcement to arbitrary CLI/MCP callers: `Toolset.Render`
flushes its own queue, while standalone `Store.Render` does not; transfer/status
coordination covers cooperating Memdolt operations, not foreign Dolt writers.
