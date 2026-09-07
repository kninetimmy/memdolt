# Memory interoperability and optional memhub migration

Issue #140 adds these CLI commands, all with `--dir` and `--json`:

```sh
memdolt export /existing/local/directory/memory.json --dir /source/repository --json
memdolt import /existing/local/directory/memory.json --dir /fresh/repository --json
memdolt import --from-memhub /existing/local/directory/export.json --dir /fresh/repository --json
```

Initialize the fresh destination explicitly with `memdolt init --dir ...`.
Import requires current schema, clean main without merge/conflict state,
no durable memory and no proposal branches (including cleanup residue).
Existing documents/chunks, config, derived indexes and rendered files remain.
Those derived views can become stale: refresh them explicitly after inspection.
There is no force-wipe, history rewrite, automatic initialization or migration.

## Two distinct formats

Native **memdolt interop v1** has `memdolt_export_version: 1`, integer
`source_schema_version: 4`, RFC3339 `exported_at`, captured `main_commit`,
`tables` and `pending_proposals`. It differs from numeric-ID memhub v1 and
cannot be reverse-imported by memhub. Neither JSON format is synchronization;
use Dolt history and the existing repository transfer commands for that.

`tables` contains `facts`, `decisions`, `tasks`, `commands`, `session_notes`,
`project_state`, `project_arch` and committed `proposals` metadata arrays.
Every row contains exactly its schema columns except generated `facts.live_key`.
SQL NULL is JSON `null`; every other cell is a string, including canonical
uppercase ULIDs, lossless decimal INTs and UTC `YYYY-MM-DD HH:MM:SS` DATETIME
values. NULL and empty strings differ. Evidence, alternatives, complete
narrative history, superseded rows, note text/provenance and command counters
survive. Existing column sizes and numeric ranges apply; no migration is added.

Each pending entry carries its `id`, captured `head`/`parent` hashes, complete
`metadata`, and `changes` containing a fixed `table` and complete nullable
`from`/`to` row images. Inserts have `from: null`. Supported shapes are an
ordinary repository fact insert, decision insert, fact overwrite, or
link-first/fresh-fact supersede. Main and all proposal heads are captured in
one read; subsequent rows/schema/diffs use those immutable hashes. Dirty data
and later branch changes cannot enter the export. Merged branch residue is
omitted; accepted metadata on main remains in `tables.proposals`.

Import recreates each supported payload as one ordinary unaccepted proposal.
Existing CLI review still owns promotion and all its guards. Multiple-commit
branches, unsupported data/schema changes, global pending targets and
before-images differing from exported main refuse before output/import writes.
An older conflicting base cannot be silently rebased: reconcile it in the
retained source and export again. Interop does not transport the original graph.

## Actual legacy-v1 fields and intentional dispositions

`--from-memhub` reads the tagged v0.2.0 `src/export/v1.rs` contract plus only
v0.2.2's five nullable note fields. Supported `memhub_export_version` is `1`;
declared source schemas `1`-`24` are supported. Older absent `session_notes`,
`project_state` and `project_arch` arrays default to empty. Absent optional
fact kind/superseder and decision summary stay NULL; absent decision source
uses v1's explicit `user` default. Present arrays must be arrays, not null.

Positive legacy integer IDs map consistently to new ULIDs on ID-bearing
tables, including supersession references and recreated pending rows.
Commands instead map to the actual `kind` primary key. `identity_map` reports
`table`, `source_id`, target `key` and `value`. Neither task schema has a
structured task-link column: task notes, including prose referencing old
numbers, remain byte-for-byte unchanged. Interpret those numbers through the
mapping; import never guesses or rewrites prose references. Notes retain exact
text, actor/raw actor and nullable `session_id`, `agent_id`, `provider_id`,
`model_id`, `variant`. This metadata is opaque provenance, never authority.

Legacy confidence is validated as finite and explicitly omitted because the
approved Dolt schema removed it. Native evidence/alternatives have no legacy
equivalent and start NULL. Timestamps retain their UTC instant; fractions
that cannot fit DATETIME seconds refuse rather than truncate. The exported
host root is not copied into any durable repository identity or imported row.

