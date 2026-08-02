# M1 spike — what `facts` uniqueness can guarantee

PRD §6.1 declared `UNIQUE KEY uk_fact_key (key)` on `facts`. Three other
parts of the document require two rows to share one key: §3.2 keeps
`superseded_by` because supersession relates two rows that both stay in the
table, §8.1 scores superseded rows with `superseded_penalty` (which only
means something if those rows are still retrievable), and §11.1 offers
keep-both as a fact-key conflict outcome. This spike measures what that
UNIQUE key actually permits, measures the arrangement the PRD adopts
instead, and does not implement any of the M1 lanes that use it.

## 1. Verdict

**`UNIQUE KEY uk_fact_key (key)` forbids supersession itself, not just
keep-both. The PRD now guarantees something narrower: at most one *live*
(non-superseded) row per key, enforced by a unique index over a generated
column, with no bound on how many superseded rows share a key.**

- Under §6.1's schema as written, a second row carrying an existing key is
  rejected on one machine with no merge involved (§3). So is the
  supersede-and-replace pair — link the old row, insert the new one — even
  inside one transaction. The constraint blocked the flow §6.2 describes.
- With `live_key GENERATED ALWAYS AS (IF(superseded_by IS NULL, key, NULL))
  STORED` and `UNIQUE KEY uk_fact_live_key (live_key)`, a second *live* row
  under an existing key is still rejected, supersede-and-replace succeeds,
  a key accumulates unboundedly many superseded rows, and every row under
  the key — superseded or not — stays retrievable through `MATCH … AGAINST`
  (§4). That is exactly the guarantee §8.1's penalty presumes.
- The merge behavior PRD §6.3 records is unchanged by the swap: two branches
  that each add a live fact under one key still land two rows in
  `dolt_constraint_violations_facts`, none in `dolt_conflicts_facts` (§5).
  The repair is now non-destructive — superseding the loser restores the
  invariant, where the old index left only deletion.
- One new merge behavior has to be handled. A branch that supersedes a
  fact and inserts its replacement, merged into a `facts` table that also
  moved, reports a constraint violation that is **already satisfied**: the
  merged rows are correct and `dolt constraints verify facts` exits 0, but
  two records sit in `dolt_constraint_violations_facts` and block the merge
  until they are cleared (§6). This is not a property of generated columns
  — a plain `UNIQUE` column shows it whenever a branch hands its unique
  value from one row to another (§6, control 2).
- The generated column must be `STORED`. `VIRTUAL` is accepted, indexes
  correctly and enforces the same uniqueness, but silently breaks
  `MATCH … AGAINST` on the same table: the FULLTEXT index returns nothing
  (§7). A `VIRTUAL` column would have passed every uniqueness test in this
  spike and destroyed recall.
- `key` needs its own B-tree index (`KEY idx_fact_key (key)`). It used to
  get one from `uk_fact_key`. Without one, `WHERE key = ?` does not fall
  back to a scan — it errors (§7).
- Dotted-prefix filtering and fact FULLTEXT coexist on v1.88.1 when
  `ft_facts` is declared `(value, key)`, fact retrieval uses
  `MATCH(value, key)`, and the prefix predicate is ``key LIKE ?`` with a
  bound dotted prefix such as `build.%` (§8). With `key` first in the
  FULLTEXT index, that predicate errors even when the B-tree exists.

## 2. What was measured

| | |
|---|---|
| Date | 2026-08-02 |
| Dolt | `dolt version 1.88.1` |
| Binary | `C:\Users\Kninetimmy\AppData\Local\Programs\dolt-1.88.1\bin\dolt.exe` |
| OS | Windows 11 Pro, build 26200, 64-bit |
| Shell | Git Bash — `GNU bash, version 5.2.37(1)-release (x86_64-pc-msys)` |
| Storage | disposable local Dolt repositories, all outside any git checkout |
| Schema | PRD §6.1's `facts` columns and live-key indexes in §§3–7; §8 records the follow-up FULLTEXT-order contract |

