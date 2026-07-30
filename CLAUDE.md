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

Go module at the repo root: `github.com/kninetimmy/memdolt`, Go ≥1.24
(`cmd/memdolt` is the only package so far; later milestones add the
`internal/...` packages from PRD §14).

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