# Local code indexing and locating

Issue #138 implements the code-index subset of M5. It uses
`.memdolt/code_index.sqlite`, outside Dolt, memory recall, exports and sync.
It works in a Git repository without initializing memdolt memory or contacting
the Dolt owner. `memdolt index status/rebuild` still manages memory embeddings
in the separate `.memdolt/embeddings.sqlite`.

```sh
memdolt code status --dir <repository> --json
memdolt code index --dir <repository> --json
memdolt locate "where is a card charge authorized" --dir <repository> --json
memdolt locate "authorize a charge" --dir <repository> --no-refresh --limit 3
memdolt code rm --dir <repository> --json
memdolt eval locate --golden <matching-golden.json> --dir <repository> --json
memdolt eval locate --golden <matching-golden.json> --dir <repository> --rerank --min-rerank-score 0 --json
```

`code status` is read-only and never creates a missing directory or index.
Default locate lazily refreshes, while explicit `code index` pays the same
work up front. Refresh walks sorted/deduplicated `git ls-files -z`, preserving
path whitespace. It includes grammar-known source, excludes `.min.` filenames
and non-source files, and reports denied/skipped/binary files. No untracked
scratch is indexed. Source language inference follows the tagged final-dot
suffix rule, including its root-level extension-like filename edge cases.

The fast path compares millisecond mtime plus byte size. It opens a source to
check its type/identity but does not read unchanged content. If those metadata
values move, raw-byte SHA-256 distinguishes a touch from a real edit. HEAD is
reporting metadata only. Edits preserving both mtime and size can remain unseen;
there is no filesystem watcher or claim of a transaction across Git and source
files. Reads detecting a mid-read change skip/prune that file. Deleted, renamed,
unreadable, denied, linked and binary replacements lose their stale chunks and
vectors. The changed deny-rule hash forces content rechecking even when file
metadata is unchanged. Binary means invalid UTF-8 or NUL-containing source.

Refresh counters reconcile as
`new + changed + unchanged + skipped + excluded + denied = tracked`;
`binarySkipped` is a subset of new/changed, and `deletedFiles` counts prior
index rows removed. `committed` identifies a completed chunk transaction.
Chunk/FTS changes commit before inference; an embedding failure is visible and
leaves that complete text index, with missing/invalid vectors retried next time.
No failed partial vector batch is published.

`--no-refresh` is CLI-only. It makes no Git command and keeps old ranking,
line metadata and indexed HEAD. Snippets still come from current disk files,
so their text can differ from that old ranking. Current root, path, link,
owner-file and deny checks still run; a missing source gives an empty snippet
and warning, and other unsafe/unreadable sources refuse visibly. MCP and eval
always use lazy refresh. The existing `serve` command still owns its usual
Dolt startup/shutdown lifecycle; only the locator call bypasses that backend.
A locate response returns ranked path, start/end line,
nullable symbol, kind, fusion components, optional rerank score and at most six
lines/400 characters of snippet, including any ellipsis. It never returns the
stored whole body. This bound belongs to `Locate`/its public callers, not the
internal `ChunkFile` representation used for indexing.

All seven real AST grammars are pinned: Rust 0.24.2, C# 0.23.5, Java 0.23.5,
TypeScript 0.23.2 (with its TSX dialect), JavaScript 0.25.0 (including JSX),
Python 0.23.6 and Go 0.23.4. The official Go binding is
`v0.24.1-0.20251112183152-c9492002f76e`, supporting these ABI-15 grammars.
Each parser, tree and cursor is closed; the walker allocates no queries.
Language-load/native parse failures are errors. A successful parse with no
recognized items falls back to 50-line/4000-byte UTF-8-safe windows. Oversized
AST symbols stay intact. Top-level items, impl/type methods, nested types,
container headers with member bodies excised, Go receivers, JS declarators,
Python decorators/docstrings and language-specific module docs follow the
tagged rules. Normalization occurs before AST parsing too, avoiding Go comment
nodes that end between CR and LF leaving a stray carriage return.

Configuration deliberately preserves memdolt's existing semantics:

```toml
[retrieval]
mode = 'hybrid' # default is 'fts', as in tagged memhub
rerank_candidate_pool = 20

[code_index]
fts_weight = 0.5
vector_weight = 0.5
test_path_penalty = 0.90

[deny_list]
patterns = ['(^|/)private/.*$', '(^|/)[^/]*\.generated\.go$']
```

