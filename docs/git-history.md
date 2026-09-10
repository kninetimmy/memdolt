# Cached Git file history (issue #174)

Before #174, `search file:<path>` refused with the M5 Git-ingest remedy, and
the code index held source chunks/vectors only. After #174, explicit ingestion
adds committed Git metadata to that same derived SQLite cache. CLI and the
existing MCP `search` return cached exact-path history. Decision search keeps
its existing Dolt FULLTEXT ranking, fields and direct/authenticated-owner route.

```sh
memdolt ingest-git --dir <repository> --json
memdolt ingest-git --since <commit-ish> --dir <repository> --json
memdolt search 'file:src/main.go' --dir <repository> --limit 10 --json
memdolt search 'src/main.go' --dir <repository>
memdolt search 'decision:src/main.go' --dir <repository>
```

`--since` is a nonempty **revision**, not a date. Git resolves it to a commit
with option parsing terminated, then selects `since..captured-HEAD`: commits
reachable from that HEAD excluding commits reachable from since. Since need
not be an ancestor. Omitting it selects locally available ancestry of captured
HEAD. Empty/unborn/missing/invalid revision observations fail; a successfully
observed empty range, such as `HEAD..HEAD`, is a valid zero-count ingest.
The selected immutable IDs, shallow status and actual observation time appear
in the result. Moving HEAD afterward does not change the selected range.

This is an explicit local cache, never a claim of complete or current history.
Ingestion does not fetch; shallow ancestry and unavailable objects constrain
coverage. Missing objects or failed Git commands refuse, without treating them
as empty history. Replacement objects, grafts, caller Git environment overrides,
external diffs/textconv and lazy network fetching are excluded from this
observation path. The repository's locally available real commit objects remain
authoritative. No Git ref, source file, configuration, Dolt row or memory commit
is changed by ingestion.

## Metadata, paths and counts

Git's native `%an`, `%aI` and `%s` supply author name, **author date** and subject.
The tagged memhub field called `committed_at` actually used `%aI`; this port
names the field `authoredAt`. It is neither committer time nor ingest time.
Native `%s` is Git's subject presentation (first message paragraph joined by
spaces), not the whole commit message. Raw commit objects are length-framed and
validated before trusting NUL-framed pretty output. UTF-8, tabs, unit separators
and other supported delimiters survive exactly in those native metadata fields.
Invalid UTF-8, embedded NULs, malformed headers/identities/dates and declared
non-UTF-8 encodings refuse the whole observation instead of being transcoded,
truncated or echoed in diagnostics.

Name-status records use `-z`, preserving literal UTF-8 and whitespace instead
of storing Git's quoted escape spellings. Paths never pass through a shell or
SQL interpolation. Git paths are never trimmed or rewritten. Unsupported
absolute/traversing/backslash/colon/NUL/protected-namespace paths are excluded;
invalid UTF-8 in Git's path observation refuses the range. An OS path whose
protection cannot be inspected refuses visibly. The parser supports framed
tabs/newlines/unit separators; whether such a path can pass filesystem identity
inspection also depends on the host OS. Unicode and spaced paths work on Windows
and Unix. Query backslashes mean Windows directory separators, as in the tag;
use `/` for portable queries. Query path whitespace is significant, unlike the
tag's trimming; whitespace before the `file:` prefix is ignored. Matching is
exact and case-sensitive, with no glob, substring, `./` cleanup, Unicode folding
or rename-following search.

Changes are `A`, `M`, `D`, `T`, `R` or `C`. Native rename/copy detection uses 50%
similarity and a 1000-file exhaustive-detection limit. Copies are detected from
Git's ordinary candidate set, without `--find-copies-harder`; they may otherwise
appear as additions. `R`/`C` attaches only the destination to the commit, and
checks both source and destination paths for denial. Old-name history stops at
its previous records; a rename does not create a deletion record or transfer
older history to the new name. Merges compare the first parent, so one path has
at most one record per commit. Root commits compare an empty tree; empty commits
are retained with no file links.

