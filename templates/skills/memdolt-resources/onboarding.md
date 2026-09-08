# Guided Memdolt onboarding

This is a host workflow over existing CLI/MCP operations, not a runtime wizard
or installer. Read it in full before setup. Approval applies to the concrete
choices below; do not repeatedly ask for an action already authorized.

## Inspect and select the target

1. Resolve the intended project's absolute root. Inspect repository instructions,
   README, build manifests, scripts, current Git changes and existing memory
   configuration. Inspect the installed `memdolt version`, PATH and host server
   bindings. Do not mistake the Memdolt source checkout for the target project.
2. Identify the authoritative memory system from repository instructions and the
   user's choice. If memhub is authoritative, preserve it: explicitly agree on
   Memdolt adoption or a disposable rehearsal before writing. Do not quietly
   replace instructions or send the same writes to both systems. Record an
   approved authority/target change in the repository's own instructions.
3. Inspect whether `.memdolt/` contains an initialized store. If no store exists,
   continue to the starting-memory choice without running store reads. For an
   existing initialized store, inspect source narratives with
   `memdolt state show --dir <repository> --json` and
   `memdolt arch show --dir <repository> --json`. For an initialized store, also
   inspect `repo status --local`, `review list`, `task list`, `doc ls`,
   `index status` and `global status`, each with `--dir <repository> --json`.
   A missing narrative is different from a failed store read. Preserve existing
   narratives, rows and pending proposals; do not reseed them. On a corrupt,
   newer, locked or otherwise refused store, report the actual error and stop
   dependent writes. Do not delete data or rerun init as a generic repair.
4. Inspect the chosen host's installed workflows and settings. Keep generic
   memhub skills and unrelated entries. Report old installed aliases
   (`check-init`, `recall`, `wrap-up`, `locate`, `eval-locate`, `global`)
   with their paths and apparent ownership for human review; do not delete or
   overwrite them. Use the eight `memdolt-*` entry points. The shared resources
   must be copied beside the host's commands/skills root as documented in the
   README; the target project need not contain this source checkout. Preserve
   user trust choices and let the host present its own approval prompts.

Use the selected absolute root as a separate `--dir` argument on CLI calls.
For MCP, select the intended server binding and verify its live `status` data
directory matches that root's `.memdolt/dolt` before using its tools (the
database itself is the `memory` child of this data directory).
A named server alone does not prove the target. Never build a shell command by
interpolating project text, paths, session IDs, remote URLs or credentials.
Use argument-based process APIs and stdin; shell examples below quote variables.

## Choose the starting memory

Present the applicable choice before any bootstrap write:

- **Fresh local memory:** only when no store or selected existing remote should
  be reused, run `memdolt init --dir <repository> --json`. Preserve the existing
  ignore file and add `.memdolt/` only as approved. Init creates/migrates the
  store; it does not interview, seed narratives or install host configuration.
- **Existing remote memory:** select the actual remote URL and SQL user, then
  use `memdolt clone <remote-url> --user <sql-user> --dir <repository> --json`
  instead of init. Inspect the cloned narratives and memory before proposing
  changes. Clone requires a destination without an existing memory database.
- **Existing local memory:** inspect and continue. An intentional schema upgrade
  is separate from seeding; `init` refuses while an MCP owner is running.
- **Optional migration:** offer a separately approved disposable rehearsal using
  the migration guide. Import requires a current, clean, empty initialized
  destination and no proposal branches. Do not migrate user memory as part of
  onboarding or overwrite the old system. JSON bundles are not the sync path.

Keep one MCP owner per local clone. Each `memdolt serve` opens the store
itself; a second host's server competes for ownership rather than proxying the
first. Hand off by stopping the old owner's session cleanly before starting
the new host's server. Supported CLI operations route through authenticated
local IPC while an owner is live; with no owner they open the store directly.
Init/migrations require stopping the owner. Do not remove locks or launch a
native Dolt writer to bypass an ownership refusal.

## Draft project context, then approve choices

Infer purpose, stack, build/test/run commands and constraints from the inspected
project. Ask only what remains missing or contradictory. For a genuinely new
project without a chosen stack, agree on it before describing one as established.
Do not record an inferred command as successfully run.

