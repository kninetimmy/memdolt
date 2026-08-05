# M0 rig 1 — embedded-driver concurrency soak

PRD §16's first M0 rig: *"Embedded-driver soak: MCP-server-owns-store +
CLI-routes-through-IPC under concurrent load, incl. unclean-kill/stale-LOCK
recovery."* Its exit gate is **"zero data-loss events in the concurrency
soak"**, and §16 is explicit about the consequence of missing it: *"Fail →
project stops; write up findings."*

## 1. Verdict

**PASS.**

Across five configurations, 121,180 writes were acknowledged to a writer.
Every one of them was present, with the value that was written, when the
store was re-opened in a different process afterwards.

| Gate condition | Measured |
|---|---|
| Acknowledged writes absent on re-open | **0** of 121,180 |
| Acknowledged writes whose stored value differed | **0** |
| Acknowledged writes unreadable after unclean-kill recovery | **0** of 33,109 acknowledged before a kill |
| Committed rows a concurrent reader could not see | **0** of 2,376,072 reads |
| Rows in the store no ledger intent accounts for | **0** |
| Stale-LOCK recovery after an unclean kill | recovered, 3 of 3 kill runs |

This verdict is rig 1 only. M0's go/no-go also needs the ONNX rig, the
retrieval golden gate and the hub rig (§16); nothing here speaks to those.

### What this PASS does and does not license

It says: with **one** process owning the Dolt data directory and everyone
else routed through it, the embedded driver did not lose an acknowledged
write under sustained concurrent load, and did not lose one across an
unclean kill of the owning process.

It does not say the store is safe against a second *writer*. §5.2's
single-owner rule is doing real work, and it is doing it because the driver
does not (§9 below). It also does not cover updates, deletes, branches or
merges — the soak inserts (§12).

## 2. What the numbers were measured on

| | |
|---|---|
| Date | 2026-07-30 |
| Repository commit | `c05c0f5` on `orch/issue-5-build-and-run-the-m0-rig-1-concurrency-s` |
| OS | Windows 11 Pro, build 10.0.26200.8875, NTFS, 16 logical CPUs |
| Go | go1.26.5 windows/amd64, `gc` |
| C toolchain | MinGW-W64 x86_64-ucrt-posix-seh (winlibs r3) GCC 16.1.0 |
| Build settings | `CGO_ENABLED=1`, `-tags soak,gms_pure_go`, `GOAMD64=v1` |
| `github.com/dolthub/driver` | **v1.88.1** |
| `github.com/dolthub/dolt/go` | v0.40.5-0.20260507221239-14b38e279fc6 |
| `github.com/dolthub/go-mysql-server` | v0.20.1-0.20260507202550-43d6daf5958b |
| `github.com/dolthub/gozstd` | v0.0.0-20240423170813-23a2903bca63 |
| `github.com/dolthub/go-icu-regex` | v0.0.0-20260412212219-49724d547866 |

**One platform only.** Everything below was measured on Windows/amd64.
Windows is the harshest of the three targets for this rig — it is where
`LockFileEx` byte-range locks are mandatory rather than advisory, where
`TerminateProcess` gives a killed process no unwinding at all, and where
ephemeral ports run out first — but a Linux and macOS run of the same rig
is still outstanding, and the untagged CI suite is not a substitute for it.

## 3. Why the zeros can be believed

A soak that reports "zero data loss" because it never looked is worse than
no soak. Four things make these zeros checkable.

**The ledger is independent of the store.** Each writer appends its intent
to its own append-only file and flushes it to the operating system *before*
asking the store to do anything, then appends what it was told afterwards.
Reconciliation compares that file against rows read from a store re-opened
in a different process once every writer is gone. Asking the store what it
holds and comparing that against what the store says it holds would answer
nothing. Payloads carry a SHA-256 digest, so a value that comes back
changed is detected as changed rather than compared only against itself.

**The gate's definition was fixed before any measurement**, in code, in
`tests/soak/summary.go`'s `decide`. A data-loss event is: an acknowledged
write absent on re-open; an acknowledged write whose stored value differs;
an acknowledged write unreadable after stale-lock recovery; a committed
write a concurrent reader could not see or saw wrongly; or a row no ledger
intent accounts for. Two things are deliberately *not* counted, and are
reported separately as findings: a write reported to its writer as failed
that is present anyway (the store gained data, it did not lose it), and a
write whose answer never arrived (nothing promised the writer anything, so
scoring its presence either way would be inventing a promise).

**There is a negative control.** `TestReconciliationDetectsWhatItClaimsTo`
feeds the reconciler a ledger and a store that disagree in every way it
claims to detect — a lost write, a corrupted value, a phantom, an
unledgered row, both indeterminate cases, both killed-mid-write cases — and
requires it to count each one and return FAIL. Its companion requires a
store that kept everything to return PASS. Without those, "0 lost" would
mean only that nothing was looked for.

**The two records agree on the total, not just per key.** In every clean
run, `writes.attempted == writes.committed == store.rowsInProbeTable`
exactly — 47,833 in the five-minute run. A ledger reader that silently
dropped records, or a read-back that silently dropped rows, would have to
drop precisely the same ones to keep those three equal.

## 4. Configurations run

Every participant is a real operating-system process. The owner holds the
store lock and the pidfile for its whole life, runs its own writers and
readers in process, and serves everyone else on the loopback endpoint;
each client process dials the owner once and routes every operation
through it (`internal/storeipc` over `internal/ipc`).

