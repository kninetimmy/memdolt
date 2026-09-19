# M4 selected original evidence

Selected credential-free excerpts from the Architect-supplied 2026-09-19
`RESULTS.md` and named entries of `evidence.zip`. These are historical receipts,
not commands to execute. The original files remain outside Git; the harness and
unreviewed archive entries are not included. JSON blocks below retain the exact
original text. See [the acceptance report](m4-acceptance.md) for interpretation,
independent replay, limitations and current status.

## Original RESULTS.md excerpts

The revision/machine/build lines and topology-through-cleanup sections follow
verbatim; binary hash bullets and local evidence-location paths are omitted.
Computed binary hashes alone are not trusted publisher verification.

Executed 2026-09-19 against clean Git revision `937db3caead136abd7c70901184caa17906dcf39`.

## Machines and builds

- Client A: Katherine's MacBook Pro, macOS/arm64, Tailscale `100.65.43.99`.
- Client B: DESKTOP-LCJ0F6H, Windows/amd64, Tailscale `100.102.188.1`.
- Hub: RasPi, Linux/arm64, Tailscale `100.69.137.74`.
- Both memdolt clients were built from that revision using Go 1.26.6, CGO and `gms_pure_go`; native memory schema 4, embedded Dolt 1.88.1.

## Test topology and scope

The real Pi hosted a separate disposable native server: SQL 53306, remotesapi 55051, database `m4_acceptance`. Both clients transferred directly over Tailscale; SSH was used only to operate the machines. The existing `dolthub.service`, accounts and data were not changed.

The seed used synthetic tasks, notes and a state narrative, with shared identity `memdolt-m4-fa4e2db7`. Its credential-free Git-origin label was `https://example.com/acceptance/memdolt-m4.git`; no connection to that example domain was made.

The initial independently created empty hub database correctly rejected a non-fast-forward seed push (no common ancestor). Its main was inspected and confirmed unchanged. Bootstrap was corrected by stopping the temporary server, preserving the empty database separately, and copying the closed seed's native Dolt database into the temporary hub. This was fixture initialization before either client cloned. All subsequent transfers used shipped memdolt clone/push/pull, with no force, export/import transfer, schema migration or format conversion.

## Results

1. Both physical clients cloned seed main `sodb383fg9gg8tp4jcfb8f3062908fmn` with identical project identity and schema.
2. Each independently added a task and note. Mac pushed; Windows observed divergence and pulled, producing two-parent merge `mmlptp6nshs9hgvpbu6dide4eljjgj27`. Windows pushed; Mac pulled that exact commit.
3. Both clients edited the same task to different blocker notes. Mac pushed. Windows pull returned one explicit conflict with base/ours/theirs and blame. Its main stayed unchanged and clean.
4. An explicit resolution bound to both displayed heads selected the Mac row. Windows created two-parent merge `bivhftr074bciqg4rfuiqufob3d6nqt0`, pushed it, and Mac pulled it.
5. Mac, Windows and Pi agreed on that final main hash. Native Pi ancestry verified two parents for each merge. Both client working sets and the hub were clean.
6. Both clients had exactly 3 tasks, 3 session notes and 1 state narrative; all other exported memory tables and pending proposals were empty. Every exported client row matched exactly. Independent Pi SQL checks matched all 53 populated fields, including explicit NULLs.
7. Canonical table-content SHA256 on both clients: `c9a6e1a467b4db4840d54f345559c0e8308e8a01310fac9163885ef11b881390`. Whole export file digests differed because export timestamps differed; those file digests were not used as content-equality evidence.
8. After stopping the temporary hub, fresh ordinary local memdolt processes on both devices reopened schema 4 and the final hash, read all tasks/notes, and reported clean stores. No conversion, import or post-clone init was used.
9. Wrong-password cloning failed. Allowed Mac IPv4/IPv6 TCP probes reached remotesapi. An isolated Linux network namespace attached through a non-tailnet veth tested both ports over IPv4 and IPv6: all four probes timed out and the temporary firewall's drop counter increased by four.

## Evidence notes

Native Dolt's ordinary JSON output omits NULL-valued fields. JSON_OBJECT without casts also serializes ENUM status as its ordinal and adds DATETIME fractional formatting. The final independent SQL comparison used JSON_OBJECT with CAST(column AS CHAR), preserving explicit NULLs and matching the client's lossless string/NULL export representation. Initial formatting mismatches were harness issues, not changed stored data. The raw outputs remain available.