Every repository started empty; all timestamps are fixture values, not
observations. The heredocs and single-quoted SQL below are POSIX shell
syntax and do not run as written in PowerShell or `cmd`. Commands that
commit assume Dolt `user.name`/`user.email` are configured.

## 3. The schema as written

### Reproduce

Run in a fresh directory:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE facts (
  id CHAR(26) PRIMARY KEY,
  `key` VARCHAR(255),
  value TEXT,
  source VARCHAR(64) DEFAULT 'user',
  kind VARCHAR(64) NULL,
  evidence VARCHAR(1024) NULL,
  verified_at DATETIME NULL,
  created_at DATETIME,
  superseded_by CHAR(26) NULL,
  UNIQUE KEY uk_fact_key (`key`),
  FULLTEXT KEY ft_facts (`key`, value)
);
SQL

dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000001','build.command','go test ./...','2026-08-02 12:00:00')"
dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00')"
```

### Captured result

The first insert exited 0. The second was rejected:

```text
error on line 1 for query INSERT INTO facts (id,`key`,value,created_at) VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00'): duplicate unique key given: [build.command]
exit=1
```

One machine, one branch, no merge. The table kept one row:

```csv
id,key,value
00000000000000000000000001,build.command,go test ./...
```

Superseding first does not help. This transaction — PRD §6.2's supersede
payload, link and replacement together — was rejected on its `INSERT` and
rolled back whole:

```sh
dolt sql <<'SQL'
START TRANSACTION;
UPDATE facts SET superseded_by = '00000000000000000000000002' WHERE id = '00000000000000000000000001';
INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00');
COMMIT;
SQL
```

```text
error on line 3 for query INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00'): duplicate unique key given: [build.command]
exit=1
```

```csv
id,key,value,superseded_by
00000000000000000000000001,build.command,go test ./...,
```

`superseded_by` is empty: the link rolled back with the insert. Under this
index a fact key has exactly one row ever, so the flow that would set
`superseded_by` cannot run at all.

## 4. The live-key arrangement the PRD adopts, on one machine

This uniqueness run used `ft_facts(key, value)`. The dotted-prefix
follow-up in §8 changes only that FULLTEXT column order; it leaves every
live-key result below intact.

### Reproduce

Run in a fresh directory:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE facts (
  id CHAR(26) PRIMARY KEY,
  `key` VARCHAR(255),
  value TEXT,
  source VARCHAR(64) DEFAULT 'user',
  kind VARCHAR(64) NULL,
  evidence VARCHAR(1024) NULL,
  verified_at DATETIME NULL,
  created_at DATETIME,
  superseded_by CHAR(26) NULL,
  live_key VARCHAR(255) GENERATED ALWAYS AS (IF(superseded_by IS NULL, `key`, NULL)) STORED,
  KEY idx_fact_key (`key`),
  UNIQUE KEY uk_fact_live_key (live_key),
  FULLTEXT KEY ft_facts (`key`, value)
);
SQL

# A. first live row
dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000001','build.command','go test ./...','2026-08-02 12:00:00')"
# B. second live row under the same key
dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00')"
# C. supersede link, then replacement, in one transaction
dolt sql <<'SQL'
START TRANSACTION;
UPDATE facts SET superseded_by = '00000000000000000000000002'
 WHERE id = '00000000000000000000000001';
INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00');
COMMIT;
SQL
# D. the same two statements in the opposite order
dolt sql <<'SQL'
START TRANSACTION;
INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000003','build.command','go test -race ./...','2026-08-02 14:00:00');
UPDATE facts SET superseded_by = '00000000000000000000000003'
 WHERE id = '00000000000000000000000002';
COMMIT;
SQL
# E. chain, and two rows sharing one superseder
dolt sql <<'SQL'
START TRANSACTION;
UPDATE facts SET superseded_by = '00000000000000000000000003'
 WHERE id IN ('00000000000000000000000001','00000000000000000000000002');
INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000003','build.command','go test -race ./...','2026-08-02 14:00:00');
COMMIT;
SQL
# F. keep both, under distinct keys
dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000004','build.command.soak','go test -tags soak ./...','2026-08-02 15:00:00')"
```

