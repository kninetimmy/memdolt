# Repository identity and topology

Before issue #163, native clone, remote configuration, status and transfers
worked, but `[repo]` TOML was ignored and `meta.project_id` was not populated.
After it, explicit local/clone policy and Git-derived identity apply to the
existing embedded store and its authenticated owner. There is no new database
format, dependency, schema migration, live-SQL backend or machine registry.

## Identity and adoption

`memdolt init --dir <repository> --json` initializes schema as before. For a
new store with a supported Git origin it also commits `meta.project_id` and
`meta.project_origin`, attributed to the terminal user. Init reports the
identity, safe hub database name and identity commit. Repeated initialization
adds no redundant commit. Native migrations and their historical tags remain.

Supported origins are HTTPS, `ssh://user@host/owner/repo`, and SCP-style
`user@host:owner/repo` (SSH usernames are optional). Host, path and scheme case,
trailing slashes and one trailing `.git` normalize as in tagged memhub v0.2.0
and v0.2.2. Nested owner paths are supported. Credentials in HTTPS, passwords
in SSH, explicit ports, URL escapes, queries, fragments, dot segments and local
filesystem origins refuse. Origin resolution reads the selected root's local
Git configuration offline; it does not resolve SSH aliases, Git URL rewrites
or repository redirects. It ignores inherited `GIT_*` process overrides.

The ID is memhub's ASCII repository slug (at most 32 characters), a hyphen,
and the first eight hexadecimal SHA-256 characters of `host/owner/repo`.
For example, `github.com/kninetimmy/memdolt` gives `memdolt-414c4f88` and the
SQL-safe hub name `proj_memdolt_414c4f88`. The full canonical origin is stored
alongside the short ID, so a short-hash collision cannot silently combine two
projects. This identity is a routing check, not authentication of a Git server.
The local database is still named `memory`; configuration supplies the actual
hub URL, including its database name. Memdolt does not create a hub database.

An absent `.git` or missing origin permits local use without shared identity.
An invalid Git configuration, missing Git executable in a Git repository,
multiple origin values, unsupported origin or process failure refuses; those
errors never become a path-derived identity or expose Git's output. Git origin
must belong to the explicitly selected root; an ancestor's repository is not
silently used. Removing origin does not erase a committed identity. Global
entry points never derive or accept repository Git identity.

For an existing unidentified store, inspect its Git origin and local memory,
stop the owner, then deliberately run:

```sh
memdolt init --adopt-identity --dir <repository> --json
```

Adoption requires current valid schema, clean main and no active native merge,
conflicts or constraint violations. It preserves pending proposal branches,
rows and existing history, adding only its identity commit. Reopen never adopts.
If identity already exists, changed origins, partial/malformed metadata and
collisions refuse reassignment. Restore the intended origin or select the
correct memory store; there is no force/reassign flag. An intentionally renamed
Git remote requires a separately reviewed native history repair, outside this
delivery. Inspect `meta` and Dolt history with the owner stopped before repair.

An explicit native fast-forward push may publish an adopted identity to a
previously unidentified remote. Existing nonempty remote identity must match;
partial/malformed identity refuses. Native ancestry still rejects unrelated or
independently advanced history. An unidentified local store cannot acquire an
identity by pull, including startup pull: adopt deliberately before reconciling
or clone the identified remote into a fresh destination. Native clone validates
source identity before copying main and validates the copied identity again
before acceptance. Failure retains the existing reported bootstrap artifacts.

## Routing and precedence

Machine-local `.memdolt/config.toml` accepts:

```toml
[repo]
topology = "clone"
remote_url = "http://100.64.0.10:8000/proj_memdolt_414c4f88"
auto_pull_on_session_start = false
```

This address is an illustrative private-network shape, not a deployed hub.
Use the actual approved endpoint. URL validation is the existing native remote
contract: explicit absolute HTTP(S) remotesapi or OS-absolute `file:///`, no
userinfo, query, fragment or password. Username stays in a named native remote
or an explicit `--user`; only the executing owner's `DOLT_REMOTE_PASSWORD`
supplies its password. No credentials travel in TOML or authenticated IPC.

