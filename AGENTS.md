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
  §6.4). It is idempotent: a second run adds nothing to the Dolt history.
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