| Run | Duration | Owner w/r | Clients × w/r | Writers total | Acknowledged | Lost | Commits/s | Store at end |
|---|---|---|---|---|---|---|---|---|
| A — short, CI-adjacent | 15 s | 2 / 2 | 2 × 2/2 | 6 | 4,551 | 0 | 303 | 65.0 MB |
| B1 — sustained | 300 s | 4 / 4 | 4 × 3/3 | 16 | 47,833 | 0 | 159 | 801.6 MB |
| B2 — sustained, killed at 120 s | 180 s | 4 / 4 | 4 × 3/3 | 16 | 22,332 | 0 | 186 | 374.3 MB |
| C — burst | 60 s | 8 / 4 | 6 × 4/2 | 32 | 17,152 | 0 | 286 | 295.1 MB |
| C — burst, killed at 40 s | 60 s | 8 / 4 | 6 × 4/2 | 32 | 8,973 | 0 | 224 | 144.3 MB |
| D — B1's shape, one fifth as long | 60 s | 4 / 4 | 4 × 3/3 | 16 | 17,541 | 0 | 292 | 300.5 MB |
| A — unclean kill | 15 s | 2 / 2 | 2 × 2/2 | 6 | 1,804 | 0 | 301 | 23.1 MB |
| A — foreign opener | ~4 s | 1 / 1 | — | 1 | 994 | 0 | — | 6.5 MB |

Writers ran with no pause between writes (`-soak.write-interval=0`), so
each figure is the store's own ceiling at that concurrency, not a paced
workload. Readers paused 2 ms between reads.

Totals over all eight scenario runs: **162,841 writes attempted, 121,180
acknowledged, 0 lost, 0 corrupted, 0 phantom, 0 unledgered; 2,376,072 reads
attempted, 0 that could not see a committed row, 0 with a wrong value, 0
observations of the row count going backwards.** The 41,661 writes that
were not acknowledged, and the 432,656 reads that failed, are all from the
three kill runs, after the owner was dead (§10).

## 5. The sustained run, verbatim

Five minutes, 16 concurrent writers and 16 concurrent readers across five
processes.

```json
{
  "rig": "m0-rig1",
  "scenario": "concurrency",
  "startedAt": "2026-07-30T09:52:58.3822569Z",
  "finishedAt": "2026-07-30T09:58:00.2158673Z",
  "platform": {
    "os": "windows",
    "arch": "amd64",
    "numCpu": 16,
    "goVersion": "go1.26.5",
    "compiler": "gc",
    "modules": {
      "github.com/dolthub/aws-sdk-go-ini-parser": "v0.0.0-20250305001723-2821c37f6c12",
      "github.com/dolthub/dolt/go": "v0.40.5-0.20260507221239-14b38e279fc6",
      "github.com/dolthub/driver": "v1.88.1",
      "github.com/dolthub/eventsapi_schema": "v0.0.0-20260310172945-37a9265ade69",
      "github.com/dolthub/flatbuffers/v23": "v23.3.3-dh.2",
      "github.com/dolthub/fslock": "v0.0.3",
      "github.com/dolthub/go-icu-regex": "v0.0.0-20260412212219-49724d547866",
      "github.com/dolthub/go-mysql-server": "v0.20.1-0.20260507202550-43d6daf5958b",
      "github.com/dolthub/gozstd": "v0.0.0-20240423170813-23a2903bca63",
      "github.com/dolthub/ishell": "v0.0.0-20260414231531-5f031e3e9037",
      "github.com/dolthub/jsonpath": "v0.0.2-0.20240227200619-19675ab05c71",
      "github.com/dolthub/vitess": "v0.0.0-20260505163811-77e5224be390"
    },
    "modulesSource": "C:\\Users\\Kninetimmy\\Memdolt\\.orchestrator\\worktrees\\issue-5\\go.mod",
    "buildSettings": {
      "-compiler": "gc",
      "-tags": "soak,gms_pure_go",
      "CGO_ENABLED": "1",
      "GOAMD64": "v1",
      "GOARCH": "amd64",
      "GOOS": "windows"
    }
  },
  "config": {
    "duration": 300000000000,
    "killAfter": 0,
    "ownerWriters": 4,
    "ownerReaders": 4,
    "clientProcesses": 4,
    "clientWritersEach": 3,
    "clientReadersEach": 3,
    "writeInterval": 0,
    "readInterval": 2000000,
    "opTimeout": 120000000000
  },
  "writes": {
    "attempted": 47833,
    "committed": 47833,
    "refused": 0,
    "indeterminate": 0,
    "noResultRecorded": 0,
    "lost": 0,
    "corrupted": 0,
    "phantomPresentAfterRefusal": 0,
    "indeterminatePresent": 0,
    "indeterminateAbsent": 0,
    "noResultPresent": 0,
    "noResultAbsent": 0,
    "unledgeredRowsInStore": 0
  },
  "reads": {
    "attempted": 801134,
    "failed": 0,
    "missingCommittedRow": 0,
    "mismatchedPayload": 0,
    "rowCountRegressions": 0
  },
  "store": {
    "rowsInProbeTable": 47833,
    "doltCommits": 47835,
    "doltCommitsByCommitter": {
      "agent:claude-code": 11976,
      "user": 35859
    },
    "dataDirBytes": 801615991,
    "lockStateAfterRun": "none",
    "pidfileStateAfterRun": "none",
    "ipcProbeAfterRun": "no-owner"
  },
  "errorsByClass": {
    "counts": {},
    "samples": {}
  },
  "recovery": {
    "performed": false,
    "reopenSucceeded": false,
    "loudWarningLogged": false,
    "warningNamedStalePid": false,
    "staleRecordClearedAfterReopen": false,
    "pidfileRecoveredOnNextListen": false,
    "recycledPidRecovered": false,
    "committedBeforeKill": 0,
    "presentAfterRecovery": 0,
    "missingAfterRecovery": 0,
    "outcome": "not attempted in this scenario"
  },
  "roles": [
    {
      "role": "client",
      "id": "client0",
      "pid": 20268,
      "cleanEnd": true,
      "writesAttempted": 8964,
      "writesCommitted": 8964,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 149868,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405478536292600
    },
    {
      "role": "client",
      "id": "client1",
      "pid": 7924,
      "cleanEnd": true,
      "writesAttempted": 8967,
      "writesCommitted": 8967,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 149908,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405478592963000
    },
    {
      "role": "client",
      "id": "client2",
      "pid": 17832,
      "cleanEnd": true,
      "writesAttempted": 8965,
      "writesCommitted": 8965,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 149950,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405478565149900
    },
    {
      "role": "client",
      "id": "client3",
      "pid": 16100,
      "cleanEnd": true,
      "writesAttempted": 8963,
      "writesCommitted": 8963,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 149919,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405478528954500
    },
    {
      "role": "owner",
      "id": "owner",
      "pid": 13668,
      "cleanEnd": true,
      "writesAttempted": 11974,
      "writesCommitted": 11974,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 201489,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405478619610700
    }
  ],
  "dataLossEvents": 0,
  "verdict": "PASS",
  "verdictReasons": [
    "zero data-loss events under the definition recorded in tests/soak/summary.go"
  ],
  "findings": null
}
```

