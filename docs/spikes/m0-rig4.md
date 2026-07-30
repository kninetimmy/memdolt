# M0 rig 4 — Dolt hub on the Pi, two-client round trip over Tailscale

PRD §16's rig 4 asks whether a Dolt hub deployed on the Pi and reached over
Tailscale supports `clone`/`pull`/`push`/`merge` between real, physically
distinct clients. Its exit gate is **"push/pull round trip clean"**.

## 1. Verdict

**PASS.**

The gate is met, demonstrated in both directions between two distinct
clients through the Pi hub:

- Windows → hub → macOS (macOS re-cloned and received the Windows commit)
- macOS → hub → Windows (a fresh Windows clone received the macOS commit)

Every one of these was verified by a **fresh re-clone into a clean
directory**, never by a push exit code — each clone's contents were checked
against what should be there. `dolt pull` was also exercised as a distinct
operation, forced into a real fast-forward, rather than relying on clone
alone to stand in for it.

### What this PASS does and does not cover

It says: two independent physical machines, on the tailnet, using SQL-grant
auth over remotesapi, can clone from and push to a shared Dolt hub on the
Pi, and a third machine's fast-forward pull picks up what one of them wrote.

It does **not** say anything about merge, divergent history, concurrent
writers, non-fast-forward push rejection, or the off-tailnet firewall
property — see §6.

## 2. What was measured on

| | |
|---|---|
| Date | 2026-07-30 |
| Dolt version (hub and both clients) | v1.88.1 |
| Hub | Pi 5 "raspi", aarch64, tailnet 100.69.137.74 |
| Client 1 | Windows, DESKTOP-LCJ0F6H |
| Client 2 | macOS, "katherines-macbook-pro", darwin-arm64, tailnet 100.114.161.49 |
| Hub database | `rig4smoke` (scratch db left from loopback verification) |
| Hub port | remotesapi 50051 |

## 3. Hub deployment

The hub runs on the Pi 5 as a systemd service:

| | |
|---|---|
| service | `dolthub.service`, system unit, enabled (survives reboot) |
| unit file | `/etc/systemd/system/dolthub.service` |
| runs as | `dolthub` system user, nologin |
| binary | `/opt/dolt/bin/dolt` v1.88.1 |
| data dir | `/var/lib/dolthub`, on the Pi's SD card |
| ports | MySQL 3306 bound to `100.69.137.74`; remotesapi 50051 on the wildcard |
| boot handling | `ExecStartPre` polls up to 120 s for the tailnet IP before starting `dolt`; `Restart=on-failure` |
| ops | `systemctl {status,restart} dolthub`; `journalctl -u dolthub -f` |

**Pinned to v1.88.1, not current dolt.** `dolthub/driver v1.88.1` embeds
dolt at the pseudo-version cut for the v1.88.1 release (2026-05-07). Dolt's
current release is v2.2.3. Installing current dolt on the hub would put a
major-version boundary inside the experiment — a failed round trip would be
ambiguous between "remotesapi doesn't work" and "a v1 client can't talk to
a v2 hub," two variables in one measurement. The hub is pinned to match the
client exactly so rig 4 measures one thing. PRD §13.1's stated floor
(remotesapi push supported on Dolt ≥ v1.30 both sides) is cleared either
way, so pinning costs nothing in capability. Skew against v2.x is a real,
separate question that belongs to §16's M4 "version-skew guards," to be
measured only once rig 4 has a clean same-version baseline.

**SD card, not SSD — amended decision.** The earlier hard requirement for
an SSD data dir has been relaxed to a recommendation: final deployment
should use an SSD (endurance and random-write performance under commit
churn), but memdolt must not refuse to run without one, and rig-4 testing
ran on the Pi's SD card, the only storage attached, because rig 4's
question is functional, not a performance one. No timing or throughput
figures were taken during rig 4, so this run creates no SD-card-annotation
obligation.

