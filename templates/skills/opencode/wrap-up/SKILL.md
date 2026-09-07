---
name: wrap-up
description: Wrap up an OpenCode memdolt session with independently verified provenance.
compatibility: opencode
---

# Wrap up the session

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
   `list_proposals`. Draft only changes supported by this session's evidence:
   task additions or closures, verified commands actually run, fact or decision
   proposals, supersessions, and one concise session note. Keep verified host
   metadata out of the note text.
2. Show every draft grouped by kind. Wait for explicit per-item approval or a
   clear approval of the whole group. Rejected drafts are dropped.
3. Make the first durable write the approved session summary:
   `memdolt opencode wrap-up-note <current-session-id> "<summary>" --json`.
   Pass the ID and text as separate arguments. This re-verifies the exact
   Session.Info before opening the store, stores all available metadata, and
   attributes the note as canonical `agent:opencode` while retaining raw `cli`.
4. Apply approved task changes with `task_add` and `task_done`, and observed
   command outcomes with `record_command`.
5. Stage facts and decisions with `propose_fact`, `propose_decision`, or
   `propose_supersede`. Never promote them. Tell the user that a human reviews
   staged claims with `memdolt review`.
6. Call `render` (CLI equivalent: `memdolt render --json`) to refresh the local
   generated files from committed main. Report the source commit, written
   outputs, and recoverable backups. Rendering does not flush pending MCP
   notes or promote claims; the verified CLI summary in step 3 is already committed.

Stop on the first failure; on a partial render or unknown reply, inspect the
reported outputs/backups before retrying. Before issue #133, M3 had no wrap-up
render, sync, transcript, metrics, visualization, document, global, locate,
or repository-operation step. After it, step 6 adds only render; the other
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