Code fusion is independent of `[retrieval.scoring]`, and ignores memory's
rerank toggle/floors and stale/superseded penalties. FTS5 BM25 gathers up to
100 matches using quoted AND terms and bound SQL; the vector side scans current
384-dimensional BGE rows. Negative BM25 is min/max normalized, with tied finite
scores receiving 1; cosine is clamped to [0,1]. The test penalty applies only
to top-level `tests/`, `benches/` and `examples/`. Ties use chunk ID. Optional
reranking uses the shared pool size (at least the result limit), preserves
fusion scores, and has no runtime floor. Model, dimension, text hash, vector
hash, byte length, finite components and nonzero norm all gate vector currency;
bad or missing vectors are counted and cannot contribute cosine scores.

Memhub's `[deny_list].patterns` used path globs. Memdolt already uses Go regexes
for memory text; that existing contract is unchanged. Code scans both path and
content with those regexes. For migration, glob `private/**` becomes
`(^|/)private/.*$`, and `*.generated.go` becomes
`(^|/)[^/]*\.generated\.go$` in a TOML literal string. This is not byte-compatible
memhub TOML. The tagged default secret-path exclusions (`.env`/`.env.*`, private-key
names/extensions, nested `secrets/`, `.aws/credentials`, `.gcloud/credentials*`
and `.gnupg/`) remain fixed code-index exclusions even with `patterns = []`;
their opt-out semantics therefore differ from memhub. Regex/config errors
fail closed. No second pattern setting or glob dependency was added.

All code source paths must remain within the canonical Git root. Symlinks,
Windows reparse traversal, absolute/traversing/stream paths, and protected
`.git`, `.memhub`, `.orchestrator` and `.memdolt` source components are refused.
The shared `layout.CheckOwnerSource` compares the opened source against this
repository's known `server.pid` before content reads, including no-refresh
snippets and managed configuration/header reads. It preserves document
ingestion's existing protection too. Absent owner metadata permits ordinary
local use; unverifiable metadata refuses. Hard-link or symlink aliases cannot
admit that credential. This protects known owner-file identity, not arbitrary
copies of secrets or every filesystem reader in the application.

An exclusive `.memdolt/code_index.lock` serializes cooperating refresh, query
and remove operations; contention is a visible refusal. Status uses a SQLite
read transaction and may fail visibly during a conflicting operation. The lock
is independent of the Dolt owner and renderer. After a crash, stop code-index
operations and inspect the index, lock and any journal residue before manually
removing residue. There is no automatic stale-lock/PID protocol.

The index uses DELETE journaling with FULL synchronous writes. This intentionally
differs from tagged memhub's WAL/NORMAL settings so status cannot create WAL or
shared-memory sidecars. Existing journal/WAL residue refuses instead of risking
recovery/removal of an unrelated companion file. Rooted handles and identity
checks guard managed paths; SQLite still opens a checked filesystem pathname.
The operation lock coordinates memdolt, not foreign filesystem/SQLite writers;
identity checks do not provide a filesystem compare-and-swap against changes in
the final check/open or check/remove interval. A recognized application ID is
required before opening an existing index for writes. Rebuild resets only known
derived schema; removal refuses linked files, hard-link aliases, unknown
occupants or unrelated schema. It never recursively deletes a directory.

The shared BGE/ms-marco pipeline still verifies every model/native artifact
before initialization, retains NFD compensation and releases native sessions.
Code's unsplit symbols exposed a prior missing 512-token limit: long inputs
previously reached ONNX with too many positions. `encodeSingle` and `encodePair`
now apply fastembed's right-side LongestFirst truncation while retaining BERT's
two/three special tokens. Every `Engine.Embed`, `EmbeddingTokenIDs`,
`RerankerTokenIDs` and `Engine.Rerank` caller gets that fixed model limit;
short inputs, memory scoring/config and artifact verification remain unchanged.
Truncation changes inference input only, never stored source/chunks/snippets.

The [frozen-reference notice](../tests/golden/testdata/locate/NOTICE.md) records
the unchanged golden files, correct benchmark/corpus pairing, tagged-corpus
drift, licenses, reproducible commands and measured quality. Both 18/18 Rust
and 17/17 polyglot fusion match gates pass; separate rerank floor-0 safety gates
reject both probes on each corpus. Default fusion leaks both, intentionally.
Protected Ubuntu CI runs the exact locator command alongside the unchanged
memory-retrieval/scale gate, reusing only verified cached models. This subset
does not implement Git-ingested history/`search file:`, global memory, public
memory commands, remaining M5 parity, or a full-M5 acceptance claim.