Key evidence: `comparison.json`, `pi-row-comparison.json`, `pi-canonical-rows.json`, `pi-final-sql.json`, `windows-conflict-preview.json`, `windows-after-conflict-preview.json`, `resolution.json`, both `*-final-export.json`, `offline-reopen.json`, `boundary-probes.json`, `boundary-rules.json`, and `cleanup.json`. Other JSON files retain command outcomes and intermediate states.

## Cleanup and limits

The temporary server was stopped; both temporary ports were confirmed closed. Its firewall table, network namespace and veth were removed. The original hub service remained active and its original firewall table remained present. Generated plaintext test credentials were deleted locally; SSH passwords were never placed in scripts, evidence files or memhub. Closed fixture databases and evidence are retained for inspection.

This passes the PRD section 16 physical two-client round-trip/counts/hashes/plain-reopen gate for this fixture and exact version pair. It is not production-hub deployment acceptance, an external physical off-tailnet-host test, live MCP conflict-dialog acceptance, exhaustive conflict-class coverage, version-range compatibility, a power-loss test, a backup/restore drill or completion of all M4–M6 work. The existing production hub's managed deployment and credential setup remain separate.

## Selected JSON receipts

### seed-push.json

```json
{
  "returncode": 1,
  "stdout": "",
  "stderr": "push did not confirm completion for captured main sodb383fg9gg8tp4jcfb8f3062908fmn; remote outcome may be unknown; inspect remote main before retrying (non-fast-forward history must be reconciled manually): Error 1105: unknown push error; no common ancestor\n"
}
```

### pi-initial-main.json

```json
{
  "returncode": 0,
  "stdout": "{\"rows\": [{\"main_hash\":\"dig8i3r6hokd84huf3k2lokfqphm6e2q\"}]}\n{}\n\n\n",
  "stderr": "\n"
}
```

### pi-copy-native-seed.json

```json
{
  "returncode": 0,
  "stdout": "",
  "stderr": ""
}
```

### comparison.json

```json
{
  "main": "bivhftr074bciqg4rfuiqufob3d6nqt0",
  "canonical_tables_sha256": "c9a6e1a467b4db4840d54f345559c0e8308e8a01310fac9163885ef11b881390",
  "counts": {
    "commands": 0,
    "decisions": 0,
    "facts": 0,
    "project_arch": 0,
    "project_state": 1,
    "proposals": 0,
    "session_notes": 3,
    "tasks": 3
  },
  "all_rows_equal": true,
  "schema": 4
}
```

### pi-row-comparison.json

```json
{
  "all_hub_fixture_rows_equal": true,
  "explicit_nulls_verified": true,
  "cells": 53,
  "hub_main_equal": true,
  "hub_clean": true,
  "merge_parents": {
    "bivhftr074bciqg4rfuiqufob3d6nqt0": [
      "8jruo5g8hjd3of3td4hkmnkf2v7qae6i",
      "47bti82vpa8cd52a67toi8h971nvq2ht"
    ],
    "mmlptp6nshs9hgvpbu6dide4eljjgj27": [
      "prgiaj5260ub143786pqliha7jlo52m1",
      "3q8g3v01mbafj873vq7kvedsbn9a2p3t"
    ]
  }
}
```

### mac-final-export.json

```json
{
  "memdolt_export_version": 1,
  "source_schema_version": 4,
  "exported_at": "2026-09-19T19:10:04.432337Z",
  "main_commit": "bivhftr074bciqg4rfuiqufob3d6nqt0",
  "tables": {
    "commands": [],
    "decisions": [],
    "facts": [],
    "project_arch": [],
    "project_state": [
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "body": "M4 base state: both clients start here.",
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QVX78D87BK97W9JKNTT"
      }
    ],
    "proposals": [],
    "session_notes": [
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QS8QDYDFJWPHFQEQHJN",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 physical acceptance seed: preserved across both client clones.",
        "variant": null
      },
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:08:51",
        "id": "01M2XH4GE8MZY3QQRN2HX5350Z",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 independent note from windows over real Tailscale connection",
        "variant": null
      },
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:08:53",
        "id": "01M2XH4JVD4RK8QGRNQHEAQ63W",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 independent note from mac over real Tailscale connection",
        "variant": null
      }
    ],
    "tasks": [
      {
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QPJ803KY1J77B7EF0DP",
        "notes": "Synthetic conflicting blocker chosen on mac",
        "status": "blocked",
        "title": "M4 shared completion task",
        "updated_at": "2026-09-19 19:09:21"
      },
      {
        "created_at": "2026-09-19 19:08:50",
        "id": "01M2XH4FGM9E9W1P3FMCEDFFZX",
        "notes": "Must survive native divergent merge",
        "status": "open",
        "title": "M4 independent task from windows",
        "updated_at": "2026-09-19 19:08:50"
      },
      {
        "created_at": "2026-09-19 19:08:53",
        "id": "01M2XH4JRFJ336Y4NDX7HV9149",
        "notes": "Must survive native divergent merge",
        "status": "open",
        "title": "M4 independent task from mac",
        "updated_at": "2026-09-19 19:08:53"
      }
    ]
  },
  "pending_proposals": []
}
```

