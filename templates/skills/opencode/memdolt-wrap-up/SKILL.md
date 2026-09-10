---
name: memdolt-wrap-up
description: Wrap up an OpenCode memdolt session with independently verified provenance.
compatibility: opencode
---

# Wrap up the session

Use the repository's explicitly selected authoritative memory system. Confirm
the absolute repository target and the bound MCP `status` data directory before
using its tools; pass that target as `--dir <repository>` on every store CLI call.
If authority or target is unclear, resolve it before writing.

## Identity pre-flight

Before any durable memory call, take the current session ID only from this
session's OpenCode host context. Never infer it from a session list, title,
active-session heuristic, transcript, or filesystem. It must be `ses` followed
by one or more ASCII letters, digits, underscores, or hyphens. If it is absent
or malformed, stop before every write, render, or sync.

Pass the validated ID as its own process argument to
`memdolt opencode session-info <current-session-id> --json`. The command uses
argument-based process execution to call
`opencode2 api get "/api/session/<current-session-id>"`, requires an exact
returned `data.id`, and reports `session_id` plus available `agent_id`,
`provider_id`, `model_id`, and `variant`. If the command or validation fails,
stop before every durable memory write, render, or sync.

The CLI verifies the supplied ID against the API; it cannot authenticate the
origin of that ID. Obtaining the current ID from host context is your workflow
obligation. Never discover or guess another session, even if its API lookup
would succeed.

## Draft, approve, and write

1. Read the current queue with `list_tasks` and staged claims with
   `list_proposals`. Inspect existing narratives with
   `memdolt state show --dir <repository> --json` and
   `memdolt arch show --dir <repository> --json`; distinguish an absent narrative
   from a failed read. Draft state/architecture changes against that text, not
   a reseed. Draft only changes supported by this session's evidence:
   task additions or closures, verified commands actually run, fact or decision
   proposals, supersessions, and one concise session note. Keep verified host
   metadata out of the note text.
2. Show every draft grouped by kind. Wait for explicit per-item approval or a
   clear approval of the whole group. Present state and architecture separately
   and obtain approval for each changed narrative. Unchanged or rejected
   narratives require no write. Rejected drafts are dropped.
3. Make the first durable write the approved session summary:
   `memdolt opencode wrap-up-note <current-session-id> "<summary>" --json --dir <repository>`.
   Pass the ID and text as separate arguments. This re-verifies the exact
   Session.Info before opening the store, stores all available metadata, and
   attributes the note as canonical `agent:opencode` while retaining raw `cli`.
   Before #171 its body came from an argument or stdin; now the same command
   accepts `--from-file <approved-file>` instead of the summary argument, with
   the file rules in step 6. Session verification and provenance stay unchanged;
   no caller-supplied provenance overrides are accepted.
4. Apply approved task changes with `task_add` and `task_done`, and observed
   command outcomes with `record_command`.
5. Stage facts and decisions with `propose_fact`, `propose_decision`, or
   `propose_supersede`. Never promote them. Tell the user that a human reviews
   staged claims with `memdolt review`.
   Before issue #139 human fact/decision CLI commands were deferred; after it
   they exist for trusted terminal use. This agent workflow still uses the
   reviewed lane; never substitute direct add/verify/summary/supersede commands
   or `--actor user` for a proposal and human review.
6. Write only approved changed narratives through
   `memdolt state set --dir <repository> --actor "opencode" --json` and
   `memdolt arch set --dir <repository> --actor "opencode" --json`, supplying
   each approved multiline body on stdin, with no body argument. Before #171
   there was no `--from-file` flag; now `--from-file <approved-file>` also works.
   It conflicts with any body argument, even empty; omitting both keeps stdin.
   Files resolve from process cwd independently of --dir and must be regular,
   nonblank UTF-8 within 65535 raw bytes. Opened-file/owner-alias protection and
   read/close validation finish before memory opens; external and rendered
   files are allowed. Existing stdin/argument normalization and limits remain.
   Use separate process arguments and stdin, not text
   interpolated into a shell command. These direct lanes append versions;
   they do not replace fact/decision proposals or authorize human promotion.
   Retain each ID/hash and read it back with `state show` / `arch show` before
   proceeding. On error, stop and inspect confirmed/unknown outcomes as below.
7. Call `render` (CLI equivalent: `memdolt render --dir <repository> --json`) to refresh the local
   generated files from committed main. Report the source commit, written
   outputs, and recoverable backups. Before issue #145 rendering did not flush
   pending MCP notes. Now MCP render and its live-owner CLI route flush that
   owner's notes before the snapshot; failed flushing prevents publication.
   Proposals remain excluded. The verified CLI summary in step 3 is already
   committed and is not queued or replayed by render.
   Before #145's review correction, later render failures could omit its flushed
   note effects. Now retain `noteCommits` (note ID to commit hash) on success or
   failure and inspect those notes and Dolt history before retrying; an empty
   SourceCommit does not negate the confirmed flush.

Before #155 these wrap-ups did not inspect or update state/architecture. Now
steps 1, 2 and 6 add separately approved narrative continuity before render;
unchanged/rejected narratives add no version. Existing approval, identity,
note flushing and transfer rules remain. Narratives appear in rendered views,
not recall results. A later render failure never authorizes replaying a
confirmed narrative write; inspect `state show` / `arch show` and Dolt history.

Before #145 late commit errors could discard confirmed results and retain a
committed note group for retry. Now retain each returned row id and commit hash
alongside its error; inspect note/task/command lists and Dolt history before
retrying. Confirmed groups are never replayed. Only known uncommitted groups
retry on explicit render or orderly shutdown; unknown groups are inspection-only.
Close reports earlier/final failures and discards remaining groups after its
bounded final attempt. A lost reply means unknown outcome, not rollback.

Stop on the first failure; on a partial render or unknown reply, inspect the
reported outputs/backups before retrying. Before issue #133, M3 had no wrap-up
render, sync, transcript, metrics, visualization, document, global, locate,
or repository-operation step. After it, the then-step 6 added only render; the other
operations remain outside this template. The identity pre-flight still stops
every later write and render on verification failure.

Before issue #137, the repository-transfer step was absent. After it, when
the user's request includes a repository transfer, use `repo_push` to publish
committed main. If reconciliation is needed, `repo_pull` merges compatible
committed history; actual conflicts require explicit human form choices over
every displayed row and blame. Never synthesize confirmation. All choices
remain pending until the complete validated merge commits as user. Follow
nextCursor after nine modern forms; a genuine legacy client receives one
complete form. Expiry, cancel, restart or changed heads requires fresh review;
without form support use `memdolt pull --json` and `memdolt pull --resolve <file>`.
Inspect local and remote main after an unknown reply before any retry. Queued
notes and pending/global proposals remain excluded. Existing per-item approval,
verified session identity and render boundaries still hold; transfers do not
flush notes or refresh views.