`commitsSeen` counts selected observed commits, `commitsIndexed` counts those
whose stored metadata passes the deny rules, and `deniedCommits` is their
difference. `uniqueFilesSeen` counts distinct allowed destination paths in this
range; `commitFileLinksSeen` counts their allowed commit/path pairs.
`deniedFilesSkipped` counts excluded change records, including every change of
a denied commit. These are per-invocation observations, not newly inserted rows
or cumulative totals. Primary keys make repeated and overlapping ranges
idempotent. A repeated range updates its observation timestamp without adding
another coverage record.

Ingestion reads at most 100000 selected commits and 64 MiB per Git response.
Exceeding either bound fails with a smaller-range remedy, never a successful
truncated ingest. All observations, parsing and protection checks finish before
publishing any Git history. One SQLite transaction then publishes commits,
paths, links and range metadata together. `committed=true` means transaction
finalization was observed successfully; a later close/output error still fails
and retains that result. Failed/unobserved transaction finalization reports
`outcomeUnknown=true`, never a successful ingest or a rollback assertion.
Bootstrap may already have created/upgraded the empty derived schema at that
point; a refused observation cannot publish partial history.

## Search and protection boundaries

File results use `matcher: "exact:file-history"` and a `results` array with
`type: "file_history"`, `path`, `commitSha`, `author`, `authoredAt`, `subject` and
`changeType`. Ordering is parsed author seconds descending, then full commit ID
ascending for equal instants, including equivalent times with different UTC
offsets. Human output quotes path/author/subject so control characters cannot
forge extra report lines; JSON retains the exact values through JSON escaping.
Decision results retain their previous fields and have no `coverage` field.

File responses add `coverage`: `cached=true`, distinct ingested `ranges`, the
`lastIngest` range (or null), effective `limit`, `truncated`, and `deniedResults`
encountered during this query. Results combine all previously ingested ranges;
the last range alone is **not** a complete description of their union. Neither
this metadata nor search observes current Git HEAD. The default limit is ten;
file results and inspected candidate rows are capped at 200000. `truncated=true`
means more results/candidates exist beyond a bound. Denied counts cover only
inspected candidates. A missing cache/history yields an honest empty array.