### windows-final-export.json

```json
{
  "memdolt_export_version": 1,
  "source_schema_version": 4,
  "exported_at": "2026-09-19T19:10:02.1380051Z",
  "main_commit": "bivhftr074bciqg4rfuiqufob3d6nqt0",
  "tables": {
    "commands": [],
    "decisions": [],
    "facts": [],
    "project_arch": [],
    "project_state": [
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "body": "M4 base state: both clients start here.",
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QVX78D87BK97W9JKNTT"
      }
    ],
    "proposals": [],
    "session_notes": [
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QS8QDYDFJWPHFQEQHJN",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 physical acceptance seed: preserved across both client clones.",
        "variant": null
      },
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:08:51",
        "id": "01M2XH4GE8MZY3QQRN2HX5350Z",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 independent note from windows over real Tailscale connection",
        "variant": null
      },
      {
        "actor": "agent:codex",
        "actor_raw": "codex",
        "agent_id": null,
        "created_at": "2026-09-19 19:08:53",
        "id": "01M2XH4JVD4RK8QGRNQHEAQ63W",
        "model_id": null,
        "provider_id": null,
        "session_id": null,
        "text": "M4 independent note from mac over real Tailscale connection",
        "variant": null
      }
    ],
    "tasks": [
      {
        "created_at": "2026-09-19 19:07:20",
        "id": "01M2XH1QPJ803KY1J77B7EF0DP",
        "notes": "Synthetic conflicting blocker chosen on mac",
        "status": "blocked",
        "title": "M4 shared completion task",
        "updated_at": "2026-09-19 19:09:21"
      },
      {
        "created_at": "2026-09-19 19:08:50",
        "id": "01M2XH4FGM9E9W1P3FMCEDFFZX",
        "notes": "Must survive native divergent merge",
        "status": "open",
        "title": "M4 independent task from windows",
        "updated_at": "2026-09-19 19:08:50"
      },
      {
        "created_at": "2026-09-19 19:08:53",
        "id": "01M2XH4JRFJ336Y4NDX7HV9149",
        "notes": "Must survive native divergent merge",
        "status": "open",
        "title": "M4 independent task from mac",
        "updated_at": "2026-09-19 19:08:53"
      }
    ]
  },
  "pending_proposals": []
}
```

### pi-canonical-rows.json