## 6. Unclean kill and stale-LOCK recovery, verbatim

The owner is killed with `os.Process.Kill` — `TerminateProcess` on Windows,
`SIGKILL` on Unix. It gets no signal, no deferred close, no chance to
release the lock or remove the pidfile. Client processes keep running for
the remaining 60 s and are still trying to write.

```json
{
  "rig": "m0-rig1",
  "scenario": "unclean-kill",
  "startedAt": "2026-07-30T09:58:04.9013741Z",
  "finishedAt": "2026-07-30T10:01:06.1504524Z",
  "platform": {
    "os": "windows",
    "arch": "amd64",
    "numCpu": 16,
    "goVersion": "go1.26.5",
    "compiler": "gc",
    "modules": {
      "github.com/dolthub/aws-sdk-go-ini-parser": "v0.0.0-20250305001723-2821c37f6c12",
      "github.com/dolthub/dolt/go": "v0.40.5-0.20260507221239-14b38e279fc6",
      "github.com/dolthub/driver": "v1.88.1",
      "github.com/dolthub/eventsapi_schema": "v0.0.0-20260310172945-37a9265ade69",
      "github.com/dolthub/flatbuffers/v23": "v23.3.3-dh.2",
      "github.com/dolthub/fslock": "v0.0.3",
      "github.com/dolthub/go-icu-regex": "v0.0.0-20260412212219-49724d547866",
      "github.com/dolthub/go-mysql-server": "v0.20.1-0.20260507202550-43d6daf5958b",
      "github.com/dolthub/gozstd": "v0.0.0-20240423170813-23a2903bca63",
      "github.com/dolthub/ishell": "v0.0.0-20260414231531-5f031e3e9037",
      "github.com/dolthub/jsonpath": "v0.0.2-0.20240227200619-19675ab05c71",
      "github.com/dolthub/vitess": "v0.0.0-20260505163811-77e5224be390"
    },
    "modulesSource": "C:\\Users\\Kninetimmy\\Memdolt\\.orchestrator\\worktrees\\issue-5\\go.mod",
    "buildSettings": {
      "-compiler": "gc",
      "-tags": "soak,gms_pure_go",
      "CGO_ENABLED": "1",
      "GOAMD64": "v1",
      "GOARCH": "amd64",
      "GOOS": "windows"
    }
  },
  "config": {
    "duration": 180000000000,
    "killAfter": 120000000000,
    "ownerWriters": 4,
    "ownerReaders": 4,
    "clientProcesses": 4,
    "clientWritersEach": 3,
    "clientReadersEach": 3,
    "writeInterval": 0,
    "readInterval": 2000000,
    "opTimeout": 120000000000
  },
  "writes": {
    "attempted": 44940,
    "committed": 22332,
    "refused": 0,
    "indeterminate": 22604,
    "noResultRecorded": 4,
    "lost": 0,
    "corrupted": 0,
    "phantomPresentAfterRefusal": 0,
    "indeterminatePresent": 1,
    "indeterminateAbsent": 22603,
    "noResultPresent": 0,
    "noResultAbsent": 4,
    "unledgeredRowsInStore": 0
  },
  "reads": {
    "attempted": 684675,
    "failed": 310765,
    "missingCommittedRow": 0,
    "mismatchedPayload": 0,
    "rowCountRegressions": 0
  },
  "store": {
    "rowsInProbeTable": 22333,
    "doltCommits": 22335,
    "doltCommitsByCommitter": {
      "agent:claude-code": 5595,
      "user": 16740
    },
    "dataDirBytes": 374294153,
    "lockStateAfterRun": "stale",
    "pidfileStateAfterRun": "stale",
    "ipcProbeAfterRun": "owner-dead"
  },
  "errorsByClass": {
    "counts": {
      "transport_connection_refused": 333344,
      "transport_connection_reset": 23,
      "transport_dial": 2
    },
    "samples": {
      "transport_connection_refused": [
        "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it.",
        "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it."
      ],
      "transport_connection_reset": [
        "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56066-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
        "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56056-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
        "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56067-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
        "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56052-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
        "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56068-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host."
      ],
      "transport_dial": [
        "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: bind: An operation on a socket could not be performed because the system lacked sufficient buffer space or because a queue was full."
      ]
    }
  },
  "recovery": {
    "performed": true,
    "trigger": "os.Process.Kill (SIGKILL on Unix, TerminateProcess on Windows)",
    "ownerPid": 17256,
    "ipcProbeBeforeKill": "owner-live",
    "lockStateAfterKill": "stale",
    "pidfileStateAfterKill": "stale",
    "ipcProbeAfterKill": "owner-dead",
    "stalePidInLockRecord": 17256,
    "ownerPidLivenessBeforeReap": "dead",
    "ownerPidLivenessAfterReap": "dead",
    "reopenSucceeded": true,
    "loudWarningLogged": true,
    "warningNamedStalePid": true,
    "warningLine": "time=2026-07-30T06:01:05.042-04:00 level=WARN msg=\"memdolt: removed a stale lock left by a dead owner\" path=C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\memdolt-soak2083837878\\repo\\.memdolt\\LOCK stale_pid=17256 stale_owner_id=252ff7dbc6a2565dca6f3001958f3e28 stale_host=DESKTOP-LCJ0F6H stale_acquired_at=2026-07-30T09:58:04Z stale_pid_liveness=dead stale_executable=C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\go-build3574327917\\b001\\soak.test.exe",
    "staleRecordClearedAfterReopen": true,
    "pidfileRecoveredOnNextListen": true,
    "pidfileWarningLine": "time=2026-07-30T06:01:05.173-04:00 level=WARN msg=\"memdolt: removed a stale lock left by a dead owner\" path=C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\memdolt-soak2083837878\\repo\\.memdolt\\server.pid stale_pid=17256 stale_owner_id=0276fdd29807f2be4a984d5b60e465e4 stale_host=DESKTOP-LCJ0F6H stale_acquired_at=2026-07-30T09:58:04Z stale_pid_liveness=dead stale_executable=C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\go-build3574327917\\b001\\soak.test.exe",
    "recycledPidProbe": "time=2026-07-30T06:01:05.196-04:00 level=WARN msg=\"memdolt: removed a stale lock left by a dead owner\" path=C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\memdolt-soak209963691\\recycled\\.memdolt\\LOCK stale_pid=9852 stale_owner_id=recycled-pid-probe stale_host=DESKTOP-LCJ0F6H stale_acquired_at=2026-07-30T09:01:05Z stale_pid_liveness=live note=\"pid is running but does not hold the lock; it was almost certainly recycled\"",
    "recycledPidRecovered": true,
    "committedBeforeKill": 22332,
    "presentAfterRecovery": 22332,
    "missingAfterRecovery": 0,
    "outcome": "recovered"
  },
  "roles": [
    {
      "role": "client",
      "id": "client0",
      "pid": 21580,
      "cleanEnd": true,
      "writesAttempted": 9837,
      "writesCommitted": 4183,
      "writesRefused": 0,
      "writesIndeterminate": 5654,
      "readsAttempted": 147823,
      "readsFailed": 77834,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {
          "transport_connection_refused": 83481,
          "transport_connection_reset": 6,
          "transport_dial": 1
        },
        "samples": {
          "transport_connection_refused": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it."
          ],
          "transport_connection_reset": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56066-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56056-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56067-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56052-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56068-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host."
          ],
          "transport_dial": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: bind: An operation on a socket could not be performed because the system lacked sufficient buffer space or because a queue was full."
          ]
        }
      },
      "updatedAtUnixNs": 1785405665035353200
    },
    {
      "role": "client",
      "id": "client1",
      "pid": 18564,
      "cleanEnd": true,
      "writesAttempted": 9835,
      "writesCommitted": 4186,
      "writesRefused": 0,
      "writesIndeterminate": 5649,
      "readsAttempted": 147223,
      "readsFailed": 77253,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {
          "transport_connection_refused": 82896,
          "transport_connection_reset": 5,
          "transport_dial": 1
        },
        "samples": {
          "transport_connection_refused": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it."
          ],
          "transport_connection_reset": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56053-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56054-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56073-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56059-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56074-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host."
          ],
          "transport_dial": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: bind: An operation on a socket could not be performed because the system lacked sufficient buffer space or because a queue was full."
          ]
        }
      },
      "updatedAtUnixNs": 1785405665035353200
    },
    {
      "role": "client",
      "id": "client2",
      "pid": 8888,
      "cleanEnd": true,
      "writesAttempted": 9838,
      "writesCommitted": 4185,
      "writesRefused": 0,
      "writesIndeterminate": 5653,
      "readsAttempted": 148166,
      "readsFailed": 78182,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {
          "transport_connection_refused": 83829,
          "transport_connection_reset": 6
        },
        "samples": {
          "transport_connection_refused": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it."
          ],
          "transport_connection_reset": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56051-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56060-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56061-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56064-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56057-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host."
          ]
        }
      },
      "updatedAtUnixNs": 1785405665032059000
    },
    {
      "role": "client",
      "id": "client3",
      "pid": 24196,
      "cleanEnd": true,
      "writesAttempted": 9833,
      "writesCommitted": 4185,
      "writesRefused": 0,
      "writesIndeterminate": 5648,
      "readsAttempted": 147459,
      "readsFailed": 77496,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {
          "transport_connection_refused": 83138,
          "transport_connection_reset": 6
        },
        "samples": {
          "transport_connection_refused": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": dial tcp 127.0.0.1:56050: connectex: No connection could be made because the target machine actively refused it."
          ],
          "transport_connection_reset": [
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56069-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56071-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: query: request /v0/store/query: Post \"http://127.0.0.1:56050/v0/store/query\": read tcp 127.0.0.1:56070-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56058-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host.",
            "storeipc: commit: request /v0/store/commit: Post \"http://127.0.0.1:56050/v0/store/commit\": read tcp 127.0.0.1:56055-\u003e127.0.0.1:56050: wsarecv: An existing connection was forcibly closed by the remote host."
          ]
        }
      },
      "updatedAtUnixNs": 1785405665028954000
    },
    {
      "role": "owner",
      "id": "owner",
      "pid": 17256,
      "cleanEnd": false,
      "writesAttempted": 5593,
      "writesCommitted": 5593,
      "writesRefused": 0,
      "writesIndeterminate": 0,
      "readsAttempted": 94004,
      "readsFailed": 0,
      "readsMissingCommittedRow": 0,
      "readsMismatchedPayload": 0,
      "rowCountRegressions": 0,
      "errors": {
        "counts": {},
        "samples": {}
      },
      "updatedAtUnixNs": 1785405604965163700
    }
  ],
  "dataLossEvents": 0,
  "verdict": "PASS",
  "verdictReasons": [
    "zero data-loss events under the definition recorded in tests/soak/summary.go"
  ],
  "findings": [
    "1 write(s) whose answer was lost are present in the store, and 22603 are absent; neither is scored",
    "error classes observed: transport_connection_refused, transport_connection_reset, transport_dial"
  ]
}
```