Present two separately reviewable drafts, showing changes against existing text:

- **State:** purpose, current progress, immediate next work, known blockers and
  relevant commands, distinguishing observed results from unexecuted commands.
- **Architecture:** current components, data flow, storage, boundaries and known
  constraints. Preserve established choices; uncertainty remains explicit.

Obtain separate approval for each changed narrative. Unchanged or rejected
narratives require no write. These direct lanes append versions to committed
main; they are not a way to promote a fact or decision. Stage any such claims
through `propose_fact`, `propose_decision` or `propose_supersede`; humans
review them. Never substitute human fact/decision commands or `--actor user`.

Also present explicit optional choices; preserving the existing configuration
is the default for an existing store:

| Choice | Existing operation and limits |
| --- | --- |
| Retrieval | Fresh default `fts` needs no model. Hybrid uses verified BGE-small-en-v1.5, MiniLM reranker, tokenizers and ONNX Runtime in `~/.memdolt/models/`. Explain the download/network and disk use before approval. Reuse or pre-position artifacts matching the committed manifest; mismatched hashes refuse. Run `index rebuild` for memory vectors, then `recall <query> --mode hybrid`. Merge approved `[retrieval]` changes into existing TOML; never replace other tables. |
| Code | Offer `code index` and a meaningful `locate <query>` for this project's Git-tracked source. The local index is independent of Dolt; it can work before memory init. Inspect the retrieval mode first: hybrid code indexing can also provision models. |
| Documents | Offer one explicitly selected Markdown source, inspect it for sensitive content, then use `doc add <path> --actor <host-actor>` or the repository MCP `doc_add`. Ingestion commits document text. First ingestion enables repository default docs, whose automatic inclusion requires hybrid reranking. Explicit `recall <query> --mode fts --source-type doc_chunk` also works; `doc ls/show` checks the selected content. |
| Global memory | Default is off per repository. Offer `global enable`, then an explicit `global init` or `global clone <remote-url>` only if the shared replica is missing. Inspect `global status` first. Existing shared memory is reused, not reseeded. Human global facts/decisions/docs are deliberate terminal operations; agents never impersonate the human. Global proposal acceptance still refuses, even through the terminal remedy. |
| Remote | Optional for solo local work. Select a configured endpoint and named remote before `repo remote add <name> <url> --user <sql-user>`; this saves configuration without contacting the remote. Clone/pull/status-with-remote/push need explicit transfer intent. Do not deploy a hub from this workflow. |

All table commands take the selected `--dir <repository>`; use `--json` for
structured evidence. CLI memory writers use the host actor from the entry point.
Do not turn an optional download, ingestion or transfer into an implicit side
effect of verification. If the installed CLI lacks an operation, report the
version mismatch instead of inventing a flag or silently upgrading it.

## Write and verify the approved setup

Use the existing `state set` and `arch set` lanes with explicit host attribution.
They accept multiline bodies on stdin when no body argument is given; there is
no `--from-file` flag. Feed the separately approved text through process stdin.
For example, after choosing the root/actor and preparing reviewed UTF-8 files,
the POSIX shell form is:

~~~sh
memdolt state set --dir "$memdolt_repo" --actor "$memdolt_actor" --json < "$approved_state_file"
memdolt arch set --dir "$memdolt_repo" --actor "$memdolt_actor" --json < "$approved_arch_file"
~~~

The PowerShell 7 form is:

~~~powershell
Get-Content -LiteralPath $approvedStateFile -Raw -Encoding utf8 | memdolt state set --dir $memdoltRepo --actor $memdoltActor --json
Get-Content -LiteralPath $approvedArchFile -Raw -Encoding utf8 | memdolt arch set --dir $memdoltRepo --actor $memdoltActor --json
~~~

These are invocation shapes, not permission to write both narratives. Run only
approved changed items, stop after a failure, and preserve returned IDs/hashes.
Read each back with `state show` / `arch show`, checking content and canonical
agent attribution. Execute approved build/test/run commands in the target root
using their real arguments; only then record the exact command and observed
exit status through `record_command` or
`command record <kind> <command-text> --exit <observed-status> --actor <host-actor>`.
Recording a command does not execute it. Never claim a check passed just because
it appears in a README or build script.

