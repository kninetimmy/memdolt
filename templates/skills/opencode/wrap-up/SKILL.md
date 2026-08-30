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

Stop on the first failure. M3 has no wrap-up render, sync, transcript, metrics,
visualization, document, global, locate, or repository-operation step; do not
discover or emulate those deferred surfaces.