The recovery sequence, measured, in order:

1. Before the kill, `ipc.Probe` reported `owner-live`.
2. After the kill, the store lock is `stale` and the pidfile is `stale` —
   an ownership record present, the advisory lock free — and `ipc.Probe`
   reports `owner-dead`.
3. Opening the store logs at WARN, naming the dead owner's pid:
   `memdolt: removed a stale lock left by a dead owner … stale_pid=17256
   stale_owner_id=252ff7db… stale_pid_liveness=dead`.
4. The dead owner's record is gone from the lock file afterwards.
5. The next `ipc.Listen` recovers the orphaned pidfile the same way, with
   its own WARN naming the same pid.
6. **22,332 writes were acknowledged before the kill. 22,332 were present
   afterwards. 0 missing.**

Four writes in that run had an intent recorded and no result: the owner's
four in-process writers, each killed between recording what it was about to
do and being told what happened. One of those four rows is in the store and
three are not — exactly the shape a kill at an arbitrary instant should
produce. None is scored either way.

## 7. PRD §5.2.3's `[verify]` marker, resolved

The marker read: *"Startup recovery: on open, if a LOCK file exists and its
owning pid is dead, remove it and log loudly. **[verify]** exact stale-lock
detection ergonomics — M0 spike item."*

**Resolved: the lock decides, the pid describes.**