| Selection | Behavior |
| --- | --- |
| No `[repo]` settings / empty topology | Existing native behavior: default origin, offline no-remote status if absent, no startup pull. |
| `local` | Embedded local memory; ordinary status is offline. Explicit push/pull still express transfer intent. Remote status needs an explicit name, including for `--diff` or `--user`. |
| `clone` | Same embedded memory and native clone/push/pull. Ordinary memory operations remain offline. Missing/unreachable remote does not affect ordinary local work unless startup pull is enabled. |
| `live` | Unsupported; refuse before opening or handing out a local store. No fallback. |
| `remote_url` without native origin | Supplies origin for default or explicitly named origin transfer/status. It does not create a native remote entry; `repo remote list` still lists native entries only. |
| `remote_url` and native origin | Validated URLs must match exactly. Ambiguity refuses before remote contact. Memdolt does not guess URL equivalence or rewrite either setting. |
| Another explicit native remote name | Uses that named endpoint, preserving its username and caller override. A missing name refuses. |
| `clone [remote-url]` | An omitted URL uses TOML. If both are supplied they must match exactly; clone retains its exclusive ownership and empty-destination requirements. |

Configure an initialized repository with its owner stopped:

```sh
memdolt repo configure --topology clone --remote-url <absolute-url> --dir <repository>
memdolt repo configure --auto-pull-on-session-start --dir <repository>
memdolt repo configure --auto-pull-on-session-start=false --topology local --dir <repository>
```

Only supplied keys change. An empty `--remote-url` clears that TOML default;
an empty `--topology` restores native default behavior. Other tables and values
survive semantically; TOML comments/formatting need not survive replacement.
The writer checks actual opened config identity, protected owner-file aliases
and linked/reparse managed paths, then uses the existing synced temporary-file
replacement with detected concurrent-edit refusal. There is no filesystem
compare-and-swap against foreign writers in the final check/rename interval.
Malformed TOML, unknown `[repo]` keys and invalid consumed values fail visibly.
Valid unrelated tables keep their own independent validation rules.

## Owner startup and recovery

Startup pull defaults off and requires explicit `clone`. `serve` calls existing
Pull once after ownership/schema checks and before publishing either MCP or
authenticated IPC. Conflict-free merges are authored `memdolt`, since startup
preceded any client identity or human choice. No choices, review acceptance,
model overrides or credentials are invented. Conflicts stop startup and name
the normal terminal review remedy. Missing/unreachable remotes, dirty state,
newer/incompatible schema, cancellation and uncertain/late results stop startup
with inspection instructions. Reopening a CLI client never runs startup pull.
All protocol stdout remains MCP; diagnostics use the existing error channel.

A confirmed config change or identity/migration/pull hash remains reported
through later finalization, close or output failure. Inspect the returned
hashes, local/remote main and `.memdolt/config.toml` before retrying. A missing
result never proves rollback, and startup never replays an uncertain operation.
Fetched objects/tracking refs and clone's failed artifacts may remain. Startup
does not render, rebuild indexes, flush session notes or push.

Restart the owner after deliberate topology changes. Its shared Store handle
also rechecks current repository configuration and Git identity on every
reached operation, including authenticated raw Commit and MCP methods, so a
changed `live` setting or different nonempty Git identity cannot use stale
startup policy. Git invocation is bounded to five seconds at this contextless
handle boundary; the resolver itself honors the supplied context. Combined
recall reuses its checked connection for per-source blame. Individual
LastChanged calls for selected recall hits and staging each imported proposal
remain separate checked Store operations; transfers/adoption additionally
inspect captured committed metadata. No caching framework or watcher is added.

## Structural inventory and preserved boundaries

- `localdolt/project_identity.go`: adds offline resolution, canonical identity,
  strict paired metadata validation and explicit initialization. It uses bound
  SQL, the existing proposal mutex, clean/schema/deny guards and native commit
  result helper. `InitializeIdentity` changes identity only when absent. These
  restrictions bind its callers, not arbitrary native SQL or caller-authored
  `Store.Commit` statements. Full-origin equality binds checked store opens,
  adoption and transfers; it is not a global registry or an authentication check.
- `localdolt/repo_config.go`: adds the independent `[repo]` reader, validated
  setter and one-invocation startup-pull adapter. The setter owns syntax/path
  policy; CLI configure also checks native-origin conflicts while owning the
  store. Transfer selection independently refuses conflicting configurations.