### Captured result

The DDL exited 0; `SHOW CREATE TABLE facts` reported the generated column
back as `` `live_key` varchar(255) GENERATED ALWAYS AS
(if(`superseded_by` IS NULL,`key`,NULL)) STORED ``.

| Step | exit | Outcome |
|---|---|---|
| A first live row | 0 | inserted |
| B second live row, same key | 1 | `duplicate unique key given: [build.command]` |
| C link then insert | 0 | old row superseded, replacement live |
| D insert then link | 1 | `duplicate unique key given: [build.command]` |
| E two rows superseded by one row | 0 | both links set, third row live |
| F distinct key | 0 | inserted |

B and D were both refused with the message §3 produced, which quotes the
`key` value rather than the `live_key` column name:

```text
error on line 1 for query INSERT INTO facts (id,`key`,value,created_at) VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00'): duplicate unique key given: [build.command]
```

```text
error on line 2 for query INSERT INTO facts (id,`key`,value,created_at)
VALUES ('00000000000000000000000003','build.command','go test -race ./...','2026-08-02 14:00:00'): duplicate unique key given: [build.command]
```

D is the ordering requirement: the replacement's `live_key` collides until
the old row's `superseded_by` is set, so the link must be written first.
A transaction does not relax that — the check is per statement.

Final rows. Three rows share `build.command`, two of them superseded, and
`live_key` is NULL on exactly those two:

```csv
id,key,value,superseded_by,live_key
00000000000000000000000001,build.command,go test ./...,00000000000000000000000003,
00000000000000000000000002,build.command,go test -tags soak ./...,00000000000000000000000003,
00000000000000000000000003,build.command,go test -race ./...,,build.command
00000000000000000000000004,build.command.soak,go test -tags soak ./...,,build.command.soak
```

Two rows carrying NULL `live_key` coexist under one unique index, and a
third row was superseded by an id that also supersedes another row: NULLs
are distinct to this index, which is what lets superseded rows accumulate.

Superseded rows stay retrievable — the property `superseded_penalty` needs:

```sh
dolt sql -r csv -q "SELECT id,\`key\`,value,superseded_by IS NULL AS live FROM facts WHERE MATCH(\`key\`,value) AGAINST ('build command' IN NATURAL LANGUAGE MODE) GROUP BY id ORDER BY id"
```

```csv
id,key,value,live
00000000000000000000000001,build.command,go test ./...,false
00000000000000000000000002,build.command,go test -tags soak ./...,false
00000000000000000000000003,build.command,go test -race ./...,true
00000000000000000000000004,build.command.soak,go test -tags soak ./...,true
```

Both lookups the client needs work, and constraints verify:

```sh
dolt sql -r csv -q "SELECT id,value FROM facts WHERE live_key = 'build.command'"
dolt sql -r csv -q "SELECT COUNT(*) AS rows_under_key FROM facts WHERE \`key\` = 'build.command'"
dolt constraints verify facts
```

```csv
id,value
00000000000000000000000003,go test -race ./...
```

```csv
rows_under_key
3
```

`dolt constraints verify facts` exited 0 with no output.

## 5. Merge: two branches each add a live fact under one key

This is PRD §6.3's first conflict class, re-measured against the new index.

### Reproduce

Run in a fresh directory, with the §4 DDL:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE facts (
  id CHAR(26) PRIMARY KEY,
  `key` VARCHAR(255),
  value TEXT,
  source VARCHAR(64) DEFAULT 'user',
  kind VARCHAR(64) NULL,
  evidence VARCHAR(1024) NULL,
  verified_at DATETIME NULL,
  created_at DATETIME,
  superseded_by CHAR(26) NULL,
  live_key VARCHAR(255) GENERATED ALWAYS AS (IF(superseded_by IS NULL, `key`, NULL)) STORED,
  KEY idx_fact_key (`key`),
  UNIQUE KEY uk_fact_live_key (live_key),
  FULLTEXT KEY ft_facts (`key`, value)
);
SQL
dolt add . && dolt commit -m "Create facts schema"
dolt branch fact-theirs

dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000001','build.command','go test ./...','2026-08-02 12:00:00')"
dolt add facts && dolt commit -m "Insert ours fact"

dolt checkout fact-theirs
dolt sql -q "INSERT INTO facts (id,\`key\`,value,created_at) VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','2026-08-02 13:00:00')"
dolt add facts && dolt commit -m "Insert theirs fact"

dolt checkout main
dolt merge --no-edit fact-theirs
```

### Captured result

The merge exited 1:

```text
Updating 4crq70rrt9kk3fb93t6as50ciqtriipk..4h1n9j63d70ui1rdrsms6fhmq63f5elb
Auto-merging facts
CONSTRAINT VIOLATION (content): Merge created constraint violation in facts
Automatic merge failed; 1 table(s) are unmerged.
Fix constraint violations and then commit the result.
Constraint violations for the working set may be viewed using the 'dolt_constraint_violations' system table.
They may be queried and removed per-table using the 'dolt_constraint_violations_TABLENAME' system table.
```

Same surface split as PRD §6.3 records, now naming the new index:

```csv
conflict_rows,violation_rows
0,2
```

```csv
violation_type,id,key,value,live_key,violation_info
unique index,00000000000000000000000001,build.command,go test ./...,build.command,"{""Name"": ""uk_fact_live_key"", ""Columns"": [""live_key""]}"
unique index,00000000000000000000000002,build.command,go test -tags soak ./...,build.command,"{""Name"": ""uk_fact_live_key"", ""Columns"": [""live_key""]}"
```

The repair no longer has to discard a row. Superseding the loser restores
the invariant in the same transaction that clears the reviewed records:

```sh
dolt sql <<'SQL'
START TRANSACTION;
UPDATE facts SET superseded_by = '00000000000000000000000002'
 WHERE id = '00000000000000000000000001';
DELETE FROM dolt_constraint_violations_facts
 WHERE id IN (
   '00000000000000000000000001',
   '00000000000000000000000002'
 );
COMMIT;
SQL
dolt constraints verify facts
```

`dolt constraints verify facts` exited 0 with no output, and both rows
survived:

```csv
conflict_rows,violation_rows
0,0
```

```csv
id,key,value,superseded_by,live_key
00000000000000000000000001,build.command,go test ./...,00000000000000000000000002,
00000000000000000000000002,build.command,go test -tags soak ./...,,build.command
```

```text
On branch main
All conflicts and constraint violations fixed but you are still merging.
  (use "dolt commit" to conclude merge)
```

After `dolt add facts && dolt commit`, both rows were still matched by
`MATCH(key,value) AGAINST ('build command' …)` — the loser is penalized
later by §8.1, not lost here:

```csv
id
00000000000000000000000001
00000000000000000000000002
```

## 6. Merge: a supersede proposal into a `facts` table that also moved

This is PRD §6.2's accept path — one branch stages the `superseded_by` link and
the replacement row — merged three-way rather than fast-forwarded.

### Reproduce

Run in a fresh directory, with the §4 DDL, then:

```sh
dolt sql <<'SQL'
INSERT INTO facts (id,`key`,value,created_at) VALUES
 ('00000000000000000000000001','build.command','go test ./...','2026-08-02 12:00:00'),
 ('00000000000000000000000009','convention.style','gofmt','2026-08-02 12:00:00');
SQL
dolt add . && dolt commit -m "Durable facts on main"

