# memdolt

Local-first CLI + MCP server (Go) giving coding agents durable per-repo
project memory stored in Dolt — a MySQL-compatible database with git
semantics.

**`docs/prd/memdolt-prd.md` is the product authority.** Read it before
proposing design changes; don't silently diverge from it.

## Session Continuity

memhub is the source of truth at `.memhub/project.sqlite`.
The rendered files under `.memhub/rendered/` are the local human-readable
view. They are generated from the DB and ignored by Git by default.
Re-render after `/wrap-up` with `memhub render`.

The PRD is ingested into memhub (54 chunks), so `recall` surfaces the
relevant sections directly — prefer it over re-reading the whole document.

## Build / test / run

Go module at the repo root: `github.com/kninetimmy/memdolt`, Go ≥1.26.2
(the minimum `github.com/dolthub/driver` requires).

**Two build settings are mandatory**, because the embedded Dolt driver
requires them:

- `CGO_ENABLED=1` and a working C compiler (gcc/clang; MinGW-w64 on
  Windows). Dolt's block store imports `github.com/dolthub/gozstd`, a cgo
  wrapper around zstd, unconditionally — there is no pure-Go build.
- The `gms_pure_go` build tag, which swaps go-mysql-server's ICU-backed
  `REGEXP` implementation for the standard library's. Without it the build
  also needs system ICU development headers, which are not available on all
  three supported platforms.

Set them once per shell (or with `go env -w`) so the commands below work
as written; CI sets them at the workflow level:

```sh
export CGO_ENABLED=1
export GOFLAGS=-tags=gms_pure_go
```

- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...`
- Format check: `gofmt -l .` (must print nothing; `gofmt -w .` fixes it)
- Lint: `golangci-lint run` (config in `.golangci.yml`; not vendored —
  install it separately, e.g. from
  https://golangci-lint.run/welcome/install/; CI pins the version it uses
  in `.github/workflows/ci.yml`)
- Run the CLI: `go run ./cmd/memdolt version` (add `--json` for a single
  machine-readable JSON object instead of the human-readable line)
- Create a store: `go run ./cmd/memdolt init` makes `.memdolt/dolt` beneath
  the current directory (`--dir` points it elsewhere) and applies every
  schema migration the store is missing, one Dolt commit each (PRD §6.1,
  §6.2, §6.4). It is idempotent: a second run adds nothing to the Dolt
  history.
- Check a repository: `go run ./cmd/memdolt doctor` reports the store
  lock's ownership state (held, an orphaned record, absent), whether a live
  owner answers on its IPC endpoint, and whether the store's schema is
  newer than the binary (PRD §5.2.4, §6.4). It creates nothing — run
  against a directory with no store it says so — and exits nonzero when a
  check fails. A condition memdolt clears by itself (a stale lock record,
  an orphaned pidfile) is a warning and exits zero.
- Write and read the direct lanes (PRD §3.1): `memdolt task add|done|block|list`,
  `memdolt note add|list`, `memdolt command record|get`, `memdolt state set|show`
  and `memdolt arch set|show`. Each write is one Dolt commit on `main`, authored
  by the actor (`--actor "Claude Code"` normalizes to `agent:claude-code`; the
  default is `user`), so `dolt_log` answers provenance on its own. `note add`
  and the two `set` commands read their body from stdin when given no argument.
- Review what agents staged (PRD §7, §11.2): `memdolt review list|show|accept|reject|expire|stale`.
  `show` renders a proposal as the single-commit diff of its branch; `accept` merges
  exactly the one commit that proposal was staged with into `main` under a `--no-ff`
  merge commit — authored by the reviewer and messaged `review accept <kind> <id>`,
  so `dolt_log` alone carries the whole propose-review-merge cycle — and then deletes
  the branch. `reject` deletes the branch and leaves `main` where it was, `expire`
  sweeps branches older than `--older-than`, and `stale` reports them without writing
  anything. The merge is fail-closed (PRD §6.3): a data conflict, a constraint
  violation that verification shows is real, or a row memdolt cannot attribute leaves
  `main` untouched and the proposal still pending.

  **That sentence used to read "merges exactly that branch".** It merged the branch
  by name and trusted it to hold one commit, which is true of every branch `stage()`
  produces and of nothing else: a branch carrying two commits was shown and
  deny-list-scanned as its head commit alone and merged whole. `accept` now counts
  what the branch carries past its merge base with `main` and refuses unless the
  count is one, then merges the staging commit *by hash* — so the commit review
  showed is the commit that merges. **The count binds `AcceptProposal` alone, not
  every review verb**: `show`, `reject`, `expire` and `stale` still take a proposal
  branch whatever is on it, and `show` renders its head commit against that commit's
  first parent — a partial view of a branch carrying more, which is how an operator
  sees what a branch holds before rejecting it. Refusing at the one verb that writes
  to `main` is the point, not an oversight at the others.
- Run the M0 rig-1 concurrency soak (PRD §16): `go test -tags
  soak,gms_pure_go ./tests/soak/...`

The soak lives behind the `soak` build tag, so `go test ./...` never starts
it — it runs real processes and kills one of them. Note that the mandatory
tag has to be repeated on that command line: a `-tags` there *replaces* the
one in `GOFLAGS` rather than adding to it, so `go test -tags soak
./tests/soak/...` silently drops `gms_pure_go` and fails inside
go-icu-regex. Duration and concurrency are flags (`-soak.duration`,
`-soak.owner-writers`, `-soak.client-processes`, …), and a long run needs
`-timeout` raised past the 10-minute default. Findings and measured
numbers: `docs/spikes/m0-rig1.md`.

## The deny-list (PRD §11.3)

`.memdolt/config.toml` is per-machine and optional. `[deny_list]` is the
only table with a reader so far:

```toml
[deny_list]
patterns = ['(?i)\bAKIA[0-9A-Z]{16}\b']
```

Patterns are Go regular expressions, written as TOML *literal* strings
(single quotes) because a regex's backslashes are not valid escapes in a
TOML basic string. They are matched against `store.CommitRequest.Text` —
the memory a write records in its own words — before the write's
transaction opens, so a refused write leaves no row and no commit. Every
failure refuses: an unreadable file, TOML that does not parse, a
`[deny_list]` table that decodes no `patterns` key and a pattern that does
not compile all refuse the write rather than let it through unscanned (PRD
§13.3 — memdolt keeps secrets out rather than promising to delete them).

**That list used to end at "a pattern that does not compile".** A
`[deny_list]` table whose key was misspelled — `pattern =`, `patterns_ =`,
or no keys at all — decoded to zero patterns, which was indistinguishable
from a repository that had configured none: the typo disabled enforcement
and nothing said so. `denylist.Load` now reads the table's presence as an
operator who meant to configure rules and refuses when no `patterns` key
decoded. An explicit `patterns = []` still loads as no deny-list, because
the key decoded and the empty list is a decision somebody wrote down.
**The refusal binds `Load`, not every entry point of its kind**:
`denylist.Compile` takes patterns a caller already holds, where zero of
them means zero and is no evidence of a typo, so `Compile(nil)` is still
no deny-list and no error. Nor does it see past a misspelled *table*
name: `[denylist]` is a config file with no `[deny_list]` in it and loads
as no deny-list, since refusing on any undecoded key would trip on the
rest of PRD §11.3's config, which has no reader yet.

**Every `CommitRequest` declares its text, or declares it has none.** One
that sets neither `Text` nor `NoText` is refused before anything is
applied, so a lane that writes through a `CommitRequest` cannot skip the
deny-list by leaving a field at its zero value — it fails loudly on its
first write instead. `NoText` is for commits that carry no prose anyone
wrote; the migration runner is the case it exists for, deliberately, so
that a deny-list config that cannot be evaluated never stands between an
operator and `memdolt init`. The declaration travels over IPC too, where
`storeipc.CommitRequest` carries both fields to the owner that enforces
them.

**That tripwire binds `CommitRequest`, not every write to `main`.** It was
written when the two were the same thing: before the review lane, every
write went through `localdolt.commitConn`, so "a new write lane fails
loudly rather than skipping the deny-list" was true of write lanes in
general. After it, that sentence is true of `CommitRequest` alone, and
`review accept` is the counterexample that proves the difference — it
promotes a proposal by merging its branch, so the rows are already staged
and it concludes with a `DOLT_COMMIT` and no `CommitRequest`. Nothing
failed loudly; it simply wrote. Its coverage is a second, hand-maintained
scan in `internal/store/localdolt/review.go`, over prose read from the
proposal branch's own diff, listing the columns in `scannedColumns` —
which has to be kept in step with `Fact.text`/`Decision.text` in
`propose.go` by hand, since nothing checks it. That diff is the head
commit's, so the scan covers the whole of what merges only because
`requireOneCommit` refuses to promote a branch carrying anything else.
**A future lane that writes to `main` without a `CommitRequest` owes the
same, and will get no warning if it forgets.**

## Conventions for agents (PRD §14)

- The PRD is authority; don't silently diverge from it.
- Agents are untrusted writers — the review gate is non-negotiable.
  Propose; humans promote.
- Fail loudly.
- No scope creep beyond the §12 parity matrix; §1.2 non-goals are enforced
  in review.
- Feature branch + PR always.
- Flag new dependencies before adding them.

<!-- orchestrator:managed:start version=1 -->
This file is partially managed by Orch (see `.orchestrator/config.toml`).
- In **Assist** mode, tracked-file changes are mechanically denied; a mutating
  request triggers read-only planning instead.
- In **Delivery** mode, work happens in an isolated per-issue worktree, never in
  this checkout directly.
- Model/effort routing, concurrency, and host plugin setup live in
  `.orchestrator/config.toml` — edit that file, not this block.
- Orch upgrades this block through Delivery. Do not hand-edit it; a hand edit
  blocks the next install/upgrade until reverted or removed.
<!-- orchestrator:managed:end -->