---
name: wrap-up
description: Wrap up a Claude Code memdolt session through the current M3 tools.
framework: memdolt
---

# Wrap up the session

1. Read the current queue with `list_tasks` and staged claims with
   `list_proposals`. Draft only changes supported by this session's evidence:
   task additions or closures, verified commands actually run, fact or decision
   proposals, supersessions, and one concise session note.
2. Show every draft grouped by kind. Wait for explicit per-item approval or a
   clear approval of the whole group. Rejected drafts are dropped.
3. Apply approved task changes with `task_add` and `task_done`, and observed
   command outcomes with `record_command`.
4. Stage facts and decisions with `propose_fact`, `propose_decision`, or
   `propose_supersede`. Never promote them. Tell the user that a human reviews
   staged claims with `memdolt review`.
5. Record the approved summary with `log_session_note`.
   Report it as queued: its actor's batch commits at the five-minute deadline
   or orderly server shutdown; an abrupt exit can lose an uncommitted note.

Stop on the first tool failure. M3 has no wrap-up render, sync, transcript,
metrics, visualization, document, global, locate, or repository-operation step;
do not discover or emulate those deferred surfaces.