*What the detection guarantees.* The kernel releases an advisory lock when
the holding process exits, however it exits. A free lock is therefore proof
that the process which wrote the record is gone — measured across three
unclean kills, where the lock was free and the record was still present
every time. That is a positive proof, not a heuristic, and it is what
`singleowner.Inspect` reports as `stale`.

*What it does not guarantee, measured.* A pid check cannot do the same job,
and the rig demonstrates it rather than asserting it. `measureRecycledPID`
writes a lock record naming a pid that is unambiguously alive — the test
process's own — and never held that lock. Recovery proceeds correctly and
says so:

```
level=WARN msg="memdolt: removed a stale lock left by a dead owner"
  stale_pid=9852 stale_owner_id=recycled-pid-probe stale_pid_liveness=live
  note="pid is running but does not hold the lock; it was almost certainly recycled"
```

A pid-driven implementation would have refused to recover there, and would
have left the operator with a store nothing could open. Windows recycles
pids from a table aggressively, so this is not a thought experiment.

*What the pid is still for.* It names the previous owner in the loud log
and in the error a second process receives, which is what an operator
actually needs. It is recorded as corroborating detail and is never
consulted to decide staleness.

*Two further measured details behind the ergonomics.* First, the liveness
of a killed pid is not a stable signal even in the easy direction: sampled
immediately after `Kill` and before the process was reaped, Windows already
reported `dead`, because `GetExitCodeProcess` sees the exit code through
the handle the parent still holds. It happens to agree with the lock here;
it is not guaranteed to, and nothing depends on it. Second, the record is
cleared **in place** rather than the file unlinked. Unlinking a file while
holding a lock on it lets two processes lock two different inodes under one
name and both believe they own the store. An empty lock file left on disk
costs nothing.

The remaining, unremovable limitation: an advisory lock binds only
processes that take it, and depends on the filesystem honouring it. See §9
and §12.

## 8. Commit provenance under load

PRD §3.1 makes commit metadata load-bearing. Measured in the five-minute
run, with the owner attributing its writes to `agent:claude-code` and the
four client processes attributing theirs to `user`:

