# Shared global memory

Before issue #146, the global replica and combined recall in PRD §10 were
design-only. The human CLI, global documents and combined CLI/MCP recall now
operate on a real Dolt replica. Global proposal acceptance remains separate:
existing global-target proposals stay unaccepted and retain their terminal
`memdolt review` remedy. This delivery does not establish physical two-machine
hub acceptance or complete the parity matrix.

## Layout and bootstrap

Each client uses its own home directory, resolved by the OS. Existing repository
layouts are unchanged. No machine defaults or known-project registry is written.

```text
~/.memdolt/
  models/                            existing verified ONNX model cache
  global/
    .memdolt/
      dolt/memory/                   ordinary durable Dolt database and history
      LOCK                          ordinary exclusive ownership lock
      server.pid / server.sock      owner artifacts if an owner was started here
      embeddings.sqlite             local derived global vectors
      config.toml                   optional replica-local policy for native use
```

The global wrapper takes the calling repository's `.memdolt/config.toml` policy,
including its deny-list, file permissions and `[global]` settings. Native remote
configuration remains inside the ordinary Dolt layout. The existing schema and
`meta` identity travel with the database; no new hub-only table, host identity,
local-root metadata or acceptance marker is added. Documents retain the existing
selected absolute `path` column: it is source metadata, not a portable file alias.

Initialize the calling repository with ordinary `memdolt init` first. For the
first global client:

```sh
memdolt global enable --dir <repository>
memdolt global init --dir <repository>
memdolt repo remote add origin https://hub.example/global --global --dir <repository> --user <sql-user>
memdolt push --global --dir <repository>
```

On another client, opt its repository in, then clone instead of initializing:

```sh
memdolt global enable --dir <repository>
memdolt global clone https://hub.example/global --dir <repository> --user <sql-user>
memdolt repo status --global --dir <repository>
memdolt pull --global --dir <repository>
memdolt index rebuild --global --dir <repository>
```

The URLs above are placeholders for the selected remotesapi database. Absolute
`file:///` remotes also work, including `file:///C:/...` on Windows. Existing URL,
credential, schema, deny-scan, conflict-resolution and no-force rules apply.
Passwords come only from `DOLT_REMOTE_PASSWORD` in the executing process.
`repo remote list`, `repo status --local`, `push`, `pull --resolve <file>` and
`index status` accept the same `--global --dir <repository>` flags. Initialization
and clone are explicit; ordinary global operations never create or migrate a
missing, corrupt or unsupported durable store.

## Policy and human operations

```toml
[global]
enabled = false
include_docs_in_default = false
```

These settings belong to each calling repository. `global disable` preserves all
data and other repositories' settings. `global status --json` reports the opt-in
and, when enabled, the actual replica status or its availability error. While
disabled it explicitly reports that the replica was not opened.

`fact add/verify/supersede/list` and `decision add/set-summary/supersede/list`
accept `--global`. Writes retain the trusted `user` CLI boundary; source labels
do not grant authority. Born-global fact add uses the existing live-key upsert,
retaining id/creation time and replacing its value/source/kind/evidence and
verification stamp. Decision title collisions retain every record and report
the colliding ids, including database-collation equivalents.

`fact promote <id-or-key> --global` and `decision promote <id> --global` capture
one committed repository row, then copy it with a fresh ULID. Source rows/history
stay intact. Source/evidence/kind/summary/alternatives, timestamps and supported
SQL NULL/empty distinctions survive. Existing live global fact keys refuse
promotion, including identical text; inspect them and use an explicit human
`fact add --global` if replacement is intended. Ambiguous keys require an id;
superseded rows require selecting their live replacement to avoid dangling
cross-scope links. Results and the real user-authored commit record the source
commit/id, without storing a host path.

`doc add/list/show/remove --global` uses the existing document operations within
the global database. Source checks protect both the calling repository's and
global replica's owner credentials and aliases. Reads/removals can select the
stored id on another client; they do not open a synced path. Unchanged hashes
add no commit; changed content retains the document id and replaces all chunks.
Each successful global add enables only its calling repository's default global
docs flag, even for an unchanged document first added by another repository.
Repository document first-ingestion/opt-out behavior remains unchanged. A late
configuration error preserves confirmed document progress and names the manual
`[global] include_docs_in_default` repair; inspect before retrying.

Before integration with issue #145, the global branch could still discard a
native document result on a late transaction error. After integration, observed
native hashes and document/chunk identities survive add/remove finalization
errors and cancellation. Add leaves config unfinalized and names the calling
repository's `[global]` repair even when the shared table was already populated.
An unobserved native result retains the unknown-outcome marker: inspect rows
and history, without replay or an inferred rollback. The result policy is the
shared document/native seam, not a new global transaction implementation.

## Recall, ownership and recovery

Both CLI and MCP use the same application boundary. Enabled recall captures one
committed snapshot per scope, preserving dirty/proposal exclusion; these two
commits are not claimed to be an atomic cross-database snapshot. Participating
writes serialize during each capture and changed foreign main refuses capture.
`scope` and `snapshotCommit` identify each result; optional `lastChanged` keeps
the matching scope's actual blame. Equal ids/keys survive in one candidate pool
with deterministic scope-aware ties, repository first on an otherwise equal tie.

The active repository's mode, limits, weights, accepted-only/stale policy,
superseded/age penalties and rerank floors govern both scopes. Repository
`[retrieval] include_docs_in_default` and `[global] include_docs_in_default` apply
independently. Default docs still require reranking; explicit doc filters can
select both scopes. Query inference runs once and the combined candidate pool
undergoes one rerank pass. Global tasks, notes, narratives, archives and code are
excluded. Disabled-global recall uses the prior repository path and omits the
new scope metadata, preserving output apart from timing/observability.

Global vectors use the existing verified model and current-source-hash checks.
They remain in separate local SQLite, outside Dolt and sync. Missing/stale vectors
emit a scoped warning and the appropriate `index rebuild --global` remedy; only
matching stale rows use lexical fallback. A missing replica or competing owner
is a visible error, never an implicit empty corpus or automatic initialization.

Global operations hold the established exclusive replica lock. A simultaneous
CLI or another repository's MCP owner refuses before opening a competing engine.
They do not route global work through another repository's policy. Source-record
and recall capture can use the repository's authenticated owner via two explicit
read operations. Failed authentication or lost replies do not fall back/replay.
No global promotion write or new accepting tool is registered over IPC/MCP.

Stop the active operation/owner before retrying a lock refusal. Inspect lists,
local/remote status and reported hashes after any unknown/late outcome; retain
confirmed progress. Foreign filesystem/Dolt writers do not share this mutex, and
path checks retain the existing final-check/open interval limitations. Nothing
here adds a filesystem compare-and-swap or distributed transaction.
