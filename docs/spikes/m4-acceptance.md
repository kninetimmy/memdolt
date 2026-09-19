# M4 physical fixture acceptance and remaining work

Recorded 2026-09-19 for [issue #177](https://github.com/kninetimmy/memdolt/issues/177).
The [PRD §16](../prd/memdolt-prd.md#16-milestones--gates) physical two-client
round-trip/counts/hashes/plain-reopen gate **passed for the exact embedded
1.88.1-to-native-1.88.1 fixture**. Task 42 and full M4 remain open. Production
managed deployment and native reader/writer grant acceptance, real-host MCP
conflict dialogs, physical external-host ingress denial and broader version
compatibility are unverified. Optional live SQL topology remains optional;
the M5 parity and M6 restore gates are unchanged.

Before this record, #147/#163/#167 documented implemented subsets and left the
physical gate pending. After it, the original receipts establish the narrower
fixture pass below; they do not convert those subsets into production acceptance.
No live operation was performed while preparing this documentation.

## 1. Physical evidence and provenance

[Selected original evidence](m4-acceptance-evidence.md) contains exact excerpts
from the Architect-supplied `RESULTS.md` and selected credential-free JSON entries
of `evidence.zip`. The source report identifies the machines and procedure;
the named receipts preserve outcomes, including failure and cleanup. The raw
archive and harness are not committed. These are historical observations, not
new branch test results. Binary hashes without trusted publisher expectations
would establish consistency only; this report makes no supply-chain claim from
the original report's binary hashes.

| Item | Observation and original source |
| --- | --- |
| Source | Clean Git revision `937db3caead136abd7c70901184caa17906dcf39`; original `RESULTS.md` revision/build excerpt. |
| Clients | Katherine's MacBook Pro, macOS/arm64; DESKTOP-LCJ0F6H, Windows/amd64. Both built with Go 1.26.6, CGO and `gms_pure_go`, embedded Dolt 1.88.1, native schema 4; original machine/build excerpt. |
| Hub | RasPi Linux/arm64, native Dolt 1.88.1; separate disposable `m4_acceptance` database, SQL 53306/remotesapi 55051, direct Tailscale transfers. Original topology/results excerpt; exact-version scope stated in its limits. Existing `dolthub.service`, accounts and data were unchanged. |
| Identity and seed | `memdolt-m4-fa4e2db7`, Git-origin label `https://example.com/acceptance/memdolt-m4.git` (no request to that domain), seed main `sodb383fg9gg8tp4jcfb8f3062908fmn`; original topology and result 1. |
| Failed bootstrap | `seed-push.json`: exit 1, `no common ancestor`, unconfirmed remote outcome and inspection remedy. `pi-initial-main.json`: inspected unrelated main `dig8i3r6hokd84huf3k2lokfqphm6e2q`; original report records unchanged state. |
| Corrected bootstrap | Original topology excerpt: temporary server stopped, empty database preserved separately, closed native seed copied before either clone; `pi-copy-native-seed.json`: exit 0. This is fixture initialization, not proof of a first push into independent history. See the [corrected runbook](../hub-deployment.md#seed-an-existing-memdolt-history). |
| Independent writes | Each client added a task and note; Mac pushed, Windows merged to `mmlptp6nshs9hgvpbu6dide4eljjgj27`, then pushed and Mac pulled. Original results 1–2 and final exports retain both clients' rows. |
| Conflict | Both clients blocked the same task with different notes. Windows displayed one conflict with base/ours/theirs and blame; main remained clean and unchanged. `resolution.json` binds both displayed heads and chooses `theirs` (Mac). Original results 3–4; this is an explicit CLI task-row resolution, not a done-versus-edit race or an MCP dialog. |
| Convergence | Final main `bivhftr074bciqg4rfuiqufob3d6nqt0` on Mac, Windows and Pi; `comparison.json`, both final exports and `pi-row-comparison.json`. Both client stores and hub clean: original result 5 and hub comparison receipt. |
| Native ancestry | `pi-row-comparison.json`: final merge parents `8jruo5g8hjd3of3td4hkmnkf2v7qae6i`, `47bti82vpa8cd52a67toi8h971nvq2ht`; earlier merge parents `prgiaj5260ub143786pqliha7jlo52m1`, `3q8g3v01mbafj873vq7kvedsbn9a2p3t`. |
| Rows | Exactly 3 tasks, 3 session notes, 1 state narrative; other exported tables and pending proposals empty. Both final exports match exactly except `exported_at`. Independent Pi SQL matches all 53 fields, including 15 explicit NULLs; selected exports and `pi-canonical-rows.json` allow replay below. |
| Content digest | SHA-256 of canonical exported `tables`: `c9a6e1a467b4db4840d54f345559c0e8308e8a01310fac9163885ef11b881390`. Whole export hashes differ because timestamps differ; those hashes are not the equality evidence. |
| Offline reopen | `offline-reopen.json` and original result 8: fresh ordinary processes on both clients read schema 4/final main and tasks/notes with the hub stopped, clean stores, no conversion, import or post-clone init/migration. |
| Authentication and ingress | `wrong-password-refused.json`: clone exit 1, native unauthenticated error. Original result 9 records allowed Mac IPv4/IPv6 remotes probes and four denied SQL/remotes IPv4/IPv6 probes from an isolated non-tailnet namespace/veth, with drop counter +4. This is not reader-versus-writer grant verification or an external physical off-tailnet host. |
| Cleanup | `pi-stop-final.json`: server stopped, ports 53306/55051 closed. `cleanup.json`: temporary firewall/namespace/veth removed; original hub service active and original firewall retained. Original cleanup excerpt records local generated plaintext test credential deletion and retained closed fixture databases/evidence. |

Native ordinary JSON omits NULLs; uncast `JSON_OBJECT` also changes ENUM and
DATETIME presentation. The final Pi comparison used `JSON_OBJECT` with
`CAST(column AS CHAR)` to preserve the clients' string/NULL representation.
The source report retains earlier formatting mismatches as harness issues;
they are not evidence of differing stored rows.

This read-only replay uses only committed selected receipts (run from repo root):

```python
import hashlib, json, re
from pathlib import Path

text = Path("docs/spikes/m4-acceptance-evidence.md").read_text(encoding="utf-8")
receipts = {name: json.loads(body) for name, body in
            re.findall(r"### ([^\n]+)\n\n```json\n(.*?)\n```", text, re.S)}
mac, windows = (receipts[n + "-final-export.json"] for n in ("mac", "windows"))
assert {k: v for k, v in mac.items() if k != "exported_at"} == {
    k: v for k, v in windows.items() if k != "exported_at"}
tables = mac["tables"]
pi = json.loads(receipts["pi-canonical-rows.json"]["stdout"])
for name, rows in tables.items():
    observed = pi[name]["rows"]
    assert ([json.loads(r["row_json"]) for r in observed] == rows
            if rows else observed == [{"row_count": "0"}])
values = [v for rows in tables.values() for row in rows for v in row.values()]
assert len(values) == 53 and values.count(None) == 15
digest = hashlib.sha256(json.dumps(tables, sort_keys=True,
                                  separators=(",", ":")).encode()).hexdigest()
assert digest == receipts["comparison.json"]["canonical_tables_sha256"]
print("PASS: both clients, Pi, 53 fields/15 NULLs, canonical digest", digest)
```

## 2. Every M4 scope item

Implementation references describe existing contracts, unchanged by this issue.
Checks marked **fresh** ran in §4. Other named checks are existing coverage
inventoried from source, **not rerun by this report's selected gate**.

| PRD §16 scope | Implemented behavior and deterministic evidence | Physical evidence / remaining acceptance |
| --- | --- | --- |
| Remotes config | Native remote list/add/remove and stored username; [`TestRemoteCLIConfigAndTransferDirectAndOwner`](../../cmd/memdolt/remote_test.go). | Fixture used an origin for both clones/transfers. Full live credential/config lifecycle not measured. |
| Pull/push/repo-status | Native transfers, committed inspection, compatible two-parent merges; **fresh** [`TestPullMergeIndependentAndSatisfiedHistory`](../../internal/store/localdolt/pull_merge_test.go), [`TestPullCLIConflictResolutionDirectAndLiveOwner`](../../cmd/memdolt/pull_merge_test.go), [`TestPullOwnerCompleteResolutionAndLostOrLateReplies`](../../internal/storeipc/pull_merge_test.go), [`TestDoctorRemoteCommittedCompatibilityDirectAndOwner`](../../cmd/memdolt/doctor_compatibility_test.go). | Physical independent writes, divergence, one explicit task-row conflict, equal final rows/head, offline reopen passed. Not every transport/failure class was physically exercised. |
| Conflict elicitation | **Fresh** [`TestRepoPullMCPAtomicContinuationAndGenuineLegacy`, `TestRepoPullStateAttacksAndStorageFailuresNeverPromote`, `TestRepoPullPushMCPCompatibleAttributionAndFallback`](../../internal/mcpserver/transfer_test.go) cover scripted modern/legacy choices, state refusal and terminal fallback. | No installed-host dialog participated. Real-host MCP conflict dialogs remain open; do not transfer the M3 Claude waiver into an M4 acceptance claim. |
| Hub init/systemd docs | Six generated artifacts and private startup checks; [`TestHubArtifactsAndNativeYAML`, `TestHubBundlePreservation`](../../internal/hub/hub_test.go), [isolated native Linux rig](../../tests/hub/verify_linux.py). **Fresh** `TestVersionAndBoundedReadiness` covers exact release/bounded readiness. | Separate Pi native server exercised transfer, not a production installation of the generated units. Production managed deployment and physical external-host ingress denial remain open. Bootstrap instructions corrected below without changing generated bytes. |
| Auth setup | [Runbook](../hub-deployment.md#review-accounts-and-credentials) separates Linux ownership, SQL users and global remotes grants. [`TestCloneAuthenticationAndCancellationAreLocalAndRedacted`](../../internal/store/localdolt/clone_test.go) covers client credential routing/refusal. | Valid writer transfers and wrong-password refusal observed. `SHOW GRANTS`, reader clone/pull plus denied push, and writer push acceptance on the intended deployment remain open. A bootstrap success message alone does not prove grant boundaries. |
| Version-skew guards | Committed schema/identity validation; **fresh** `TestDoctorEmbeddedReleaseEvidence` and `TestDoctorRemoteCommittedCompatibilityDirectAndOwner` in [doctor checks](../../cmd/memdolt/doctor_compatibility_test.go), plus `TestPullRefusesInvalidMergedConstraintsAndSupersession` in [pull checks](../../internal/store/localdolt/pull_merge_test.go). | Exact 1.88.1 pair/schema 4 passed. Other native/client releases and version ranges remain unverified. Doctor's remote executable release remains unobserved unless separately inspected on the hub; storage metadata is insufficient. |
| Topology config | Local/clone routing, Git-derived committed identity, explicit adoption and optional startup pull; [`TestRepoConfigDefaultTargetAndLocalPolicy`, `TestRepoStartupNativeRefusalAndLateMerge`](../../internal/store/localdolt/repo_config_test.go), [`TestProjectIdentityNativeRoundTripAndMismatch`](../../internal/store/localdolt/project_identity_test.go). | Fixture shares identity across real clones and reopens locally unchanged. Production routing/adoption/startup deployment acceptance is separate; see [topology contract](../repository-topology.md). |
| Remote `Store` implementation (topology B), if time allows | Live SQL mode still refuses. No remote SQL Store implementation is delivered. | Optional work remains optional and is not silently made a blocker or claimed shipped by this fixture. |

## 3. Every §6.3 conflict class

All named checks in this table are **fresh** from
[`internal/store/localdolt/pull_merge_test.go`](../../internal/store/localdolt/pull_merge_test.go).

| Conflict class | Existing policy and passing check | Physical disposition |
| --- | --- | --- |
| Same fact key, distinct ULIDs | `TestPullConflictRowsAndAtomicResolution/key` and `TestPullExplicitTaskReopenAndManualFactWinner/key`: select/manual winner, supersede losers, retain both rows and one live key. | Not exercised; fixture has no facts. |
| Supersede-and-replace | `TestPullMergeIndependentAndSatisfiedHistory/satisfied`: verify already-satisfied constraints, clear records, preserve supersession and two parents. | Not exercised; fixture has no facts. |
| Same-row cells, including fact value | `TestPullConflictRowsAndAtomicResolution/value` and `TestPullManualChoicesRejectMalformedAndPreserveSnapshot`: exact rows/blame, complete head-bound choices, refusal preservation and manual value. | One task blocked-versus-blocked note conflict passed with explicit `theirs`; not every supported table or manual row shape. |
| Task done-versus-edit | `TestPullConflictRowsAndAtomicResolution/task` refuses implicit reopening; `TestPullExplicitTaskReopenAndManualFactWinner/task` allows explicit reopening. | Not exercised: neither physical task edit completed the task. |
| Notes/fresh-ULID inserts | `TestPullMergeIndependentAndSatisfiedHistory/independent`: preserve both histories and clean resulting store; shared merge gate checks both failure surfaces. | Independent tasks and notes from both clients survived; 3 of each after merge. |
| Schema conflicts | `TestPullRefusesInvalidMergedConstraintsAndSupersession/schema` and `/unknown_schema` refuse changed/unknown DDL; `/metadata` refuses conflicting metadata without moving main. | Not exercised: schema stayed 4 throughout. Upgrade remedy and compatibility work remain as documented. |

Dolt's disjoint-cell automatic merge is separate from a conflicting same-cell
edit; the [M1 reproduction](m1-conflict-surfaces.md#5-task-edits-on-disjoint-columns)
is historical native evidence, not a freshly repeated physical case. The divergent
`Pull` restrictions described in PRD §6.3 bind that operation and its CLI/owner/MCP
callers, not every Store write, raw native SQL operation, proposal acceptance or
fast-forward. This report neither broadens nor weakens those restrictions.

## 4. Fresh local verification and CI boundaries

Executed in the issue #177 Windows/amd64 worktree on 2026-09-19:

```powershell
$env:GOTOOLCHAIN = 'go1.26.6'
$env:CGO_ENABLED = '1'
$env:GOFLAGS = '-tags=gms_pure_go'
go test ./internal/store/localdolt ./internal/storeipc ./internal/mcpserver ./cmd/memdolt ./internal/hub -run 'Test(Pull|RepoPull|DoctorEmbeddedReleaseEvidence|DoctorRemoteCommittedCompatibilityDirectAndOwner|VersionAndBoundedReadiness)' -count=1 -timeout 5m
```

Exit 0: localdolt **18.703s**, storeipc **3.352s**, mcpserver **25.587s**,
cmd/memdolt **12.001s**, hub **0.092s**. These are real disposable native stores
and scripted protocol clients, not installed-host UI tests. No new runtime test
or framework is warranted for this documentation change.

The receipt replay above passed: both clients and Pi match on 53 fields/15
NULLs and the canonical digest. All 13 selected JSON blocks also matched their
original archive text exactly; new local link targets exist. `git diff --check`
passed. `git diff --word-diff --ignore-all-space` was reviewed: removal markers
belong to the intentional bootstrap/status corrections, with no unintended
dropped words. These are local documentation checks, not physical reruns.
CI does not run this exact selected Go invocation or `git diff --check`.
Required branch checks remain **Lint**, **Test (ubuntu-latest)**,
**Test (windows-latest)**, **Test (macos-latest)** and **Test (race)**;
**Test (hub ingress)** is advisory. The tested revision's
[post-merge CI run 34772048333](https://github.com/kninetimmy/memdolt/actions/runs/34772048333)
passed all six jobs; that historical evidence does not substitute for this
documentation branch's CI or a production deployment.

## 5. Remaining task 42 work and exact structural scope

Next, inspect the intended production hub and its existing data/accounts/units,
then obtain approval for its concrete managed deployment. Verify native grants
with separate reader/writer identities and retained denial/success receipts.
Exercise allowed private and denied physical external-host traffic for both
ports/address families. Run a real supported host's conflict dialog, recording
host/protocol versions, displayed provenance, explicit choices, canceled flow
and final persisted rows. Any broader release support needs an explicitly
selected compatibility matrix and independently observed native releases;
do not infer it from the one passing pair. Keep unobserved conflict classes
distinguished from deterministic coverage. Backups/restore, retention/GC and
upgrades stay in their existing M6 work; M5's parity audit remains separate.

The complete changed-element inventory is:

| File / element | Before and after; preserved behavior |
| --- | --- |
| `docs/spikes/m4-acceptance.md` (this report) | New evidence/disposition/verification/scope record. Adds no execution path or acceptance waiver. Full M4 remains open. |
| `docs/spikes/m4-acceptance-evidence.md` | New selected original receipts and source excerpts; no secrets, harness or unreviewed archive copied. Historical payloads remain exact. |
| `docs/hub-deployment.md` account/bootstrap paragraph, seed procedure, physical and doctor status | Retains the old clone/push sentence as a before-state; removes its implication that unrelated native genesis is a fast-forward seed target. Adds stopped native seeding into a new destination, preservation/credential separation and post-start checks. Updates pending physical status to this fixture pass. Existing account, private boundary, exact-version, readiness and doctor contracts hold. |
| `README.md` M4 status and self-host guidance | Retains the prior pending-gate statement as historical; links the pass and its open boundaries. Other milestone status and commands hold. |
| `AGENTS.md` Build/test/run status | Adds a current #177 before/after notice above historical delivery records. Build/test conventions, agent policy, managed Orch block and earlier runtime scopes hold. |
| `docs/prd/memdolt-prd.md` §16 status | Adds the fixture's evidence/disposition link and open work; original gate wording, historical subset statements, §6.3 policy, optional topology and M5/M6 gates hold. |

No Go symbol, generated artifact byte (including `SETUP.md`), runtime policy,
dependency, durable schema, workflow, service or credential changes. Existing
`internal/hub/files.go:compareFiles` still requires exact bytes for every file
in the supplied generated artifact set; this is the hub bundle comparison
contract, not a restriction on every filesystem reader. The generated SETUP's
existing runbook link reaches the corrected guidance without invalidating a
bundle. No new restriction is attributed to a runtime symbol by this issue.