- `dolt_log` holds 47,835 commits: the 47,833 writes plus two made before
  the load started (Dolt's own initial commit, and the probe table's DDL).
- Attributed to `agent:claude-code`: 11,976 = the owner's 11,974 writes
  plus those two.
- Attributed to `user`: 35,859 = exactly 8,964 + 8,967 + 8,965 + 8,963,
  the four client processes' acknowledged writes.

Attribution is exact under 16-way concurrency, and it survives the trip
over IPC — a client's author travels in the request and is what lands in
`dolt_log`. Issue #4's finding that `DOLT_COMMIT` attributes to the engine
user `root` unless `--author` is passed explicitly is confirmed by
construction: `localdolt` passes it on every commit, and nothing in 121,180
commits landed as `root`.

## 9. The foreign opener — PRD §17 R1, corrected

§5.2 and R1 both expected a second process on the same data directory to
hit "database is locked". Measured, verbatim from the summary:

```json
{
  "secondMemdoltOpenError": "localdolt: take store lock: C:\\Users\\Kninetimmy\\AppData\\Local\\Temp\\memdolt-soak3080281999\\repo\\.memdolt\\LOCK: lock file is held by a live process (pid 12076 on DESKTOP-LCJ0F6H since 2026-07-30T09:52:18Z)",
  "secondMemdoltOpenMatchedErrLocked": true,
  "secondMemdoltOpenMillis": 106,
  "driverOpenError": "",
  "driverPingError": "",
  "driverReadError": "",
  "driverReadRows": 990,
  "driverWriteError": "Error 1105: cannot update manifest: database is read only",
  "driverCommitError": "",
  "stepOutcomes": {
    "commit": "not attempted: an earlier step failed",
    "open": "ok",
    "ping": "ok",
    "read": "ok",
    "write": "dolt_read_only"
  },
  "ownerStillCommitsAfterwards": true
}
```

Read that as a sequence. A process that does **not** take memdolt's lock —
a `dolt` CLI, a `dolt sql-server`, anything that has never heard of
memdolt — opens the data directory successfully, answers a ping, and reads
990 rows correctly. It has been silently downgraded to read-only, and finds
out only at its first write:

```
Error 1105: cannot update manifest: database is read only
```

There is no error at open, no error at ping, and no error at read. A health
check cannot see this. A writer believes it is writing until it commits.
That is materially worse than the failure the risk register anticipated,
and it is the reason §5.2's single-owner lock exists rather than being belt
and braces: a second **memdolt** process is refused in **106 ms** with an
error that matches `store.ErrLocked` and names the holder, before the
driver is involved at all.

The owner was still able to commit afterwards (`ownerStillCommits: true`),
and the run's 994 acknowledged writes all reconciled — the foreign opener
damaged nothing. But it could read everything, and nothing stopped it.

## 10. Every error class observed

Counts are totals across the final measurement pass.

| Class | Count | Where | What it is | Explained? |
|---|---|---|---|---|
| `transport_connection_refused` | 474,234 | the three kill runs only | Client processes still trying to reach the endpoint of a process that no longer exists. The rig's clients retry without backoff for the rest of the run. | Yes |
| `transport_connection_reset` | 67 | the three kill runs only | Requests that were in flight at the instant the owner was killed; the socket died mid-answer. | Yes |
| `transport_dial` (`WSAENOBUFS`) | 2 | B2 kill run | *"the system lacked sufficient buffer space or because a queue was full"* — 333,344 failed dials in 60 s exhausts socket resources. A consequence of the retry-free hammering above, not of the store. | Yes |
| `dolt_read_only` | 1 | foreign-opener run | `cannot update manifest: database is read only`, §9. | Yes |
| `transport_ephemeral_port_exhaustion` (`WSAEADDRINUSE`) | 268,912 | **a superseded run only** | §11, F2. Zero occurrences after the fix. | Yes |

**No error class was observed that this write-up cannot account for.**

What is conspicuously *absent* is as informative as what is present: across
121,180 concurrent commits there were **zero** engine-level failures — no
`dolt_conflict`, no `sql_duplicate_key`, no `sql_deadlock`, no
`dolt_nothing_to_commit`, no serialization failures, not one refused write
in any run. The classifier has cases for all of them (`tests/soak/stats.go`)
and none fired. Every non-acknowledged write in the whole pass failed
because the owner was dead, never because the store said no.

## 11. Findings

**F1 — The silent read-only downgrade (§9).** Folded into PRD §5.2 and §17
R1. The residual risk cannot be removed by memdolt's lock: it binds only
processes that take it. `doctor` should detect a foreign process on the
data directory, and the docs must forbid pointing one at it.

**F2 — A naive loopback client exhausts Windows' ephemeral ports.** The
first five-minute run produced **268,912** failures with
`WSAEADDRINUSE` — *"Only one usage of each socket address is normally
permitted"* — and 33,044 writes that never reached the store. Cause:
`http.DefaultTransport` keeps two idle connections per host, so a client
with more than two requests in flight gets a fresh TCP connection per
request; each lands in `TIME_WAIT` for ~120 s; Windows' dynamic range is
16,384 ports (49152–65535), so anything above ~136 new connections per
second exhausts it.