**remotesapi cannot be bound to one address.** In dolt v1.88.1 the
remotesapi listener has no host/address configuration — `--host` governs
only the MySQL listener, and the `remotesapi:` config block accepts only
`port` and `read_only`. (A `cluster.remotesapi.address` field exists but is
a different feature.) `--remotesapi-port=50051` therefore always listens on
the wildcard (`ss` shows `*:50051`), reachable from the LAN as well as the
tailnet. The "bound to the tailnet IP only" goal is unachievable by
configuration and is instead enforced by an nftables rule
(`nft add rule inet memdolt_hub input iifname != "tailscale0" iifname !=
"lo" tcp dport 50051 drop`), applied and removed by the `dolthub.service`
lifecycle rather than a persistent `nftables.service` (whose
`/etc/nftables.conf` starts with `flush ruleset`, which would tear down
Tailscale's own live rules). **This rule's off-tailnet drop has never been
probed** — see §6.

## 4. Auth model and error taxonomy

remotesapi clone/push requires a SQL user holding `CLONE_ADMIN`. The
auto-created `root@localhost` doesn't qualify and isn't reachable from
another host anyway, so a fresh hub refuses every clone from it. Hub setup
(v1.88.1 removed `sql-server -u/-p`; users are created in SQL, and global
flags must precede the subcommand):

```
dolt --data-dir=<D> --privilege-file=<D>/.doltcfg/privileges.db sql -q "
  CREATE USER IF NOT EXISTS 'rig4'@'%' IDENTIFIED BY '<pw>';
  GRANT ALL PRIVILEGES ON *.* TO 'rig4'@'%' WITH GRANT OPTION;
  GRANT CLONE_ADMIN ON *.* TO 'rig4'@'%';"
```

**The password always comes from `DOLT_REMOTE_PASSWORD`, never the URL.**
Embedding credentials as `http://user:pass@host:50051/db` is silently
ignored — dolt still authenticates as `root`, and the error still names
`root`.

**Flag asymmetry between clone, push, and pull.** `-u`/`--user` on `clone`
supplies the user name; on `push`/`pull`, `-u` is `--set-upstream` and the
user name flag is instead the long form `--user=`. All three verified
sequences:

```
dolt clone -u rig4 http://100.69.137.74:50051/rig4smoke
dolt push --user=rig4 origin main
dolt pull --user=rig4 origin main
```

`-u` is also undocumented at a glance on `clone`: it's supported in
v1.88.1 but doesn't appear in `dolt clone --help`'s SYNOPSIS line, only far
down in OPTIONS — reading the synopsis alone suggests remote auth isn't
supported.

**Error taxonomy — two failures that look alike and mean opposites.** The
discriminator is which user the message names:

- `root` named → credentials never left the client (user name absent, or
  passed via URL userinfo and silently ignored):
  `PermissionDenied: API Authorization Failure: root has not been granted
  CLONE_ADMIN access`. Fix the client invocation, not the grants.
- the real user name → credentials arrived and the password was wrong:
  `Unauthenticated: API Authentication Failure: Access denied for user
  'rig4' (errno 1045, sqlstate 28000)`. Server-side user/password problem.

`CLONE_ADMIN` cannot be tested until authentication passes, because
authorization is evaluated after.

**`dolt commit` prerequisite.** `dolt commit` refuses to run until
`user.name` and `user.email` are set (`dolt config --global --add
user.name`/`user.email`). A fresh client install has neither, so the round
trip dies at the commit step — after the clone has already succeeded —
which reads like a push/remote problem but is purely local identity.

**The fast-forward pull's misleading trailing line.** Forcing a real
fast-forward (rewinding a client past a peer's commit, then pulling it
back) produces output that pairs a real progress line with a misleading
final one:

```
Fast-forward / Updating fbkne6fa..bod0ehid
Everything up-to-date
```

The `Updating <old>..<new>` line is the real fast-forward; the trailing
`Everything up-to-date` refers to a second phase and does **not** mean the
pull was a no-op. Read the `Updating` line, not the last line.

## 5. Checksums of the installed binaries

The dolthub/dolt GitHub release (v1.88.1, published 2026-05-07T22:26:57Z)
publishes no checksums asset, so these are cross-machine consistency
references computed locally after install, not verification against a
publisher-supplied hash.

**Pi hub, linux-arm64:**

| | |
|---|---|
| tarball `dolt-linux-arm64.tar.gz` | 40940687 bytes, sha256 `20692aecab83f907208206de1395e34054ba2653221a9917fdec9aa4d4021621` |
| binary `dolt-linux-arm64/bin/dolt` | sha256 `a932dd1f1591fe896717e53e2c1fd3a26e6f3130da49f9e7a9abbf6848d05f95` |

**Windows client, windows-amd64:**

| | |
|---|---|
| zip `dolt-windows-amd64.zip` | 39924643 bytes, sha256 `bae5a4e726cd78f467cf2e12bfa16adfb03d9a6020fe613cf47e17f0806880e8` |
| binary `dolt-windows-amd64/bin/dolt.exe` | 128685568 bytes, sha256 `ffe20295da9f4386d589713d410b8696e1a86036ec23258ce807bb5429444a38` |

**macOS client, darwin-arm64:**

| | |
|---|---|
| tarball `dolt-darwin-arm64.tar.gz` | 41032655 bytes, sha256 `e486d7fbeaeaff9e13f39c57ea639034f28960d417ed63dc5a505290a0c23966` |
| binary `dolt-darwin-arm64/bin/dolt` | 118448514 bytes, sha256 `2be26259c84c792869988a8a88865aab05ba2129a866b557a3932e70ba8a2f4d` |

The macOS binary's SHA-256 was re-checked after `sudo cp` and matched the
pre-install hash, confirming the privileged copy altered nothing. Gatekeeper
did not quarantine the binary (`gh release download` sets no
`com.apple.quarantine` xattr, unlike a browser download), so the runbook's
`xattr -d` step was unnecessary.

## 6. What rig 4 does NOT cover

**Merge, out of scope by design.** §16's scope line reads
"clone/pull/push/merge," but the exit gate reads only "push/pull round trip
clean," and the gate is the correct reading here. What rig 4 exercises is
the transport layer — remotesapi chunk transfer, SQL-grant auth, ref
updates. A fast-forward pull never invokes the merge algorithm at all: it
detects that local HEAD is an ancestor and moves a pointer, with no cell
comparison, no second-parent commit, and no conflict-table population. So
merge is genuinely not validated by these results, and this PASS does not
transfer to it. This is deliberate, not an omission: `dolt merge` is a
purely local operation against local storage, with no hub, no tailnet, no
remotesapi, and no version skew involved — a merge failure would be a Dolt
product bug in Dolt's most-exercised subsystem, not a deployment or
topology finding, and would not flip the M0 go/no-go. Do not file merge as
a rig-4 gap or re-open rig 4 for it.

The one merge-adjacent question that does remain belongs to M1, not M0:
whether Dolt's conflict *surface* matches what PRD §6.3's resolution policy
assumes. It is filed as an M1 task and is explicitly unverified here.

**Divergent history and conflicts.** Not exercised. All history in rig 4 is
linear.

**Concurrent writers.** Each client wrote exactly one row; no two clients
wrote at the same time.

**Non-fast-forward push rejection.** Two clients pushing divergent history
to the same branch — i.e. the hub rejecting a non-fast-forward push — was
never measured.

**The off-tailnet firewall property.** The nftables drop rule in §3 has
never been probed from off the tailnet. "They would have to reach my
tailnet" is asserted, not measured.

**The rig4 credential is still unrotated.** The rig4 password was pasted
into session transcripts three times during the spike. Rotation was
explicitly deferred for the duration of the spike only, accepted because
the credential guards only the scratch database `rig4smoke` on a hub
reachable solely over the tailnet — the same unverified tailnet-only
premise above is the load-bearing assumption under this deferral. The
credential must be rotated before it guards anything beyond the spike.