dolt checkout -b proposal/01supersede
dolt sql <<'SQL'
UPDATE facts SET superseded_by = '00000000000000000000000002' WHERE id = '00000000000000000000000001';
INSERT INTO facts (id,`key`,value,source,created_at)
VALUES ('00000000000000000000000002','build.command','go test -tags soak ./...','agent:claude-code','2026-08-02 13:00:00');
SQL
dolt add facts && dolt commit -m "propose supersede fact build.command"

dolt checkout main
dolt sql -q "UPDATE facts SET value='gofmt -l .' WHERE id='00000000000000000000000009'"
dolt add facts && dolt commit -m "Unrelated durable edit on main"

dolt merge --no-edit proposal/01supersede
```

### Captured result

The merge exited 1 with a constraint violation, although the merged rows
are correct and nothing in them violates the index:

```text
Updating vae5spib9pld5d1dblmkh4ifusnoch62..92m2mgceqb28drbrmrq1puoa78j9mf8b
Auto-merging facts
CONSTRAINT VIOLATION (content): Merge created constraint violation in facts
Automatic merge failed; 1 table(s) are unmerged.
Fix constraint violations and then commit the result.
```

```csv
conflict_rows,violation_rows
0,2
```

```csv
violation_type,id,key,superseded_by,live_key
unique index,00000000000000000000000001,build.command,00000000000000000000000002,
unique index,00000000000000000000000002,build.command,,build.command
```

The two reported rows do not collide: one carries `live_key` NULL, the
other `build.command`. The merged table:

```csv
id,key,value,superseded_by,live_key
00000000000000000000000001,build.command,go test ./...,00000000000000000000000002,
00000000000000000000000002,build.command,go test -tags soak ./...,,build.command
00000000000000000000000009,convention.style,gofmt -l .,,convention.style
```

and `dolt constraints verify facts` exited 0 with no output — against the
same working set that Dolt is refusing to conclude. The violation is
already satisfied. Clearing the records without touching a row is enough:

```sh
dolt sql <<'SQL'
START TRANSACTION;
DELETE FROM dolt_constraint_violations_facts
 WHERE id IN (
   '00000000000000000000000001',
   '00000000000000000000000002'
 );
COMMIT;
SQL
dolt constraints verify facts
dolt add facts && dolt commit -m "Conclude merge"
```

All three exited 0, counts returned `0,0`, and `dolt status` reported
`nothing to commit, working tree clean`.

### Control 1 — the trigger is divergence in `facts`

The same proposal branch, merged into a `main` whose only new commit
inserted a `tasks` row, exited 0 and committed itself. Hashes and the date
line are per-run; the `Author:` line is elided:

```text
Updating mntuhanspg709k8rj40e4tc6b50r7h1s..2tqgaqf83au2c8j9a9qcoc18h6m9g311
commit mntuhanspg709k8rj40e4tc6b50r7h1s (HEAD -> main)
Merge: hoqsp455alnbodu9p35nle80pakprik1 2tqgaqf83au2c8j9a9qcoc18h6m9g311
Date:  Sun Aug 02 15:06:46 -0400 2026

	Merge branch 'proposal/01supersede' into main

facts | 2 +*
1 tables changed, 1 rows added(+), 1 rows modified(*), 0 rows deleted(-)
```

```csv
conflict_rows,violation_rows
0,0
```

A fast-forward — `main` not moved at all — is likewise clean. The
already-satisfied violation appears when `facts` itself has commits on both
sides, whatever rows those commits touched: the run above had `main` edit an
unrelated fact under a different key.

### Control 2 — not a property of generated columns

Any branch that vacates a unique value on one row and takes it on another
behaves the same way. With no generated column at all:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE t (id CHAR(2) PRIMARY KEY, k VARCHAR(32) NULL, UNIQUE KEY uk_t_k (k));
INSERT INTO t VALUES ('01','a');
SQL
dolt add . && dolt commit -m base
dolt checkout -b handoff
dolt sql <<'SQL'
UPDATE t SET k = NULL WHERE id = '01';
INSERT INTO t VALUES ('02','a');
SQL
dolt add t && dolt commit -m "hand the unique value to another row"
dolt checkout main
dolt sql -q "INSERT INTO t VALUES ('09','unrelated')"
dolt add t && dolt commit -m "unrelated insert on main"
dolt merge --no-edit handoff
```

