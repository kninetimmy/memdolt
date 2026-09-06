# M3 compatibility evidence and Claude test waiver

Recorded 2026-09-06 for [issue #119](https://github.com/kninetimmy/memdolt/issues/119).
This report supplies the replacement M3 exit evidence in [PRD §16](../prd/memdolt-prd.md#16-milestones--gates):
passing deterministic compatibility checks and one real OpenCode provenance
write. **Claude compatibility should work on the checked paths; live Claude
behavior remains unverified.**

The user canceled their Claude subscription and explicitly waived live Claude
acceptance on 2026-09-06, then approved this evidence plan and the exact synthetic
note in §4. The original gate required a real Claude Code session performing
recall, proposals, elicited review, and task operations. No such session is
claimed here. No runtime, dependency, or template change accompanies this report;
M4–M6 remain deferred.

## 1. Revisions and environment

| Item | Observed value |
|---|---|
| Tested source | `69d3b9f79c20252af84be095d1e3b3c1abc24a83` (merged #118) |
| Fresh local checks | Issue #119 worktree on Windows/amd64; `go version go1.26.5 windows/amd64`; MinGW-w64 GCC 16.1.0 |
| Required Go settings | `CGO_ENABLED=1`, `GOFLAGS=-tags=gms_pure_go` |
| Embedded Dolt / MCP SDK | `github.com/dolthub/driver v1.88.1`; `github.com/modelcontextprotocol/go-sdk v1.7.0` in `go.mod` |
| Fixture binary version | `v0.0.0-20260906130448-69d3b9f79c20`, Go `go1.26.5`, commit `69d3b9f79c20252af84be095d1e3b3c1abc24a83`, rechecked with `memdolt version --json` |
| Fixture binary SHA-256 | `2BD3CE375DDBE33C6C215FA78DE0B1100E8A6E1FE90C0365EE3E7868851BFF12`, rechecked before the approved write |
| Installed Claude Code | `2.1.229`; earlier registration/preflight only, login expired before memory calls; no further live acceptance attempted after the waiver |
| Real OpenCode host | OpenCode 2 `v0.0.0-beta-18684` |

The source revision above is unchanged by this documentation work. The fixture
binary was built before this report; it was not rebuilt from the documentation
commit. Test client names such as `Claude Code` with version `1` are scripted SDK
identities, not observations of the installed Claude host.

## 2. Fixture setup

The real-host fixture is an isolated Git repository at
`C:/Users/Kninetimmy/memdolt-acceptance-20260906`, branch `acceptance`, Git commit
`9979216`. It contains the source revision's coexisting `.mcp.json`,
`opencode.json`, and core skill templates. Both registrations launch
`memdolt serve`; the fixture binary is available on the host's `PATH`.
`.memdolt/config.toml` contains:

```toml
[retrieval]
mode = 'fts'
```

The store is schema 4. Its earlier synthetic seed task is
`01M1VDCPMPC23K5GHQ4C1JXKWH`, Dolt commit
`q01efo7ji772set0d2l00b9oblhoa319`. Before the approved wrap-up, `memdolt note list
--json` returned `{"notes":[]}`. The seed is fixture data, not real project work.

The original bootstrap shell transcript was not recovered. For an equivalent
fresh fixture, use a checkout of the tested source, build with the settings in
§1, put its binary directory on `PATH`, create a new Git repository, and copy
the two registrations and `templates/skills` into it. Run these commands from
the respective directories; this is a reproduction recipe, not a claim these
were the historical bootstrap invocations:

```powershell
# Tested-source checkout.
$env:CGO_ENABLED = '1'
$env:GOFLAGS = '-tags=gms_pure_go'
go build -o .\bin\memdolt.exe ./cmd/memdolt

# Fresh fixture, with that bin directory on PATH and the files above copied in.
memdolt init --json
memdolt task add "Synthetic M3 acceptance seed" --json
memdolt doctor --json
memdolt note list --json
```

Set the FTS config above after initialization. Fresh row and commit IDs will
differ. A new OpenCode run must take its own current ID from its host context;
the recorded ID in §4 is evidence from this run, not an ID to reuse.

## 3. Deterministic compatibility checks

Executed afresh in the issue #119 worktree on 2026-09-06:

```powershell
$env:CGO_ENABLED = '1'
$env:GOFLAGS = '-tags=gms_pure_go'
go test ./cmd/memdolt ./internal/mcpserver -count=1 -timeout 5m
```

Result, exit 0:

```text
ok  github.com/kninetimmy/memdolt/cmd/memdolt         11.513s
ok  github.com/kninetimmy/memdolt/internal/mcpserver   7.351s
```

The existing fixtures create temporary real Dolt stores and apply migrations.
Most protocol tests use paired in-memory SDK transports and scripted responses.
The stdio test launches the Go test binary as a child process that executes the
production Cobra `serve` command. No installed Claude process participates.
The package invocation above runs every named check below; the checked-in tests
contain the exact requests and fixture setup.

| Surface | Passing check and what it establishes |
|---|---|
| Registration | [`TestTrackedHostRegistrationsUseNativeCoexistingShapes`](../../cmd/memdolt/host_templates_test.go) parses Claude `mcpServers.memdolt` and OpenCode `mcp.servers.memdolt`, both invoking `memdolt serve`. [`TestDoctorRecognizesOnlyParsedOpenCodeRegistrationPaths`](../../cmd/memdolt/doctor_test.go) exercises supported JSON/JSONC paths and refusals. This proves config shape/recognition, not a real host connection. |
| Stdio and discovery | [`TestServeCommandUsesStdioWithoutNonProtocolOutput`](../../cmd/memdolt/serve_test.go) connects through a real child process, negotiates `2026-07-28`, lists 16 tools with a positive TTL, closes, and verifies owner/store release. The adjacent cancellation test verifies shutdown ordering. [`server_test.go`](../../internal/mcpserver/server_test.go) covers instructions, discovery/cache hints, and legacy initialize fallback. |
| Recall | [`TestM3ToolsSchemasSuccessAndRefusals`](../../internal/mcpserver/tools_test.go) calls `recall` with `{"query":"race lane","mode":"fts","source_types":["fact"],"provenance":true}` after controlled proposal acceptance and requires a result with provenance. This is FTS protocol evidence, not a live Claude query or a fresh hybrid-quality measurement. |
| Proposal isolation | The same test calls `propose_fact` and `propose_decision`, then queries `AS OF 'main'` to require zero durable rows before controlled acceptance. It also stages supersession and checks visible duplicate-key refusal. Its setup helper accepts with `Force: true`; those accepts do not prove human elicitation. |
| Review | [`TestReviewPendingBatchWorksThroughModernAndLegacyElicitation`](../../internal/mcpserver/elicitation_test.go) calls `review_pending` with `{"mode":"batch"}` and scripted `approve_all`, verifies repository rows and two reviewer-`user` commits, and leaves global proposals pending. Successive/legacy tests cover approve/skip, one-round legacy forms, and traversal to proposal ten; adjacent tests exercise state refusal and partial-progress reporting. They use the application review gate, but their responses are automated, with no human dialog observed. |
| Task operations | `TestM3ToolsSchemasSuccessAndRefusals` calls `task_add` with title `Exercise every M3 tool`, `list_tasks` with status `open`, and `task_done` with the returned ID; a missing ID must return a visible error. The add commit author is asserted as `agent:claude-code` for the synthetic client. |
| Actor attribution | [`TestPerRequestAttributionChangesWithinOneConnection`, the modern/legacy user-identity tests, and `TestMissingIdentityFailsClosedAndOpenCodeKeepsRawName`](../../internal/mcpserver/server_test.go) cover request/session identity, unknown fallback, agent-only MCP identities, and canonical `agent:opencode` with raw `cli`. The tool test verifies note/task commit attribution. Real note fields are independently recorded in §4. |
| OpenCode provenance | [`opencode_test.go`](../../cmd/memdolt/opencode_test.go) runs a fake API child process to check exact optional metadata, direct/owner readback, deny-list checks, and refusal before store open. These fixture results are separate from the real API observation below. |

Official [Claude Code MCP documentation](https://code.claude.com/docs/en/mcp),
consulted 2026-09-06, documents local stdio servers, project `.mcp.json`
registration, and interactive structured form elicitation. Combined with the
passing checks, this supports an **expected-compatibility inference**. It does
not establish what Claude `2.1.229` actually negotiates, displays, or calls.
Actual Claude recall, proposal/task operations, and human elicitation remain
unverified under the user's waiver.

## 4. Real OpenCode wrap-up observation

The actual OpenCode session completed with exit 0. Its captured final response
identified the current session ID as `ses_f892831dbffebwJyPdZgtq7cOr`, sourced
from `<env>` → `Current conversation session ID` in that session's host context.
The workflow did not obtain it from a session list, title, or filesystem search.
An independent `opencode2 api get` returned an exact `data.id` match before the
write; `memdolt opencode session-info` separately verified that ID.

All five commands below were captured in order, with exit 0 and working
directory `C:/Users/Kninetimmy/memdolt-acceptance-20260906`:

```text
opencode2 api get "/api/session/ses_f892831dbffebwJyPdZgtq7cOr"
memdolt opencode session-info ses_f892831dbffebwJyPdZgtq7cOr --json
memdolt note list --json
memdolt opencode wrap-up-note ses_f892831dbffebwJyPdZgtq7cOr "Synthetic M3 acceptance: OpenCode read-only session verification matched Session.Info; this note tests provenance persistence." --json
memdolt note list --json
```

The user explicitly approved that exact synthetic note before invocation.
The first list returned no notes. The writer returned note
`01M1VF63REYH8W4FM504V98QYH`, Dolt commit
`shkrq0os5nn53blu32aij6d9gn2kd5np`, and timestamp `2026-09-06T13:40:35Z`.
The host's final list and an independent operator-side CLI readback both
contained exactly one note:

```json
{"notes":[{"session_id":"ses_f892831dbffebwJyPdZgtq7cOr","id":"01M1VF63REYH8W4FM504V98QYH","actor":"agent:opencode","actorRaw":"cli","text":"Synthetic M3 acceptance: OpenCode read-only session verification matched Session.Info; this note tests provenance persistence.","createdAt":"2026-09-06T13:40:35Z"}]}
```

The real API response had no `data.agent` or `data.model`; `session-info`
returned only `session_id`. The four optional fields `agent_id`, `provider_id`,
`model_id`, and `variant` were absent from the persisted note's public JSON,
matching the available nullable provenance. Raw SQL NULL values were not
directly queried in this acceptance run. No agent/provider/model/variant value
was guessed or copied into note text.

The independent API recheck still matched. `memdolt doctor --json` observed a
reachable owner IPC endpoint (PID `12740`), schema 4, and the parsed OpenCode
registration; the write used the existing owner route. Fixture Git remained
clean on `acceptance`. Captured command evidence is retained locally at
`C:/Users/Kninetimmy/memdolt-acceptance-20260906/artifacts/opencode-wrap-up-approved.jsonl`;
the commands and relevant outputs above make the evidence readable without
that machine-local artifact.

The host-context source is a workflow observation reported by the real host.
The CLI guarantees only validation and an exact API identity match for the
caller-supplied ID; it cannot authenticate where that ID came from. The real
write exercises the CLI provenance workflow and owner routing. It does not
establish OpenCode MCP recall/review behavior or capture its raw MCP handshake;
`actorRaw: cli` here is the CLI note's persisted attribution. No proposal,
task write, render, sync, or transcript-archive feature was exercised by this
approved wrap-up.

## 5. Local and CI evidence boundaries

For this documentation change, the fresh package run in §3 passed.
`git diff --check` passed with no output, and
`git diff --word-diff --ignore-all-space` was reviewed for unintended dropped
words. The original M3 exit sentence remains intact with an explicit replacement
marker; the final diff contains only this report and the PRD gate record.

Source revision `69d3b9f79c20252af84be095d1e3b3c1abc24a83` has a completed,
successful [CI push run 34035001474](https://github.com/kninetimmy/memdolt/actions/runs/34035001474),
queried on 2026-09-06 with:

```text
gh run view 34035001474 --repo kninetimmy/memdolt --json headSha,status,conclusion,url,jobs
```

All five jobs succeeded: `Lint`, `Test (ubuntu-latest)`, `Test (windows-latest)`,
`Test (macos-latest)`, and `Test (race)`. The checked-in
[workflow](../../.github/workflows/ci.yml) runs `go test ./...` on the three OSes,
`go test -race ./...` on Ubuntu, gofmt plus golangci-lint `v2.12.2`, and
`go test -tags golden,gms_pure_go ./tests/golden/... -v -timeout 30m -run TestRetrievalGolden`
on Ubuntu only; the golden step was skipped on Windows/macOS as configured.
This workflow has no standalone `go vet ./...` step, no live-host acceptance,
and no `git diff --check` step.

The prior-session handoff also records broader local test/race/vet/format/lint
and golden checks passing at the same source, including scale Recall@3 of
21/21 with zero safety failures. Those earlier local logs/invocations were not
recovered for this report and those checks were not rerun for this documentation
change. The CI result above was independently queried; it is evidence for the
unchanged tested source, not a claim that this documentation PR has passed CI.
This report is written before that PR's CI outcome; its current checks must be
verified separately before merge.