- `document_file.go`: extracts `replaceConfig` from the existing bool writer.
  Document/global callers retain their validation and bool semantics; all
  replacement callers retain temporary-file sync, semantic preservation,
  detected-edit refusal and confirmed `changed` reporting. Final reread shares
  `readConfigBytes`; byte comparison still detects every changed config.
- `localdolt.go`: `Config.Global` marks explicit global operations. `Open`
  now checks routing/identity before use; `handle` rechecks active policy.
  Engine ownership, schema refusal, direct/staged commit ordering and result
  semantics remain. Before #163 callers could ignore malformed TOML; after it
  all reached repository Store operations refuse it. NoText still skips deny
  regex evaluation; it no longer bypasses repository routing-file parsing.
  `Close` always remains available to release ownership after policy refusal.
- `global.go`: `OpenGlobal` supplies the explicit global flag. Its enabled
  calling-repository policy, checked global paths, exclusive lock, credentials,
  global recall and human/review write boundaries remain. This exclusion binds
  explicit global entry points, not every raw `New` pointed at a global path.
- `clone.go`: default URL selection plus source/copied identity checks; the
  native metadata iterator now returns schema and identity values. Existing
  main/history transfer, no source initialization, progress suppression,
  artifact retention, native origin/tracking and schema checks remain. Earlier
  missing-main refusal now occurs before copying main, retaining bootstrap.
- `transfer.go`, `transfer_engine.go`: reuse protected native remote refresh,
  select TOML/native defaults and compare captured local/incoming identities.
  Push compares existing remote metadata before attempting promotion; failures
  before the native push attempt are now `refused`, previously conservatively
  `unknown`. Unknown attempted pushes retain their existing inspection remedy.
  Native fast-forward, credential/redaction, changed-text scanning, pull merge
  choices, confirmed hashes and retained fetches remain. Native/foreign writers
  do not participate in the Store mutex; native ancestry guards still govern
  push, without new coordination against remote identity rewrites.
- `remote.go`: adding origin now rejects a conflicting TOML default; other
  names, sanitized listing, native persistence/read-back and result behavior
  remain. `repo_status.go` adds optional identity/topology output and local
  routing. Native preview/rollback, diffs, pending counts and no-promotion
  policy remain, with paired incoming identity required before assessment.
- `retrieval.go`, `recall_snapshot.go`: extract the same fixed-table, bound-ID
  blame reader so combined capture uses its existing checked connection for
  every source. Before #163 capture reopened a connection per blame row; after
  it, the one capture retains its mutex, committed readers, exact queries and
  final foreign-main refusal without a new Git process per corpus row. Public
  LastChanged still checks policy on each call; unsupported source types still
  refuse, now after the connection check. Retrieval scoring and output remain.
- CLI `init.go`, `clone.go`, `repo_config.go`, `repo.go`, `lanes.go`, `serve.go`,
  `remote.go`, `transfer.go`:
  expose adoption/configure/default clone URL, report confirmed effects, apply
  policy before owner routing and run one startup pull before publication.
  Existing root commands, schemas, authenticated one-submit/no-fallback routes,
  stdout protocol and shutdown order remain. Global init/clone select explicit
  global policy; remote/transfer help describes precedence and identity refusal.
  No new IPC operation or MCP tool/input is added; all 22 tool
  registrations, review gates, queues and provenance conventions remain.
- New native identity/config tests and CLI repository tests exercise direct,
  authenticated raw/typed owner and production MCP paths with disposable roots.
  Existing clone/repo/document fixtures update only ignored/malformed-config
  assumptions or earlier-refusal diagnostics. Native state checks still prove
  unchanged rows/history; frozen golden assertions and production protections
  remain. No live user home, credentials, stores, installation or hub is changed.
- README, AGENTS, CLAUDE, PRD, server instructions and shared onboarding/catch-up
  guidance add matching before/after records, precedence and recovery. Host
  registrations, generic workflows, human approvals and existing milestone
  evidence remain. This completes this identity/topology slice only: physical
  two-client/private-network acceptance and measured client version compatibility
  remain M4 work; optional live SQL and M5/M6 work remain separate.