The merge exited 1, with §5's message under the other table name (the
`Updating <hash>..<hash>`, `Auto-merging t` and trailing
`Constraint violations for the working set …` hint lines are elided):

```text
CONSTRAINT VIOLATION (content): Merge created constraint violation in t
Automatic merge failed; 1 table(s) are unmerged.
```

```csv
violation_type,id,k,violation_info
unique index,01,,"{""Name"": ""uk_t_k"", ""Columns"": [""k""]}"
unique index,02,a,"{""Name"": ""uk_t_k"", ""Columns"": [""k""]}"
```

```csv
id,k
01,
02,a
09,unrelated
```

`dolt constraints verify t` exited 0. Same shape, same already-satisfied
report. What the generated column changes is how often memdolt meets it:
supersede-and-replace is the routine accept path, so this stops being an
exotic case and becomes one the merge lane must recognize by re-verifying
before it escalates to an operator. PRD §6.3 carries that policy.

## 7. Why `STORED`, and why `key` keeps an index

Two arrangements pass every uniqueness test in §4 and §5 and are still
wrong.

**`VIRTUAL` breaks FULLTEXT.** Identical tables, one column definition
apart, one row inserted, same query:

```sh
# run once per storage class, in its own fresh directory
for mode in VIRTUAL STORED; do
  mkdir "ft_$mode" && cd "ft_$mode" && dolt init
  dolt sql -q "CREATE TABLE facts (id CHAR(26) PRIMARY KEY, \`key\` VARCHAR(255), value TEXT, superseded_by CHAR(26) NULL, live_key VARCHAR(255) GENERATED ALWAYS AS (IF(superseded_by IS NULL, \`key\`, NULL)) $mode, KEY idx_fact_key (\`key\`), UNIQUE KEY uk_fact_live_key (live_key), FULLTEXT KEY ft_facts (\`key\`, value))"
  dolt sql -q "INSERT INTO facts (id,\`key\`,value) VALUES ('0000000000000000000000000A','build.command','go test ./...')"
  dolt sql -r csv -q "SELECT id,\`key\` FROM facts WHERE MATCH(\`key\`,value) AGAINST ('build command' IN NATURAL LANGUAGE MODE) GROUP BY id"
  cd ..
done
```

```text
== VIRTUAL: MATCH(key,value) AGAINST ('build command') ==
id,key
exit=0

== STORED: MATCH(key,value) AGAINST ('build command') ==
id,key
0000000000000000000000000A,build.command
exit=0
```

`VIRTUAL` returns no rows and no error — recall would go quietly empty
while every uniqueness test still passed. Dropping the extra B-tree index
does not change it: the same `VIRTUAL` table without `idx_fact_key` also
matched nothing, and the same table with `STORED` and without
`idx_fact_key` matched the row. The generated column's storage class is
the variable.

**`key` must keep a B-tree index.** `uk_fact_key` used to provide one.
With `uk_fact_live_key` replacing it, `key` is left as the leading column
of a FULLTEXT key, and an ordinary equality filter on it does not fall back
to a scan:

```text
== without idx_fact_key: SELECT ... WHERE `key` = 'build.command' ==
error on line 1 for query SELECT id FROM facts WHERE `key` = 'build.command': Full-Text index found in filter with unknown expression: *expression.Equals
exit=1

== with idx_fact_key: SELECT ... WHERE `key` = 'build.command' ==
id
0000000000000000000000000A
exit=0
```

`list_facts`, the §11.1 conflict elicitation and the history lookups all
issue that filter, so `KEY idx_fact_key (key)` is part of the change, not
an optimization.

## 8. Dotted-prefix follow-up: FULLTEXT column order is the contract

Issue #31 settled the adjacent finding against the same Dolt v1.88.1
driver pinned in `go.mod`. With `ft_facts(key, value)`, a bound dotted
prefix still fails even though `idx_fact_key` exists:

