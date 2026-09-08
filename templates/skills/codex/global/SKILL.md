---
name: global
description: Inspect per-repository global memory enablement and the shared Dolt replica.
compatibility: codex
---

Start with `memdolt global status --dir <repository> --json`. Enablement is
per repository and defaults off. Run authorized enable/disable commands;
disabling preserves all data. Missing state needs explicit `global init` or
`global clone <remote-url>`, never implicit creation during recall.

Use `repo remote add/list`, `repo status`, `push`, `pull` and `index status/rebuild`
with `--global --dir <repository>` for the shared replica. Confirm the selected
remote and the user's transfer intent. Passwords stay in DOLT_REMOTE_PASSWORD
in the executing process. Report contention and partial/unknown results;
inspect before retrying and never automatically replay an uncertain operation.

Human-only commands are `fact add --global`, `decision add --global`, and
`fact/decision promote <id> --global`. Present their exact terminal commands
for the human; never impersonate approval with `--actor user`. Promotion copies
committed live records; existing global fact keys refuse and decision title
collisions retain both rows with a notice. Global proposal acceptance remains
a separate follow-up. Inspect `memdolt review`; never claim a global proposal
was accepted or bypass its terminal refusal.

Human global `doc add/list/show/remove --global` stays scoped. Each successful
add flips only this repository's [global] include_docs_in_default, even for an
unchanged shared document. Reference paths are metadata, not portable files.
The `recall` tool already merges enabled facts/decisions/docs in one pool and
one rerank pass. Report scope, snapshotCommit and freshness warnings. Repository
knobs and independent default-document settings remain authoritative; tasks,
notes, narratives, archives and code stay outside the global corpus.