*Whose defect it was:* the product's, not the rig's. The rig dials once per
client process and shares that client across its goroutines, which is what
a well-behaved caller does; the fault was in the route client in
`internal/ipc`, added in this issue, which built its `http.Client` with the
zero value. Fixed by giving it a pool sized for the traffic and by draining
response bodies before closing them so connections can be reused. After the
fix, the same configuration ran five minutes with **zero** errors of any
class. Both numbers are recorded here because "a naive HTTP client on a
loopback endpoint dies on Windows before it dies anywhere else" is an
operational fact the real CLI-over-IPC path needs to keep.

*It was never a data-loss event.* All 33,044 affected writes were reported
to their writers as indeterminate, and reconciliation found all 33,044
absent from the store. Nothing was acknowledged and lost.

**F3 — Writes over IPC are at-least-once, and the rig caught it happening.**
In two of the three kill runs, exactly one write per run whose answer never
arrived **was present in the store**: the owner committed it and was killed
before the response reached the client. The client cannot know. This is not
data loss, and is not scored — but it means a client that retries after a
lost answer can duplicate a write. PRD §6.1's ULID primary keys already
make retries idempotent by construction; that property is now load-bearing
rather than merely tidy, and M1's IPC write path must not weaken it.

**F4 — The IPC client has no backoff, and spins on a dead owner.** After
the kill, the rig's clients produced 474,234 connection-refused failures in
about a minute, which is what generated F2's cousin `WSAENOBUFS`. A real
CLI must re-probe on transport failure — `ipc.Probe` already distinguishes
`owner-dead` from `owner-live` — and either take the store itself or fail,
rather than retrying blindly. Nothing in M0 does this yet because nothing
in M0 routes anything but the rig.

*Acted on, after this rig ran.* The finding above stands as measured and is
kept unedited; what its last two sentences describe is no longer the
product's behaviour.

| | Behaviour on a transport failure |
|---|---|
| Before | `ipc.Client` made exactly one transport attempt per operation and returned the failure; `internal/storeipc` passed it through. Recovery, if any, was the caller's to write — and the rig's clients wrote the worst one there is. |
| After | `Client.PostJSON` re-probes the pidfile through `ipc.Probe`, at most **5** times, spaced **0 / 100 / 200 / 400 / 400 ms**, and ends in one of three ways: it adopts the live owner it finds — including a replacement listening on a **different port with a different token**, which the caller's next operation reaches on the same client, nothing rebuilt by hand; or it returns an error matching `ipc.ErrNoLiveOwner`, which is this finding's *"take the store itself"* (PRD §5.2: no live server means the CLI opens the store directly); or it fails after the bound. |

What did **not** change, and is worth stating because each is easy to
assume:

- **The failed operation is never resubmitted.** Only the connection is
  re-established. F3 is the reason: a write whose answer was lost may
  already have been applied, and retry-safety waits for M1's client-minted
  ULIDs. The `*ipc.StatusError`-versus-transport-failure split callers use
  to tell *"did not happen"* from *"unknown"* is untouched.
- **The rig's own clients still redial in an unpaced loop** — the pacing
  lives in `internal/ipc`, not in `tests/soak/roles.go`. The counts in §10
  measure that loop; rewriting it would make them unreproducible.
- **The pacing binds `*ipc.Client` alone, not every waiting decision in
  memdolt.** It is not the Dolt driver's `BackOff`, which stays disabled
  (§5.2.2 design response 2, Q1 below), and it is not
  `singleowner.Acquire`, which still refuses a second memdolt process in
  ~106 ms rather than waiting. Three different symbols at three layers; a
  bounded wait was added to one of them.
- **The `Health` method does not reconnect**, though it hangs off the same
  `*ipc.Client`: `Probe` builds a client to make its health request, so
  recovering there would re-enter `Probe`. The restriction is `PostJSON`'s
  alone, not a property of every method on the type.

**F5 — Write throughput falls by more than half as history grows.**
Measured at fixed concurrency (16 writers, identical configuration, the
only difference being how long it ran):

| | Duration | Commits | Rate | Store at end |
|---|---|---|---|---|
| D | 60 s | 17,541 | 292 /s | 300.5 MB |
| B1 | 300 s | 47,833 | 159 /s | 801.6 MB |

B1's first minute is D. Its remaining four minutes produced 30,292 commits
— **126/s, 43% of the opening rate** — as the store grew from empty to
800 MB. Concurrency is not the variable: 32 writers (C) sustained 286/s at
a comparable store size, and 6 writers (A) sustained 303/s at a much
smaller one.

At memdolt's actual write volume this is irrelevant — a memory corpus is
10³–10⁴ rows (§8.1), reached in about thirty seconds here. It matters
because it says the ceiling is a function of history size, so PRD R8's
*"~300 w/s"* **[L]** should read *~300 commits/s on an empty store,
decaying with history*, and R4's retention and `gc` machinery has a second
justification beyond disk.

**F6 — History costs ~16.8 KB per commit.** 801,615,991 bytes for 47,835
commits, each a single-row insert into a five-column table. Consistent
across runs (14.3–17.2 KB). PRD §13.3's rule of thumb is ~4 KB per
update-transaction per indexed column; at one row and one index this is
about four times that, so the PRD's sizing guidance is optimistic by
roughly that factor for tiny commits. Note-batching (§3.1) is not a nicety.

**F7 — `-tags` on the command line replaces `GOFLAGS`', it does not add.**
`go test -tags soak ./tests/soak/...` — the invocation this issue's
acceptance criteria name — **fails**, because it drops the mandatory
`gms_pure_go` and the build then needs ICU headers. The working invocation
is `go test -tags soak,gms_pure_go ./tests/soak/...`. Recorded in §14 of
the PRD and in CLAUDE.md; §14 below has the verbatim failure.