```json
{
  "returncode": 0,
  "stdout": "{\"commands\": {\"rows\": [{\"row_count\": \"0\"}]}, \"decisions\": {\"rows\": [{\"row_count\": \"0\"}]}, \"facts\": {\"rows\": [{\"row_count\": \"0\"}]}, \"project_arch\": {\"rows\": [{\"row_count\": \"0\"}]}, \"project_state\": {\"rows\": [{\"row_json\": \"{\\\"id\\\": \\\"01M2XH1QVX78D87BK97W9JKNTT\\\", \\\"body\\\": \\\"M4 base state: both clients start here.\\\", \\\"actor\\\": \\\"agent:codex\\\", \\\"actor_raw\\\": \\\"codex\\\", \\\"created_at\\\": \\\"2026-09-19 19:07:20\\\"}\"}]}, \"proposals\": {\"rows\": [{\"row_count\": \"0\"}]}, \"session_notes\": {\"rows\": [{\"row_json\": \"{\\\"id\\\": \\\"01M2XH1QS8QDYDFJWPHFQEQHJN\\\", \\\"text\\\": \\\"M4 physical acceptance seed: preserved across both client clones.\\\", \\\"actor\\\": \\\"agent:codex\\\", \\\"variant\\\": null, \\\"agent_id\\\": null, \\\"model_id\\\": null, \\\"actor_raw\\\": \\\"codex\\\", \\\"created_at\\\": \\\"2026-09-19 19:07:20\\\", \\\"session_id\\\": null, \\\"provider_id\\\": null}\"}, {\"row_json\": \"{\\\"id\\\": \\\"01M2XH4GE8MZY3QQRN2HX5350Z\\\", \\\"text\\\": \\\"M4 independent note from windows over real Tailscale connection\\\", \\\"actor\\\": \\\"agent:codex\\\", \\\"variant\\\": null, \\\"agent_id\\\": null, \\\"model_id\\\": null, \\\"actor_raw\\\": \\\"codex\\\", \\\"created_at\\\": \\\"2026-09-19 19:08:51\\\", \\\"session_id\\\": null, \\\"provider_id\\\": null}\"}, {\"row_json\": \"{\\\"id\\\": \\\"01M2XH4JVD4RK8QGRNQHEAQ63W\\\", \\\"text\\\": \\\"M4 independent note from mac over real Tailscale connection\\\", \\\"actor\\\": \\\"agent:codex\\\", \\\"variant\\\": null, \\\"agent_id\\\": null, \\\"model_id\\\": null, \\\"actor_raw\\\": \\\"codex\\\", \\\"created_at\\\": \\\"2026-09-19 19:08:53\\\", \\\"session_id\\\": null, \\\"provider_id\\\": null}\"}]}, \"tasks\": {\"rows\": [{\"row_json\": \"{\\\"id\\\": \\\"01M2XH1QPJ803KY1J77B7EF0DP\\\", \\\"notes\\\": \\\"Synthetic conflicting blocker chosen on mac\\\", \\\"title\\\": \\\"M4 shared completion task\\\", \\\"status\\\": \\\"blocked\\\", \\\"created_at\\\": \\\"2026-09-19 19:07:20\\\", \\\"updated_at\\\": \\\"2026-09-19 19:09:21\\\"}\"}, {\"row_json\": \"{\\\"id\\\": \\\"01M2XH4FGM9E9W1P3FMCEDFFZX\\\", \\\"notes\\\": \\\"Must survive native divergent merge\\\", \\\"title\\\": \\\"M4 independent task from windows\\\", \\\"status\\\": \\\"open\\\", \\\"created_at\\\": \\\"2026-09-19 19:08:50\\\", \\\"updated_at\\\": \\\"2026-09-19 19:08:50\\\"}\"}, {\"row_json\": \"{\\\"id\\\": \\\"01M2XH4JRFJ336Y4NDX7HV9149\\\", \\\"notes\\\": \\\"Must survive native divergent merge\\\", \\\"title\\\": \\\"M4 independent task from mac\\\", \\\"status\\\": \\\"open\\\", \\\"created_at\\\": \\\"2026-09-19 19:08:53\\\", \\\"updated_at\\\": \\\"2026-09-19 19:08:53\\\"}\"}]}}\n",
  "stderr": ""
}
```

### offline-reopen.json

```json
{
  "mac": true,
  "windows": true,
  "testHubStopped": true,
  "conversionOrImportUsed": false,
  "migrationAfterClone": false
}
```

### pi-stop-final.json

```json
{
  "returncode": 0,
  "stdout": "{\"testServerStopped\": true, \"portsClosed\": [53306, 55051]}\n",
  "stderr": ""
}
```

### cleanup.json

```json
{"temporaryFirewallRemoved": true, "temporaryNamespaceAndVethRemoved": true, "originalHubService": "active", "originalHubFirewallPresent": true}
```

### resolution.json

```json
{
  "localCommit": "8jruo5g8hjd3of3td4hkmnkf2v7qae6i",
  "remoteCommit": "47bti82vpa8cd52a67toi8h971nvq2ht",
  "choices": [
    {
      "conflict": "tasks:row:01M2XH1QPJ803KY1J77B7EF0DP",
      "take": "theirs"
    }
  ]
}
```

### wrong-password-refused.json

```json
{
  "returncode": 1,
  "stdout": "",
  "stderr": "clone did not complete; artifacts retained at /Users/stephenelswick/.cache/memdolt-m4-acceptance/denied-client/.memdolt/dolt; inspect them before retrying in a fresh --dir: access clone remote; check URL, connectivity and --user/DOLT_REMOTE_PASSWORD: could not access dolt url 'http://100.69.137.74:55051/m4_acceptance': rpc error: code = Unauthenticated desc = API Authentication Failure: Access denied for user 'm4_writer' (errno 1045) (sqlstate 28000)\n"
}
```