```text
filtering facts by dotted key prefix: Error 1105: Full-Text index found in filter with unknown expression: *expression.GreaterThanOrEqual
```

The smallest working contract is to put `value` first and `key` second in
the FULLTEXT index, and use the same order in `MATCH`:

```sql
KEY idx_fact_key (`key`),
UNIQUE KEY uk_fact_live_key (live_key),
FULLTEXT KEY ft_facts (value, `key`)
```

```sql
-- list_facts / recall prefix filter; bind "build.%"
SELECT id FROM facts WHERE `key` LIKE ? ORDER BY id;

-- fact lexical gather
MATCH(value, `key`) AGAINST (? IN NATURAL LANGUAGE MODE)
```

`TestFactKeyPrefixAndFulltextContract` commits the measurement. Its four
facts include three `build.*` keys, one `env.*` control, and a superseded
`build.command` row. The prefix query returns exactly:

```text
fact-build-cache
fact-build-new
fact-build-old
```

The configured FULLTEXT gather for `cargo` returns both
`fact-build-new` and superseded `fact-build-old`; the schema change does
not make retrieval discard the row that §8.1 is designed to penalize.
Before the column-order change the regression failed with the error above;
after changing both the index and `MATCH` order it passed.

The supported filter shape is deliberately narrow: a literal dotted
namespace prefix plus one terminal `%`, passed as a bound value (for
example `build.%`). There is no contract for a leading wildcard, an infix
pattern, or general substring search over `key`; content search remains
the FULLTEXT path. The prefix filter includes live and superseded facts.

## 9. What the PRD gives up

The old text implied a guarantee it never delivered: one fact per key, so
"the fact under `build.command`" was a well-defined phrase. The document
also described supersession, superseded-row scoring and keep-both, none of
which are possible under that guarantee. Something had to go, and what goes
is the strong reading:

- **The number of rows carrying a key is unbounded.** Any query that means
  "the current fact under this key" has to say so — `WHERE live_key = ?`,
  or `WHERE key = ? AND superseded_by IS NULL`. A bare `WHERE key = ?`
  returns the whole supersession chain. §4 measured three rows under one
  key.
- **Keep-both under one key is not offered.** Two live rows with one key
  are exactly what the index rejects. PRD §11.1's keep-both outcome now re-files
  the new fact under a distinct key supplied in the same dialog; §4 step F
  measured that path.
- **Nothing enforces that a supersession chain is well-formed.** The index
  permits two rows superseded by the same id — measured, §4 step E. Dangling
  ids and cycles are unconstrained by construction rather than by
  measurement: §6.1 gives `superseded_by` no foreign key. Either way these
  are application invariants now, not database ones.

The alternative — dropping the unique index entirely and enforcing one live
fact per key in Go — was not measured, because it loses what PRD §6.3 depends
on: the merge-time detection that turns two machines' same-key writes into
a reviewable constraint violation (§5) instead of two silent rows.

## 10. Scope consequence

This spike changes PRD §6.1's `facts` DDL and the sentences that describe what
it guarantees (§3.2, §6.2, §6.3, §8.1, §11.1). It adds no code and no
migration; `internal/schema/` does not exist yet, and M1 writes this DDL
once when it does. Concretely, M1 owes:

- the write ordering from §4 step D — `superseded_by` link before the
  replacement insert, and the same order inside a proposal branch;
- the already-satisfied violation from §6 — re-verify with
  `dolt constraints verify` before escalating a merge violation to an
  operator, and clear reviewed records that verification shows are moot;
- `STORED`, not `VIRTUAL` (§7), with the FULLTEXT check that would catch a
  future "optimization" back to `VIRTUAL`;
- `ft_facts(value, key)` paired with `MATCH(value, key)`, so the supported
  bound dotted-prefix filter remains usable (§8);
- application-level checks for the chain invariants §9 lists, if the
  product wants them.