## 12. What could not be measured, and what was assumed

**Process death, not machine death.** The rig kills processes. The ledger's
intent record is flushed with `File.Sync` before each write, so it survives
a killed process, but nothing here says anything about power loss or a
kernel panic — Dolt's own durability across those is unmeasured. A rig that
claimed otherwise would need hardware or a VM it can cut off.

**One platform.** Windows/amd64 only (§2). The Linux and macOS behaviour of
`flock`, of `SIGKILL` versus `TerminateProcess`, and of the port exhaustion
in F2 is inferred from the code, not measured.

**Inserts only.** The soak inserts distinct rows with distinct primary
keys. It does not update the same row from two writers, delete, branch, or
merge. That is a deliberate limit: a rig whose accounting can be proved
correct is worth more than a broader one whose accounting cannot, and
verifying last-writer-wins under concurrent updates needs a global order the
rig has no way to establish. **The consequence is that PRD §6.3's conflict
machinery is entirely unexercised**, and M1 — which lands proposal branches
and merges — must not treat this PASS as covering it.

**Cross-process read visibility is checked at aggregate granularity only.**
A reader verifies, key by key, the writes its own process was told were
committed; the acknowledgement always happens before the read, so a miss
would be a real miss. It does not check another process's keys, because
knowing which of those are committed would need coordination the rig
deliberately does not have. What does span processes is the row count,
which every reader samples and which must never decrease: 0 regressions in
2,376,072 reads.

**Assumed: the ledger's own writes are ordered and atomic.** Each writer
owns its file, appends whole lines, and flushes. A torn tail is detected
and accounted for rather than dropped, and no run produced one.

**Not measured: whether the untagged suite stays green on Linux and macOS.**
That is CI's job on the pull request, not this rig's.

## 13. Open questions for the humans

**Q1 — §5.2.2 says to rely on the driver's retry. The implementation
deliberately does not.** `localdolt` leaves the driver's `BackOff` off
(reviewed and upheld in issue #4: waiting is the opposite of what the
single-owner rule wants from a second memdolt process), and this rig
measures a second memdolt process being refused in 106 ms by memdolt's own
lock — so driver retry never runs. R1's mitigation cell has been updated to
describe what actually happens; **§5.2.2's bullet has deliberately not been
edited**, because changing a design response is not this issue's to make.
It should either be corrected or the code should change.

**Q2 — Should a foreign opener be detected rather than merely documented?**
§9 shows a `dolt` CLI can read the store, and can be pointed at it while
the MCP server is running, discovering only at its first write that it was
never going to write. `doctor` could detect one (Dolt's own `LOCK` file
exists, even though it carries no pid); no PRD requirement currently says
it must.

**Q3 — F5's throughput decay against topology B.** R8 defers re-measurement
to "if topology B is pursued". The decay measured here is a property of the
local clone, not of the hub, and shows up in both.

## 14. Reproducing this

```sh
export CGO_ENABLED=1
export GOFLAGS=-tags=gms_pure_go

# Short, CI-adjacent: every scenario at the default shape (~40 s).
go test -tags soak,gms_pure_go -count=1 -v ./tests/soak/... -soak.duration=15s

# B1 — sustained, five minutes, 16 writers.
go test -tags soak,gms_pure_go -count=1 -v -timeout 30m ./tests/soak/... \
  -run TestConcurrencySoak \
  -soak.duration=300s -soak.owner-writers=4 -soak.owner-readers=4 \
  -soak.client-processes=4 -soak.client-writers=3 -soak.client-readers=3

# B2 — the same, killed two thirds of the way in.
go test -tags soak,gms_pure_go -count=1 -v -timeout 30m ./tests/soak/... \
  -run TestUncleanKillAndStaleLockRecovery \
  -soak.duration=180s -soak.kill-after=120s \
  -soak.owner-writers=4 -soak.owner-readers=4 \
  -soak.client-processes=4 -soak.client-writers=3 -soak.client-readers=3

# C — burst: 32 writers across seven processes.
go test -tags soak,gms_pure_go -count=1 -v -timeout 20m ./tests/soak/... \
  -run 'TestConcurrencySoak|TestUncleanKill' \
  -soak.duration=60s -soak.kill-after=40s \
  -soak.owner-writers=8 -soak.owner-readers=4 \
  -soak.client-processes=6 -soak.client-writers=4 -soak.client-readers=2
```

Add `-soak.summary-dir=<dir>` to save each scenario's summary as JSON, and
`-soak.keep-scratch` to keep the store, ledgers and per-process stats.

The second build tag is not decoration — see F7. And note that the
verbatim command in this issue's acceptance criteria fails:

```
$ go test -tags soak ./tests/soak/...
# github.com/dolthub/go-icu-regex/internal/icu
..\..\..\..\go\pkg\mod\github.com\dolthub\go-icu-regex@v0.0.0-20260412212219-49724d547866\internal\icu\icu.go:8:11: fatal error: unicode/uregex.h: No such file or directory
    8 | // #include "unicode/uregex.h"
      |           ^~~~~~~~~~~~~~~~~~
compilation terminated.
FAIL	github.com/kninetimmy/memdolt/tests/soak [build failed]
```

The build constraint itself is exactly as specified: `go test ./...` with
no tags does not compile or run this package, and `soak` is what selects
it.
