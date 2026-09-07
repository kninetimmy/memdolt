---
name: wrap-up
description: Wrap up a Claude Code memdolt session with the implemented memory tools.
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
6. Call `render` (CLI equivalent: `memdolt render --json`) to refresh the local
   generated files from committed main. Report the source commit, written
   outputs, and recoverable backups. The queued summary above is excluded
   until its existing flush point; rendering does not flush it or promote claims.

Stop on the first tool failure; on a partial render or unknown reply, inspect
the reported outputs/backups before retrying. Before issue #133, M3 had no
wrap-up render, sync, transcript, metrics, visualization, document, global,
locate, or repository-operation step. After it, step 6 adds only render;
the other operations remain outside this template.