Apply the approved options, then `memdolt render --dir <repository> --json`.
Inspect the reported `PROJECT.md` / `PROJECT_LEDGER.md` outputs and source
commit. Narratives appear in rendered views but are not recall sources; notes
are also excluded from recall. Blank recall from narrative-only memory is
expected. Verify recall with actual approved facts, decisions, tasks or selected
docs only when present; do not add sample claims to make the check green.

Run `memdolt doctor --dir <repository> --json` and report its five scoped
checks: store lock, IPC, schema version, empty-recall rate and OpenCode
registration. It neither installs nor proves live compatibility across three
hosts. Separately reconnect the chosen host, confirm discovery of
`memdolt-init-project` and the selected workflows, verify the MCP tool list
(including `status`, `repo_status`, `doc_add` and `render`), and compare live
`status` with the selected target. Exercise relevant read calls. Report exactly
which live checks were possible; package tests and a green doctor result do
not prove that a live agent followed these instructions.

## Daily work, transfer and failure recovery

For an existing replica, use `memdolt-catch-up` and the
[shared catch-up procedure](catch-up.md) to reconcile its selected remote and
read refreshed local context. This relative resource link also resolves in
the installed sibling `memdolt-resources/` directory. Catch-up honors existing
transfer/refresh authorization and leaves model provisioning an explicit choice.

Use `memdolt-recall` / `memdolt-locate` during work and `memdolt-wrap-up` to
inspect and separately approve changed narratives before rendering. Its existing
task/command/proposal approvals remain. OpenCode wrap-up must verify the current
host-provided session ID before every later workflow write, and make its verified
approved summary the first durable write; onboarding does not create that note
or bypass the wrap-up identity checks.

Drive snapshot adoption becomes an explicitly selected Dolt remote workflow:
clone once, then authorized pull/merge → refresh local vectors and views → work
→ human review → wrap-up/render/flush → authorized push. Use
`repo remote list` and `repo status --local` to inspect offline; remote-aware
`repo status --diff` fetches and needs transfer authorization too.
`pull <remote> --json` reports actual conflict choices; provide a complete
human-reviewed `pull <remote> --resolve <file>` or the supported MCP human forms.
Never synthesize confirmation. Push publishes committed main without forcing;
it neither accepts proposals nor flushes notes. After pull/merge, run
`index status`, approved `index rebuild` if needed, and `render` explicitly.
Before #157, this guidance stated: "There is no Drive adopt step or new catch-up
workflow." After it, `memdolt-catch-up` supplies the shared host procedure above;
there is still no Drive adopt step or new catch-up runtime command.

Pending proposal branches and queued MCP notes do not transfer. Models, memory
vectors, code indexes, rendered files and per-machine configuration stay local.
Global memory has its own replica/remotes and explicit `--global` status,
pull/push and index refresh; repository transfers do not include it. Passwords
come only from `DOLT_REMOTE_PASSWORD` in the process executing the transfer.
A live owner needs its own credential environment; the CLI does not forward
its password. Keep secrets out of URLs, arguments, logs and committed settings.

Stop on a failure and report partial progress. A returned commit hash confirms
durability even when close/output/render later fails: inspect the corresponding
row, `state show` / `arch show`, notes and Dolt history; never replay a confirmed
write to fix a later step. An unknown native result or lost owner reply does
not prove rollback. Inspect the selected local/remote heads and row IDs before
any retry; do not automatically resubmit unknown operations. Session render
flushes its owner's queued notes before capturing committed main. Failed
flushing prevents publication; only known uncommitted groups retry on explicit
render or orderly shutdown. Unknown groups are inspection-only and confirmed
groups are never replayed. Retain `noteCommits`, source commit, written files
and backups even on failure. Inspect them before retrying a partial render;
a crash can lose uncommitted process-local notes.

Report completed choices, remaining optional work and exact verification
limits. Migration, a hub deployment, catch-up, audit/upgrade backends and live
multi-host/two-client acceptance are separate tasks.