Explicit decision prefixes take priority. Other path-looking queries (containing
`/`, `\` or `.`) select file history only if the exact path exists in cached
`git_files` or locator `indexed_files`; otherwise they keep decision fallback.
A locator-only path therefore returns empty file history. Explicit file queries
need not already be indexed. File selection uses the configured code repository
root; MCP refuses an unavailable root instead of guessing the process cwd.

These calls read only cache metadata and current path identities. They do not
run Git, read current source bodies, provision a model, refresh/ingest history,
open Dolt or contact its owner. The ordinary `serve` startup still owns Dolt;
only its file-search handler bypasses that backend. Ingestion/search neither
consume nor flush the session's queued notes. The existing MCP input remains
`query` plus optional `limit`; no tool name or ingestion tool is added. Its
output schema describes the two result branches, preserving decision fields.

The existing rooted directory, opened-file identity, application-ID,
regular/single-link SQLite and journal checks are reused. Current file identities
are inspected without content reads; deleted paths remain searchable. Linked
paths, default secret paths, protected metadata namespaces and known owner-file
aliases are excluded. The configured regexes scan stored commit ID, author,
author date, subject, change type and paths. Denied commits/changes are counted
without printing their contents. Queries recheck current configuration and
filter previously cached denied metadata; a denied query path returns a generic
error without echoing it. This does not erase earlier cache rows or scan whole
source bodies/commit-message bodies. Remove/reingest explicitly if the cache
itself needs replacement under new rules.

These policies bind `IngestGit`, `ReadFileHistory` and their named helpers/callers,
not arbitrary SQLite/Git users or every filesystem reader. `CheckOwnerSource`
still protects the selected repository's known owner identity, not arbitrary
secret copies. Unverifiable required protection refuses. Configuration is the
checked snapshot read at operation entry. The existing operation lock spans
cooperating ingest/query/refresh/remove calls; busy/crash residue refuses with
inspection required. Foreign filesystem/SQLite writers are outside that lock;
the existing final check/open and check/remove intervals remain non-atomic.

## Derived schema and complete change inventory

Before #174, schema v1 bootstrap reset recognized incompatible schemas by
dropping source tables. After it, v1 upgrades transactionally to v2 by adding
`git_commits`, `git_files`, `git_commit_files` and `git_ingestions` only. Every
source/chunk/vector ID and value survives. Unknown, malformed, unsupported or
foreign schema refuses without replacement. Known declared objects now have
their SQL definitions checked; generated FTS shadow objects retain name/type
checks. Existing v1 source reads remain usable without migration; v1 file-history
reads are empty. Ordinary locator refresh may prune absent source rows but never
historical paths. `code rm` explicitly removes the entire recognized v1/v2 cache,
including all history; a subsequent `code index` creates source data only, and
history needs another explicit `ingest-git`. Unsupported caches need inspection
with a compatible binary. There is no durable Dolt migration.

Before #174's first review correction, `checkOwnedSchema` used
`name NOT LIKE 'sqlite_%'`. SQL LIKE treats `_` as a wildcard, so foreign
`sqlitex_unrelated` tables and `sqlitex_side_effect` triggers escaped the
inventory. A native Git/SQLite reproduction erased source chunks and vectors
while ingestion reported `committed=true`. After correction,
`name NOT GLOB 'sqlite_*'` excludes only the literal reserved `sqlite_` prefix;
lookalike names reach the existing foreign-object refusal before use. This
shared fix covers `IngestGit`, `ReadFileHistory`, `Remove` and `bootstrap` through
`Refresh`/refreshing `Locate`. `Status` and no-refresh `Locate` retain their
existing read paths; this is not a new ownership check on every SQLite reader.
Real SQLite internal indexes/statistics, FTS shadow handling and the preserving
v1/v2 transition remain. New native
`TestGitHistoryRejectsForeignSQLitePrefixLookalikes` exercises foreign tables
and side-effect triggers in both versions, including all five public callers,
and checks exact source/vector rows plus byte-for-byte complete-cache retention.

| Element | Before and after / preserved boundary |
| --- | --- |
| `internal/codeindex/git.go`: `GitRange`, `GitIngestSummary`, `gitCommit`, `gitChange`, `maxGitCommits`, `maxGitBytes` | New range, observed counters, private parsed metadata and bounded observation records; no durable Store DTO changes. |
| `IngestGit` | New guarded local writer: preflight cache, capture/parse, deny/protect paths, bootstrap, publish. Existing root/config/owner/lock helpers are reused. No memory, source-body or model writer. |
| `loadGitHistory`, `gitRevision`, `gitOID`, `gitOutput`, `gitBuffer`, `gitBuffer.Write` | New immutable selection and bounded argument-based processes, sanitized Git environment, local-only observations and redacted failures. Existing `trackedFiles`/`currentHead` retain locator semantics; the latter's suppressed errors are never used as ingest authority. |
| `validateGitObjects`, `validGitIdentity`, `parseGitMetadata`, `parseGitChanges` | New raw-length/NUL framing, UTF-8/identity/date/type validation and destination extraction. Native subject formatting remains Git's; no lossy decoding or quoted-path storage. These validations do not govern unrelated Git commands. |
| `publishGitHistory` | New bound upserts and one history/range transaction. Its private finalizer defaults to native `sql.Tx.Commit`; tests can exercise an unobserved return. It trusts the outer validated payload, and introduces no general SQLite write policy. |
| `internal/codeindex/git_history.go`: `FileHistoryHit`, `HistoryCoverage`, `FileHistory`, `MaxFileHistoryLimit`, `gitObservedFormat` | New exact history DTOs, disclosed query bound and fixed-width UTC cache-observation timestamps. Git author offsets remain unchanged in output. |
| `NormalizeHistoryPath`, `historyPathAllowed`, `ReadFileHistory`, `historyCoverage`, `fileHistoryRows` | New query normalization, metadata-only path protection, guarded snapshot, current deny filtering and author-instant ordering. No implicit refresh, Git process, model, source content or memory owner access. |
| `internal/codeindex/schema.go`: `schemaVersion`, `historyDDL`, `checkOwnedSchema`, `bootstrap`, `needsRebuild` | v2 extends v1 in place. Owned definitions/counts are validated; foreign name collisions and unsupported versions refuse. `Refresh` and refreshing `Locate` reach the new bootstrap; `Remove` reaches the stricter ownership check. `Status`/no-refresh `Locate` retain their existing read paths and source v1 compatibility; neither acquires every new ingestion check. Existing header/path/locking and rollback/meta helpers remain. |
| Reused `ResolveRoot`, `openRepository`, `verifyMetadata`, `acquire`, `close`, `validateSourcePath`, `openRegular`, `openSource`, `readConfig`, `defaultDenied`, `openDB`, `checkIndexFiles`, `storedVersion`, `rollback`, `layout.CheckOwnerSource`, `denylist.Compile`/`List.Check` | Their existing implementations and source/document/locator callers remain unchanged. Git operations now use their root/config/identity/lock/schema/deny boundaries. Historical paths may be absent; a successful current-file open closes without calling `readSource`. These are named helper guarantees, not a policy for every filesystem or SQLite operation. |
| `internal/search/search.go`: `Query`, `Response`, `Hit`, `Parse`, `TryFile`, `Run`, `Lines` | Before, Parse refused file queries and Response held only decision hits. Now path candidates route before memory access; Results carries either typed branch and optional coverage. Parse's UTF-8/NUL check binds all search requests. `DecisionHit` fields, `Store.SearchDecisions`, ranking and decision formatting stay intact. Run refuses misrouted explicit file requests; it remains the decision executor, not a cache reader. |
| `cmd/memdolt/ingest_git.go`: `newIngestGitCommand`; `root.go`: `newRootCommand` | New CLI ingestion command/flags/reporting registered alongside existing commands; no existing command registration removed. Errors preserve confirmed/unknown publication flags. |
| `cmd/memdolt/search.go`: `newSearchCommand`; `code.go`: `newCodeCommand` | File routing precedes `flags.runStore`; decision routing stays unchanged. Help discloses cached bounds, preserving refresh, whole-index removal and unsupported-schema inspection. Code execution/scoring is unchanged apart from reached schema checks. |
| `internal/mcpserver/tools.go`: `registerTools`, `searchOutputSchema`, `Toolset.search` | Existing search registration gains description/output union and early file routing. The SDK's existing jsonschema-go dependency derives both real branches instead of incorrectly requiring both at once. Inputs, other registrations, Backend/IPC interfaces, authentication and note queues remain. |
| `internal/codeindex/git_test.go` and `index_test.go` | New native Git/SQLite framing, ranges, repetition, copies/renames/deletes/merges, offsets/ties, denied data, malformed observations, v1 preservation/collision, owner aliases, busy/foreign and finalization checks. Existing unsupported-schema regression now asserts retention instead of automatic destructive reset; source/removal assertions remain. |
| `internal/search/search_test.go`, `cmd/memdolt/search_test.go`, `internal/mcpserver/tools_test.go` | Replace the former file-refusal assertion with real cached-empty behavior, preserving ordinary decision and all registration assertions; add prefix/path parsing checks. |
| `cmd/memdolt/git_history_test.go`, `internal/mcpserver/git_history_test.go` | New human/JSON, modern/legacy actual MCP, live-owner equivalence, missing root/cache, unchanged input/output fields, no-Dolt/model/Git/source-body query and queued-note/native-memory preservation evidence. |
| `README.md`, `AGENTS.md`, `CLAUDE.md`, this guide, `docs/code-locator.md`, `docs/prd/memdolt-prd.md`, `internal/mcpserver/instructions.md` | Record shipped ingestion/search, before/after refusal/schema policy, cached limits and precise reader/writer scope. Existing lifecycle, attribution and human review instructions remain. |

The test-symbol inventory is:

- `internal/codeindex/git_test.go`: new `nativeGit`, `commitGitFixture` and
  `gitTableRows` supply isolated native objects and exact row observations;
  `TestGitIngestRangeRepeatPreservesVectorsAndDeletedFileHistory`,
  `TestGitHistoryTiesCopiesMergesEmptyRangesAndCacheCoverage`,
  `TestGitHistoryDenialsInvalidRevisionsAndAtomicFailures`,
  `TestGitHistoryBusyForeignSchemaAndFinalizationControls`,
  `TestGitFramingPreservesDelimitersAndRefusesMalformedObservations` and
  `TestGitHistoryOwnerAliasesAndV1NameCollisionsRefuse` exercise the named new
  boundaries. `TestGitHistoryRejectsForeignSQLitePrefixLookalikes` adds the
  review-correction ownership/preservation regression above. Existing
  `TestIndexStatusRebuildRemovalAndForeignFilePreservation`
  retains its source/removal checks with the new unsupported-version refusal.
- `internal/search/search_test.go`: existing
  `TestParseDecisionFallbackPrefixesAndRefusals` replaces the deferred-file
  expectation; new `TestFileHistorySearchParsingPreservesPathsAndDecisionPriority`
  exercises explicit paths, delimiters and prefix priority.
- `cmd/memdolt/git_history_test.go`: new `gitHistoryCLIFixture`,
  `TestGitIngestCLIHumanJSONWithoutDoltOrOwner` and
  `TestFileHistoryCLIAndActualMCPLiveOwnerPreserveMemoryAndQueuedNotes` add real
  CLI/serve evidence. Existing
  `TestSearchCLIProvidesStableDecisionJSONAndClearRefusals` preserves decision
  assertions and replaces the file-refusal expectation with cached emptiness.
- `internal/mcpserver/git_history_test.go`: new
  `TestFileHistoryMCPUsesCacheWithoutDoltGitModelsOrSourceBodies` checks both
  transports and missing-root refusal. Existing
  `TestM3ToolsSchemasSuccessAndRefusals` preserves registration/input/decision
  assertions while accepting an actual empty file-cache response.

No dependency/version, durable Dolt schema, global history, MCP name/input,
embedding/scoring rule, frozen corpus or golden threshold changes. The unchanged
locator gate still requires full Rust 18/18 and polyglot 17/17 fusion matches;
its existing floor-0 rerank probes and default fusion leak assertions remain.

Before integration with landed #173, this branch inherited 22 MCP registrations
and had no native memory-history command/tool. After the normal merge of
`e2277668`, both history surfaces coexist and all 23 landed registrations remain.
The inherited `Store.History`, typed `OwnerStore.History` operation, CLI history
command and MCP `historyTool`/`Toolset.history` retain their exact native ancestry,
nullable images, warning/session isolation and queue-preservation contracts.
Git ingestion, file-search routing/output schemas and derived-cache safeguards
remain unchanged. Only the overlapping AGENTS.md insertion needed conflict
resolution; both issue records were retained. README's maturity row also records
Git ingestion's previous deferral and shipped status alongside native history.
The native-history implementation
and tests are landed base content, not new #174 changes. The final #174 inventory
above is measured against fetched `origin/main`, including that landed base.