Only legacy pending facts and decisions are recreated. Actor, rationale and
creation time remain proposal metadata; the genesis annotation records each
recreated row's raw actor and exact `provenance_json`. A legacy decision payload
contains only title: its outer rationale is the decision rationale, just as in
memhub's accept implementation. Accepted/rejected/expired audit rows are
annotated counts, never replayed actions. Historical writes_log references may
name deleted or export-excluded rows; they are history, not new foreign keys.

Two model differences require pre-write refusal and explicit source review:

- memhub keys commands by `(kind, cmdline)`; memdolt allows one current row
  per `kind`. Duplicate kinds are not selected, merged or dropped. Reconcile
  them explicitly in a retained source copy, then export again.
- Legacy pending supersede names `target_kind: fact|decision` and existing
  `old`/`new` identifiers. It is not a fresh-fact replacement. Resolve pending
  supersessions through source human review before export. Pending global
  promotion/migration is also unsupported.

Before this replacement, memhub import offered force-wipe and removed target
writes_log even on a docs-only import, reporting that overwritten count.
After #140, memdolt offers neither destructive path. Changed main memory is
one real current-human-authored import commit. Legacy history becomes one
clearly labeled genesis note with SHA-256 source digest, counts, historical
pending-status counts and source metadata. No old authors/dates are forged as
Dolt commits. Recreated proposal commits also belong to the current human
importer at the actual import time. Previous initialization/doc history remains.

## File effects and retry

Import reads only its selected regular local file and never modifies it or
opens metadata pointers. Neither format carries documents/chunks, embeddings,
code index, config, ownership credentials, transcript archives/pointers or
repository identity. Re-ingest documents only from explicitly selected sources.

Export requires an existing local parent. Protected `.git`, `.memdolt`,
`.memhub`, `.orchestrator` paths, symlink/reparse traversal, opened credential
aliases and registered document-source destinations refuse. Existing output
must have a recognizable supported native bundle header. Shared renderer file
primitives prepare/sync a complete sibling, recheck old output, then replace
through `os.Root.Rename`. Preparation failure preserves the old output. There
is no export backup, multi-file transaction, filesystem compare-and-swap or
stronger directory-entry crash guarantee. The separate exclusive
`.<filename>.memdolt-export.lock` coordinates cooperating exports. After a
crash, stop exports and inspect output before removing its lock residue.

The direct or authenticated owner executes each complete operation once.
Its existing mutation mutex serializes participating writes; foreign Dolt
sessions remain outside it. Format, schema, identity, reference, destination
collation and persisted text/provenance checks precede writes. SQL values
stay bound. No import/export MCP mutation tool or new dependency is added.
Before decoding, the shared Unicode validator rejects invalid UTF-8 and
unpaired surrogate escapes, including nested legacy pending JSON. Format
struct members require their exact spelling; case aliases cannot overwrite
fields. Opaque provenance retains case-sensitive keys. Valid Unicode pairs,
literal escaped backslashes and replacement-character text remain unchanged.
Typed owner paths are checked before JSON marshaling can rewrite them, and
raw owner arguments use the existing shared pre-decode Unicode guard.

Each reached operation emits one JSON result with `--json`. `main_commit`
proves the main import; `created_proposals` is the exact confirmed prefix and
`remaining_proposals` the suffix without confirmed completion. `proposal_residue`
records a failed attempt's inspected branch/head, which may lack a complete
proposal; unconfirmed retained work reports unknown. `identity_map` is the complete planned
mapping: mapped pending rows exist only when their created proposal is confirmed.
`counts` describes the plan, not a claim that every proposal landed. Late
transaction, connection and reporting errors retain confirmed progress.
A lost reply or unconfirmed commit reports `unknown`; a missing returned hash
does not prove no write. Never auto-replay. Inspect `repo status --local`,
`review list`, rows/hashes and files first. A partial import is not an empty
destination: retain it and the original bundle, and select a fresh initialized
target for a reviewed new attempt.

## Optional adoption runbook

This delivery uses synthetic fixtures only. The user's own migration is
optional and unperformed. Preserve the sequence: converge memhub first;
lowest-stakes project first; inspect rows, references, nulls, review and real
history; run `memdolt index rebuild`, production recall and the unchanged
`memdolt eval retrieval` golden gate; soak one week. If choosing adoption,
quarantine the old `.memhub` by renaming rather than deleting, and retain old
state for a month. Select local configuration/index sources explicitly. Full
M5 parity, global migration and the physical hub gate remain separate.
