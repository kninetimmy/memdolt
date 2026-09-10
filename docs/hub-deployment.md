# Native Linux hub deployment

`memdolt hub init` generates a reviewable, nonsecret deployment bundle.
`hub status` observes a selected deployment. Neither installs a service, applies
a firewall, creates an account/database or opens a local memdolt store. Artifact
generation works on clients; live inspection and managed startup require Linux.

Before issue #147, PRD §13.1 showed a private SQL listener and a remotes port:

```yaml
listener: { host: "100.x.y.z", port: 3306 }   # tailnet IP — never 0.0.0.0
remotesapi: { port: 50051 }
data_dir: /mnt/ssd/memdolt-hub
```

That example remains historical. In native **Dolt 1.88.1**, remotesapi constructs
its own `:port` HTTP/gRPC listener independently of SQL `listener.host`. After
#147 the generated unit requires an **applied nftables ingress boundary**, not
just that YAML setting. The measured native release baseline is exactly 1.88.1;
managed startup refuses other releases. Remotes wire-format metadata describes
the storage/protocol format and does not establish the executable release.
This does not add version guards to existing memdolt clone/push/pull clients.

## Prerequisites and generation

Use Linux with systemd, nftables with JSON output (`nft --json`), and an explicitly
configured private network such as Tailscale. Both private IPv4 and ULA IPv6
addresses must be assigned to the selected interface. The interface must provide
the intended private ingress: for Tailscale, configure tailnet membership and ACLs
separately. A private source address alone is not authentication. For another
network, supply its service/interface/private prefixes; this bundle does not set
up that network or an SSH tunnel. Linux nftables is required for this managed
deployment even if another perimeter also exists.

