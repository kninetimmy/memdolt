---
name: wrap-up
description: Wrap up a Codex memdolt session with the implemented memory tools.
compatibility: codex
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
   Before issue #139 human fact/decision CLI commands were deferred; after it
   they exist for trusted terminal use. This agent workflow still uses the
   reviewed lane; never substitute direct add/verify/summary/supersede commands
   or `--actor user` for a proposal and human review.
5. Record the approved summary with `log_session_note`.
   Report it as queued: its actor's batch commits at the five-minute deadline
   or orderly server shutdown, and now also at the explicit render below;
   an abrupt exit can lose an uncommitted note.
6. Call `render` (CLI equivalent: `memdolt render --json`) to refresh the local
   generated files from committed main. Report the source commit, written
   outputs, and recoverable backups. Before issue #145 the queued summary was
   excluded until its deadline/shutdown flush. Now MCP render and its live-owner
   CLI route flush that owner's notes before the snapshot. Failed flushing
   prevents publication; proposals remain excluded.
   Before #145's review correction, later render failures could omit its flushed
   note effects. Now retain `noteCommits` (note ID to commit hash) on success or
   failure and inspect those notes and Dolt history before retrying; an empty
   SourceCommit does not negate the confirmed flush.

Before #145 late commit errors could discard confirmed results and retain a
committed note group for retry. Now retain each returned row id and commit hash
alongside its error; inspect note/task/command lists and Dolt history before
retrying. Confirmed groups are never replayed. Only known uncommitted groups
retry on explicit render or orderly shutdown; unknown groups are inspection-only.
Close reports earlier/final failures and discards remaining groups after its
bounded final attempt. A lost reply means unknown outcome, not rollback.

Stop on the first tool failure; on a partial render or unknown reply, inspect
the reported outputs/backups before retrying. Before issue #133, M3 had no
wrap-up render, sync, transcript, metrics, visualization, document, global,
locate, or repository-operation step. After it, step 6 adds only render;
the other operations remain outside this template.

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
notes and pending/global proposals remain excluded. Existing per-item approval
and render boundaries still hold; transfers do not flush notes or refresh views.