Install native Dolt only after comparing its archive with a checksum or signature
published by a trusted source. Computing a hash without an expected value is not
verification. The official [v1.88.1 release API](https://api.github.com/repos/dolthub/dolt/releases/tags/v1.88.1)
publishes this `dolt-linux-amd64.tar.gz` SHA-256, checked for the CI gate:

```text
26eccddbd0d7a0da6bc2a5bc6fd2adc2033a8c54d2b56d7204297a310321736e
```

That value applies only to that archive, not ARM64 or a Windows binary. The CI
workflow downloads the official HTTPS asset, verifies it before extraction or
execution, and builds memdolt from this repository. Provision other platforms
with their own trusted published verification; do not reuse this checksum.

Example generation, substituting your actual private addresses:

```sh
memdolt hub init --output /home/operator/review-hub \
  --config-dir /etc/memdolt-hub --data-dir /srv/memdolt-hub \
  --dolt /usr/local/bin/dolt --memdolt /usr/local/bin/memdolt \
  --nft /usr/sbin/nft --user memdolt-hub \
  --network-service tailscaled.service --interface tailscale0 \
  --ipv4 100.90.0.1 --ipv6 fd7a:115c:a1e0::1 \
  --allow-ipv4 100.64.0.0/10 --allow-ipv6 fd7a:115c:a1e0::/48 \
  --sql-port 3306 --remotes-port 50051 --ready-seconds 30 --json
```

`--output` is a new absolute local directory with an existing parent. The six
artifacts are `hub.json`, `config.yaml`, `private.nft`, `SETUP.md`,
`memdolt-hub.service` and `memdolt-hub-boundary.service`. Identical existing
bundles return `unchanged`; partial, modified or foreign directories refuse
without replacement. A new write failure can leave a partial bundle: the result
identifies the output and complete files. Inspect that directory before manually
recovering. There is no automatic deletion, upgrade or overwrite mode.

Paths used in Linux units must be absolute, canonical, distinct and nonoverlapping,
with a restricted literal character set. User/service/interface names, address
families, canonical private prefixes and distinct unprivileged ports are validated
before interpolation. Output/read paths refuse links, reparse points, hard-linked
files, network/device namespaces and protected repository metadata. Hub's rooted
walk checks opened identity. It does not coordinate concurrent foreign filesystem
writers or promise directory-entry crash durability. These restrictions bind
`hub.Init` and hub artifact/deployment consumers, not every filesystem operation.

## Review, accounts and credentials

Review `SETUP.md` and each artifact before installation. Inspect any account,
directory, executable or unit destination already present; do not overwrite a
foreign deployment. Use a dedicated unprivileged account, not a shared login.
The generated SETUP commands name the chosen user/data paths. Copy the complete
bundle to `config_dir`, root-owned directory 0755 and files 0644, then copy its two
unit files into `/etc/systemd/system`. The manifest and generated files must remain
byte-for-byte consistent: edit generation options in a fresh bundle and review a
replacement explicitly if deployment settings need to change.

Installed binaries and all configuration/binary parent directories must be
root-owned and not writable by group or others. Symlinked installs are refused;
use explicit resolved paths. `/tmp` is suitable for client-side generation, but
its writable ancestry is intentionally unsuitable for privileged deployment.
Data belongs to the dedicated account with mode 0700. Never share a data directory
with a second native server, embedded local store owner, or another writer.

Bootstrap SQL credentials **before** enabling remotesapi. A fresh native server
otherwise creates `root@localhost` with an empty password. Use a temporary
loopback-only server with no remotes option and with the *same* data/config/privilege
paths. As the dedicated user in a trusted interactive Bash terminal, with shell
tracing disabled, obtain the first root password without echo or command history:

```sh
cd /srv/memdolt-hub
read -r -s -p 'Initial native SQL root password: ' DOLT_ROOT_PASSWORD
printf '\n'
test -n "$DOLT_ROOT_PASSWORD" || exit 1
export DOLT_ROOT_PASSWORD
export DOLT_DISABLE_EVENT_FLUSH=1
/usr/local/bin/dolt sql-server --host 127.0.0.1 --port 13307 \
  --data-dir /srv/memdolt-hub --doltcfg-dir /srv/memdolt-hub/.doltcfg \
  --privilege-file /srv/memdolt-hub/.doltcfg/privileges.db \
  --socket /srv/memdolt-hub/dolt.sock
# After the bootstrap server has stopped:
unset DOLT_ROOT_PASSWORD
```

Do not set `DOLT_ROOT_HOST=%`. The environment initializes only a *missing*
privilege database; it does not reset credentials in an existing one. No credential
is placed in the generated YAML/unit/manifest. The generated unit and native
version probes disable native event flushing; they add no telemetry. The operator
can also set Dolt's existing `metrics.disabled=true` for the dedicated account.
Native version probes use an owned temporary home with native
`metrics.disabled=true` and `versioncheck.disabled=true` settings and a separate
empty working directory. Native 1.88.1 constructs its event emitter before
disabling metrics, so the probe also owns its exact `eventsData/dolt.lock` file
and directory, then removes its known temporary artifacts. This prevents native
update warnings/network checks
and avoids reading or changing the operator's global/repository configuration.
Cleanup failures are reported; unexpected files are retained for inspection.

In a second trusted terminal, use a native MySQL-protocol SQL client with history
disabled, for example `MYSQL_HISTFILE=/dev/null mysql --histignore='*' --socket
/srv/memdolt-hub/dolt.sock --user root --password`. The password option has no
value: it prompts. Type the two `CREATE USER ... IDENTIFIED BY ...` statements
interactively with fresh secrets for `memory_reader` and `memory_writer`, followed
by the nonsecret database/grant statements in generated `SETUP.md`. Do not put
passwords in script arguments, SQL files, URLs, Git, logs or captured diagnostics.
Keep client statement logging and shell tracing disabled for credential entry.
Verify users and grants using `SHOW GRANTS`, then stop the bootstrap server and
make `.doltcfg/privileges.db` private (0600), owned by the dedicated account.

The example SQL database `memory` is an ordinary native Dolt database. Creating
it does not invent a memdolt schema/history: seed an initialized memory database
through the existing approved clone/push workflow when needed. Existing databases
can be used directly, with their ordinary schema, commits and refs. Hub deployment
does no inference, model download, schema migration or format conversion.

Native authentication is SQL users/grants, independent of the Linux service user:

| Access | Native grant scope in 1.88.1 |
| --- | --- |
| SQL read | `SELECT ON memory.*` |
| Remote clone/fetch/pull | `CLONE_ADMIN ON *.*`; this is a global remote-read privilege |
| Remote push | `SUPER ON *.*`, plus `CLONE_ADMIN` for reads; these are broad server privileges |
| Ordinary SQL write | Explicit `SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, INDEX ON memory.*` in the example |

Do not describe `CLONE_ADMIN` or `SUPER` as database-scoped remote isolation.
Do not grant `GRANT OPTION` to a routine writer. Use a separate server/trust group
if that native scope is unacceptable. Verify read users cannot push and that the
intended writers can before use. Client remote passwords retain the existing
`DOLT_REMOTE_PASSWORD` environment flow; hub generation adds no client credential
transport or password flags.

## Private boundary and startup

`private.nft` atomically creates only `table inet memdolt_hub` with one input
filter base chain at priority -10. Existing same-name tables refuse. It never
flushes or replaces any table or the machine ruleset. Its four ordered rules:

1. Permit loopback TCP to the two configured ports.
2. Permit private-interface IPv4 traffic from the configured source prefix to the
   configured private IPv4 destination on those ports.
3. Permit the corresponding IPv6 traffic from its ULA prefix to its ULA destination.
4. Drop all remaining TCP traffic to either configured port on either IP family.

Other traffic and tables are preserved. Other chains may impose tighter denies;
an accept elsewhere cannot override this input-chain drop. This protects delivery
to the host's two ports, not forwarding traffic, arbitrary proxies or other
processes/ports. Loopback processes still need native credentials. Configure
private-network ACLs separately. See the upstream [nftables evaluation rules](https://www.netfilter.org/projects/nftables/manpage.html#lbBB).

The dedicated boundary unit checks trusted bundle bytes before running `nft -f`
with only `CAP_NET_ADMIN`, then checks the *applied* kernel table through numeric
JSON. Do not enable a distribution `nftables.service` whose config flushes the
ruleset; that can erase unrelated Tailscale/firewall rules. Ensure any other
firewall manager preserves this table on reload.

The server unit binds to the boundary and selected private-network service and
orders after both. Each start first runs the privileged **read-only** `hub
preflight` against the applied table; it does not run Dolt as root. Extra rules,
chains, unsupported JSON semantics, a dormant table, wrong ports/families/addresses,
or missing/unreadable/unapplied protection refuse. Its unprivileged `hub ready`
then checks the root-owned native executable's observed version, private native
privilege-file existence and both interface addresses. `ready` also verifies the
effective UID/GID match the configured nonroot account and group, refusing root
aliases and wrong service identities. Readiness waits at most
`ready-seconds` (1..120); failures are visible in the journal. Systemd retries
after ten seconds and limits starts to three within five minutes. The Dolt
process has no capabilities, runs with the dedicated user/group, and writes only
its data directory under the unit's filesystem restrictions. No shell is used
for these commands.

Before issue #165, a retry timer winning as the context expired could leave only
the context error in the journal, losing the last address failure. A probe that
returned success after cancellation could also pass readiness. After #165,
`waitReady` retains the most recent failed probe on every timeout/cancellation
exit, and `errors.Is` recognizes `context.Canceled` or `context.DeadlineExceeded`.
An already canceled context makes no probe; successful readiness and the 250ms
pause after each failed probe remain. A late successful probe cannot clear a
cancellation or erase a prior failure.

`waitReady` owns this error-identity and retry policy. Its only production caller
is `inspect` in `ready` mode, which retains the diagnostic in its
`private-addresses` check; `finish` still returns its generic failed-check error.
Status still makes one direct address probe, and preflight, native version and
TCP checks keep their separate behavior. Address probes remain synchronous: the
context stops retries and rejects late success, but does not interrupt an
in-flight interface query.

`hub preflight --files-only` is solely the boundary unit's pre-application file
check. A successful result **does not** assert applied protection or authorize a
server start. The server unit always invokes the full preflight first.

After review and credential bootstrap, explicitly run the generated verification,
`systemctl daemon-reload` and `systemctl enable --now memdolt-hub.service` commands.
Use `journalctl -u memdolt-hub -u memdolt-hub-boundary` to inspect visible failures.
Stopping the boundary service stops the bound server but intentionally leaves the
firewall table. To recover a failed/restarted boundary, stop Dolt first, inspect
the existing `inet memdolt_hub`, then explicitly remove only that table if it is
yours and start the boundary again. Unrelated firewall state is never disposable.

The applied check is a snapshot, not a continuous firewall monitor. A later
privileged firewall writer can weaken/remove protection without notifying
systemd; preflight cannot prevent that. The final check/start interval is not
atomic against root. Keep protection applied for the entire server lifetime;
stop the server before firewall changes. This restriction binds generated managed
startup, not arbitrary direct `dolt sql-server` invocations.

## Status and evidence

```sh
sudo /usr/local/bin/memdolt hub status --config /etc/memdolt-hub/hub.json --json
```

Status returns named checks and a nonzero exit for failure. It reports configured
artifact consistency, observed native release, private interface addresses, the
actually applied boundary, native privilege-file existence, and TCP reachability
of SQL/private IPv4 and remotes/private IPv4/IPv6. TCP reachability does not prove
process identity, successful SQL authentication, remote grants or off-network
denial. A different process could occupy a port. File existence does not prove
credential/grant correctness. Permission/probe failures remain failures; an
unsupported platform reports live checks as unknown and exits nonzero. Run status
on the hub with the privilege needed to read nftables; it does not remotely inspect
another machine.

`tests/hub/verify_linux.py` runs in CI with a checksum-verified native 1.88.1
binary, a disposable container and three private network namespaces. Package
installation and its fixture account exist only in that container image; the
runner's firewall/services are untouched, its mounts are read-only, and the
container has no external network. The rig validates generated unit
grammar with `systemd-analyze verify`, replays the actual unit's ordered startup
arguments with separate privileges, starts Dolt using generated YAML, and probes
permitted/denied IPv4/IPv6 traffic to both protected ports. Dual-stack fixture
listeners exercise the SQL IPv6 firewall even though native SQL binds IPv4. The
rig also checks unrelated traffic/table preservation, missing/dormant/foreign
protection, native startup/status, wrong version and bounded missing-address
failure. It does not install/enable units under the runner's PID 1 or change its
host firewall/accounts/services. Ordinary and golden gates remain unchanged.

This is isolated deployment evidence. The physical Pi/Linux hub, two real clients,
tailnet/off-network probes, project topology/identity, backup/retention and the
complete M4 acceptance gate remain separately tracked. The user's live hub has
not been installed, changed or accepted by this delivery.

## Doctor compatibility (issue #167)

Before #167, doctor had five local checks and no executable-release evidence or
explicit hub/remote selection. After #167 these commands expose the existing
inspection paths and their limits:

```sh
memdolt doctor --dir /path/to/repository --json
memdolt doctor --remote origin --dir /path/to/repository --json
sudo memdolt doctor --hub --config /etc/memdolt-hub/hub.json --json
```

Ordinary doctor remains offline apart from local owner IPC. It adds
`embedded-dolt-version`, sourced from the pinned dependency's
`github.com/dolthub/dolt/go/cmd/dolt/doltversion.Version`, currently **1.88.1**.
That identifies the Dolt release embedded in the running doctor binary. It does
not inspect an installed native binary or a separately running owner binary.
The measured baseline is exactly **1.88.1**; a different or unobservable embedded
release fails. There is no measured compatible range. The driver's environment
version label (default `0.40.17`) and the embedded SQL `DOLT_VERSION()` default
`SET_BY_INIT` are not release evidence. No dependency or build setting changes.

Hub selection uses the existing `hub.Inspect(..., "status", false)` path. It
does not resolve/open repository memory, even when the working directory has a
store. The same absolute generated manifest, matching bundle, Linux platform,
trusted deployment paths and permission to inspect nftables are required. Its
configured native executable is probed with the existing isolated version
environment. The JSON `hub` report retains `observed_dolt_version`, including
unsupported observed releases; doctor also lists each check with a `hub-` prefix
in human and JSON output. Missing/invalid configuration, unsupported platforms
and failed hub checks exit nonzero. Unknown native evidence is never replaced
by the embedded release. The listener/process, grant and off-network limits of
`hub status` above still hold; this does not inspect a remote host over SSH.

Explicit remote selection requires an existing initialized repository and uses
the same `storeFlags.open` direct/authenticated-owner route and `RepoStatus`
operation as `repo status <name>`. It honors the configured remote name even
with `topology='local'`. It fetches only remote main and validates the captured
committed schema/identity through that operation; it never promotes a commit or
resolves a conflict. `remote-compatibility` reports the captured hashes and
current/ahead/behind/divergence result. A missing remote, invalid schema/identity,
transport refusal, failed owner call or close error fails. Both absent committed
identities and conflicted/unassessed divergence warn with inspection remedies;
legacy RepoStatus acceptance and merge-preview rules remain unchanged.

The optional `--user <username>` overrides the remote's stored SQL username.
Without either username the existing anonymous route remains. Passwords come
only from `DOLT_REMOTE_PASSWORD` in the executing process's environment. A live
owner uses its own environment, so restart it to change the password; the CLI's
environment does not replace it over IPC. No personal Dolt credentials are
loaded. Existing remote URL validation and diagnostic redaction remain.

Successful fetch/schema/identity checks do **not** observe the remote executable
release. `remote-dolt-version` always warns that it is unobserved and names the
on-hub doctor command above. Storage/wire-format matches, the local release and
successful transfers cannot prove that release. Warnings exit zero; a green
overall report therefore does not mean every compatibility fact was observed.

JSON's optional `remote` field contains the RepoStatus observations that actually
returned, retained even if close later fails. Failure details also retain captured
main hashes in both formats, with empty hashes explicitly unobserved. A refused
direct call may retain partial observations with `status='refused'`; a failed authenticated owner call
may return none. The failed check remains authoritative: absence/zero values are
not successful observations, and a lost reply is never replayed. Main, proposal
branches, tags, staged/working memory, derived indexes and config stay unchanged.
Fetched objects and the selected tracking ref may remain on success or refusal,
exactly as documented for RepoStatus; inspect local main before retrying.

`--hub` cannot combine with explicit `--dir`, `--remote` or `--user`.
`--config` requires `--hub`, and `--user` requires a nonempty `--remote`.
Missing/empty/conflicting selections refuse before inspection and print a failed
`selection` check in both formats. `--global`, `--diff` and password flags are
not doctor inputs. These selection restrictions bind `newDoctorCommand`, not
every CLI command; the underlying hub and repository APIs retain their own
validation. `runDoctor` performs the existing-store remote guard before the
schema reader, preventing its existing Open behavior from creating a partial
store. Ordinary doctor's absent-store/ownership behavior remains unchanged.

The isolated Linux rig now also executes the actual doctor hub command in both
formats against checksum-verified native 1.88.1, then an explicitly identified
test executable reporting 1.88.2. It requires observed release/check failures,
not just a nonzero process exit. The native startup, namespace-only firewall,
read-only mounts, no external network and all previous assertions remain.
This adds no physical two-client/private-network acceptance. Pi/Linux and two
real clients must still perform the PRD §16 counts/hashes/reopen gate. M6's
backup age, disk headroom/trend and restore drill remain separate.

The complete #167 changed-element inventory is:

- `cmd/memdolt/doctor.go:newDoctorCommand` adds only the explicit selectors,
  validation and help above; root registration and all other commands remain.
  `doctorReport` retains `ok`, `checks` and local `dir`, omits `dir` outside
  repository reports, and adds optional original `hub`/`remote` evidence.
  Existing `doctorCheck` fields remain. The status constants retain their
  values; the warning comment now includes incomplete remote evidence.
- `runDoctor` adds the embedded check, remote existing-store preflight and
  optional remote inspection, preserving the original five checks and their
  owner/direct ordering. `lockCheck`, `ownerCheck`, `schemaCheck`,
  `readSchemaVersion`, `emptyRecallCheck` and OpenCode parsing are unchanged.
  `embeddedDoltCheck` compares only its supplied pinned release with `hub.Version`;
  its production callers pass `doltversion.Version`. This is not a version gate
  on every Store/transfer/native SQL operation or a query of the owner binary.
- New `runDoctorHub` reuses `hub.Inspect` status and keeps its checks/version and
  error. No hub checker, generated artifact, path guard, native probe, firewall,
  readiness policy, credential or service changes. `doctorRemoteCheck` reuses
  one `RepoStatus` call, closes the opened store once and retains returned
  observations through errors. `remoteDoltCheck` supplies the unobserved-release
  advisory. Neither changes Store, IPC, transfer/schema/identity validation,
  proposal mutex/foreign-writer limits, merge preview, credentials or replay.
- New `finishDoctorReport` extracts the failed-check count and joins output
  errors so failures remain nonzero. `writeDoctorReport` retains one JSON object
  or one line per check, adding a hub target heading. Existing warning exits
  remain zero. No new logging/telemetry or background diagnostic service exists.
- `cmd/memdolt/doctor_test.go:TestDoctorHumanOutputNamesEveryCheck` additionally
  expects the real embedded release; all original assertions remain. New
  `doctor_compatibility_test.go` exercises supported/unknown/skew release reports,
  selection/hub failures, absent/partial stores, offline defaults, actual
  direct/live-owner native remote inspections, committed schema/identity
  refusals, synthetic gRPC username/owner-password/redaction, and preservation
  of branches, complete working/staged roots, tags, counters, indexes and config.
  The late-close/output checks retain actual captured native hashes. Fixtures
  create no live user data, deployment, account or credentials.
- `tests/hub/verify_linux.py:verify` adds the actual doctor baseline/skew human
  and JSON assertions; its nested `report` only adds command selection for
  doctor. Existing setup, cleanup, namespaces, native/server probes and guards
  remain. Dockerfile, CI job names/commands, release verification, ordinary and
  frozen golden gates are unchanged.
- README, AGENTS, this runbook and PRD §§11.5/13.1/16 record the before/after,
  exact evidence and remaining gates. Shared `onboarding.md` changes the named
  local check count from five to six with its before-state retained; its
  approval, host discovery, memory, rendering and provenance procedures remain.
  No installed host workflow, MCP registration, dependency or schema changes.

## Structural scope

- New `internal/hub/config.go` owns only nonsecret hub settings, validation and
  six generated artifacts. Existing repository topology/configuration is unchanged.
  `Config.Validate` restrictions apply to all of these generated interpolations,
  not general shell/SQL/unit callers elsewhere.
- New `files.go` and `path_windows.go`/`path_other.go` own rooted hub file reads and
  create-only bundle output, exact-byte no-ops, preservation/partial-write reports,
  root ownership for deployment and link checks. Renderer, code-index and owner
  source protections retain their prior independent contracts.
- New `boundary.go` validates the single observed generated nftables table;
  `check.go` owns hub status/preflight/readiness and bounded, argument-based native
  probes, with isolated temporary native configuration for the version probe.
  These checks neither apply policy nor start a server. Existing doctor,
  native transfer/authentication and all local CLI/MCP owner behavior remain.
- New `cmd/memdolt/hub.go` supplies the four hub verbs with explicit paths/options
  and human/JSON reports. `root.go` adds one command family; all existing children,
  memory mutations, 22 MCP registrations and code-index behavior remain unchanged.
- New hub/CLI fixture tests and `tests/hub/verify_linux.py` plus its Dockerfile verify only this
  delivery. `.github/workflows/ci.yml` adds `Test (hub ingress)` separately; `Lint`,
  all existing `Test (OS)`/race status-context names, ordinary checks and golden
  retrieval/locator gates remain intact. No Go dependency or durable migration
  changes. Help, this runbook, AGENTS and PRD §§11/13/16 record the before/after
  correction and these boundaries; older SQL-bind-only prose is preserved.

The issue #165 readiness correction touches only these structures:

- `internal/hub/check.go:waitReady` consolidates cancellation exits and keeps the
  latest probe error as described above. Its signature, configured timeout and
  retry cadence remain. `inspect`, `Report.add`, `finish` and the CLI still
  report failed readiness and refuse startup through their existing paths;
  configuration, native release, service identity, credentials, ingress checks
  and generated artifacts are unchanged.
- `internal/hub/hub_test.go:TestVersionAndBoundedReadiness` keeps the native
  version assertions and replaces wall-clock readiness allowances with standard
  library `testing/synctest` checks of exact retry/deadline timing and cancellation.
  New `TestReadinessRetainsFailureAfterLateProbe` checks prior/latest diagnostic
  retention after canceled or expired probes return success or failure. These
  tests use the real helper; no production timer seam or live environment is
  added. The Linux ingress rig, ordinary CI and golden gates remain unchanged.
- This runbook adds the before/after and scope record while retaining the earlier
  bounded-readiness and journal statements. No CLI interface, dependency, schema,
  credential, service or firewall setting changes.

The native binding and privilege observations above are grounded in the pinned
[Dolt sql-server source](https://github.com/dolthub/dolt/blob/v1.88.1/go/cmd/dolt/commands/sqlserver/server.go)
and [remotes authorization interceptors](https://github.com/dolthub/dolt/blob/v1.88.1/go/libraries/doltcore/remotesrv/interceptors.go).
