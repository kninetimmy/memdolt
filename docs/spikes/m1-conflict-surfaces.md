# M1 spike — Dolt merge-conflict surfaces

Task 14 asks one narrow question left by M0 rig 4: which Dolt surfaces the
M1 review/merge lane must inspect when divergent memory branches do not
merge cleanly. This spike uses the PRD §6.1 `facts` and `tasks` shapes and
does not implement that lane.

## 1. Verdict

**The two cases fail on different surfaces in Dolt v1.88.1, and a same-row
task race conflicts only where both branches write the same cell.**

- Distinct fact ULIDs carrying the same UNIQUE `key` produce no rows in
  `dolt_conflicts_facts`; both rows appear in
  `dolt_constraint_violations_facts` as `unique index` violations.
  `dolt conflicts resolve --ours facts` exits 0 but does not remove those
  rows or clear the unmerged state.
- A done-versus-edit update to one task in which both branches also write
  `updated_at` produces a row in `dolt_conflicts_tasks` (§4). The ordinary
  `tasks` row initially remains ours. Resolving to theirs because its
  `updated_at` is newer changes `done` back to `open`, silently discarding
  durable task state.
- That conflict follows from the shared `updated_at` cell, not from the two
  branches modifying one row. The same done-versus-edit merge with neither
  branch writing `updated_at` — `status` on ours, `title` on theirs —
  merges cleanly and cell by cell (§5): exit 0, an automatic merge commit,
  `done` and `edited` both kept in the one row, and `dolt_conflicts_tasks`
  empty. Cell-level three-way merge is real in v1.88.1; it just cannot
  help two writes that land on one cell.

The M1 path must therefore inspect both conflict and constraint-violation
tables after a failed merge. It must not use whole-row
newest-`updated_at` selection for task conflicts. And whether a same-row
task race reaches a conflict at all is decided by the write path: a client
that stamps `updated_at` on every task mutation turns every such race into
a conflict, while one that writes only the columns a caller changed lets
Dolt combine disjoint task edits with no operator in the loop.

## 2. What was measured

| | |
|---|---|
| Date | 2026-08-02 |
| Dolt | `dolt version 1.88.1` |
| Binary | `C:\Users\Kninetimmy\AppData\Local\Programs\dolt-1.88.1\bin\dolt.exe` |
| OS | Windows 11 Pro, build 26200, 64-bit |
| Shell | Git Bash — `GNU bash, version 5.2.37(1)-release (x86_64-pc-msys)` |
| Storage | three disposable local Dolt repositories, all outside any git checkout |
| Schema | PRD §6.1 columns, UNIQUE keys, and FULLTEXT keys for the measured tables |

The repositories started empty. All timestamps below are explicit fixture
values, not wall-clock observations. The commands assume Dolt
`user.name`/`user.email` are already configured for commits.

Every command below was run in that shell; the heredocs and single-quoted
SQL are POSIX shell syntax and do not run as written in PowerShell or
`cmd`. While §5 was measured, §3's and §4's commands — merges, surface
queries, and resolution steps alike — were re-run there unchanged and
reproduced every result recorded for them, commit hashes excepted.

## 3. Same-key fact inserts

The DDL below is PRD §6.1's `facts` as it stood on this spike's date, with
`UNIQUE KEY uk_fact_key (key)`. §6.1 has since scoped that uniqueness to
live rows; the same merge was re-measured against the current schema in
`m1-fact-key-uniqueness.md` §5 and reproduced this section's outcome —
two rows in `dolt_constraint_violations_facts`, none in
`dolt_conflicts_facts` — under the new index name.

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
dolt add .
dolt commit -m "Create facts schema"
dolt branch fact-theirs

dolt sql <<'SQL'
INSERT INTO facts (id, `key`, value, created_at)
VALUES (
  '00000000000000000000000001',
  'build.command',
  'go test ./...',
  '2026-08-02 12:00:00'
);
SQL
dolt add facts
dolt commit -m "Insert ours fact"

dolt checkout fact-theirs
dolt sql <<'SQL'
INSERT INTO facts (id, `key`, value, created_at)
VALUES (
  '00000000000000000000000002',
  'build.command',
  'go test -tags soak ./...',
  '2026-08-02 13:00:00'
);
SQL
dolt add facts
dolt commit -m "Insert theirs fact"

dolt checkout main
dolt merge --no-edit fact-theirs
```

### Captured result

The merge exited 1:

```text
Updating vr3orfkvh5nf78lc8a2hrfne5c3fkmjh..t15o4i3jat71kpenvqo8dabf1jalpr92
Auto-merging facts
CONSTRAINT VIOLATION (content): Merge created constraint violation in facts
Automatic merge failed; 1 table(s) are unmerged.
Fix constraint violations and then commit the result.
Constraint violations for the working set may be viewed using the 'dolt_constraint_violations' system table.
They may be queried and removed per-table using the 'dolt_constraint_violations_TABLENAME' system table.
```

The two surfaces and ordinary table were queried with:

```sh
dolt sql -r csv -q \
  'SELECT (SELECT COUNT(*) FROM dolt_conflicts_facts) AS conflict_rows,
          (SELECT COUNT(*) FROM dolt_constraint_violations_facts) AS violation_rows'
dolt sql -r csv -q \
  'SELECT violation_type,id,`key`,value,created_at,violation_info
     FROM dolt_constraint_violations_facts ORDER BY id'
dolt sql -r csv -q \
  'SELECT id,`key`,value,created_at FROM facts ORDER BY id'
```

```csv
conflict_rows,violation_rows
0,2
```

```csv
violation_type,id,key,value,created_at,violation_info
unique index,00000000000000000000000001,build.command,go test ./...,2026-08-02 12:00:00,"{""Name"": ""uk_fact_key"", ""Columns"": [""key""]}"
unique index,00000000000000000000000002,build.command,go test -tags soak ./...,2026-08-02 13:00:00,"{""Name"": ""uk_fact_key"", ""Columns"": [""key""]}"
```

```csv
id,key,value,created_at
00000000000000000000000001,build.command,go test ./...,2026-08-02 12:00:00
00000000000000000000000002,build.command,go test -tags soak ./...,2026-08-02 13:00:00
```

`dolt conflicts resolve --ours facts` then exited 0 with no output. The
counts remained `0,2`, and `dolt status` still reported:

```text
On branch main
You have unmerged tables.
  (fix constraint violations and run "dolt commit")
  (use "dolt merge --abort" to abort the merge)

Unmerged paths:
  (use "dolt add <table>..." to mark resolution)
	modified          facts
```

This fail-closed manual resolution kept ours. The data row and the two
reviewed violation records had to be changed in one transaction; a
standalone `DELETE FROM facts` was rolled back because its transaction
still ended with recorded constraint violations.

```sh
dolt sql <<'SQL'
START TRANSACTION;
DELETE FROM facts
 WHERE id = '00000000000000000000000002';
DELETE FROM dolt_constraint_violations_facts
 WHERE id IN (
   '00000000000000000000000001',
   '00000000000000000000000002'
 );
COMMIT;
SQL
dolt constraints verify facts
```

`dolt constraints verify facts` exited 0 with no output. Re-querying
reported `0,0`, left only the selected row, and changed status to
`All conflicts and constraint violations fixed but you are still merging`
with `dolt commit` required to conclude the merge.

## 4. Task done-versus-edit

Both branches here write `updated_at` in addition to their disjoint column.
§5 isolates that by repeating this case with neither branch writing it.

### Reproduce

Run in a second fresh directory:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE tasks (
  id CHAR(26) PRIMARY KEY,
  title VARCHAR(512),
  status ENUM('open','done','blocked'),
  notes TEXT NULL,
  created_at DATETIME,
  updated_at DATETIME,
  FULLTEXT KEY ft_tasks (title, notes)
);
INSERT INTO tasks
VALUES (
  '01TASK00000000000000000000',
  'original',
  'open',
  NULL,
  '2026-08-02 11:00:00',
  '2026-08-02 11:00:00'
);
SQL
dolt add .
dolt commit -m "Create task baseline"
dolt branch task-edit

dolt sql -q \
  "UPDATE tasks SET status='done', updated_at='2026-08-02 12:00:00'
    WHERE id='01TASK00000000000000000000'"
dolt add tasks
dolt commit -m "Complete task"

dolt checkout task-edit
dolt sql -q \
  "UPDATE tasks SET title='edited', updated_at='2026-08-02 13:00:00'
    WHERE id='01TASK00000000000000000000'"
dolt add tasks
dolt commit -m "Edit task"

dolt checkout main
dolt merge --no-edit task-edit
```

### Captured result

The merge exited 1:

```text
Updating o3ccto53rk6gommmbdiq1a5o0d80gdpr..jhbitpkpg49b8psokmk0v4353m94e66l
Auto-merging tasks
CONFLICT (content): Merge conflict in tasks
Automatic merge failed; 1 table(s) are unmerged.
Use 'dolt conflicts' to investigate and resolve conflicts.
```

The ordinary row after the failed merge was ours:

```csv
id,title,status,notes,created_at,updated_at
01TASK00000000000000000000,original,done,,2026-08-02 11:00:00,2026-08-02 12:00:00
```

This projection of `dolt_conflicts_tasks` captured the three versions:

```sh
dolt sql -r csv -q \
  'SELECT base_title,base_status,base_updated_at,
          our_title,our_status,our_updated_at,our_diff_type,
          their_title,their_status,their_updated_at,their_diff_type
     FROM dolt_conflicts_tasks'
```

```csv
base_title,base_status,base_updated_at,our_title,our_status,our_updated_at,our_diff_type,their_title,their_status,their_updated_at,their_diff_type
original,open,2026-08-02 11:00:00,original,done,2026-08-02 12:00:00,modified,edited,open,2026-08-02 13:00:00,modified
```

The branch with the newer `updated_at` was theirs. Selecting it:

```sh
dolt conflicts resolve --theirs tasks
dolt sql -r csv -q \
  'SELECT id,title,status,notes,created_at,updated_at FROM tasks'
```

produced:

```csv
id,title,status,notes,created_at,updated_at
01TASK00000000000000000000,edited,open,,2026-08-02 11:00:00,2026-08-02 13:00:00
```

`dolt_conflicts_tasks` was then empty, but `done` had been lost. After
aborting and repeating the merge, an explicit manual combination proved
that the edit and completion can both be retained:

```sh
dolt conflicts resolve --ours tasks
dolt sql -q \
  "UPDATE tasks SET title='edited', updated_at='2026-08-02 13:00:00'
    WHERE id='01TASK00000000000000000000'"
```

```csv
id,title,status,notes,created_at,updated_at
01TASK00000000000000000000,edited,done,,2026-08-02 11:00:00,2026-08-02 13:00:00
```

That combination is evidence that manual resolution is possible, not a
general auto-merge rule. The product still needs operator review of the
base/ours/theirs values because another task edit may not combine safely.

## 5. Task edits on disjoint columns

§4's conflict has two candidate explanations: the `updated_at` cell both
branches wrote, or Dolt declining to merge two modifications of one row at
all. This run separates them. It is §4 with the two `UPDATE`s narrowed to
their disjoint columns — `status` on ours, `title` on theirs — and neither
branch writing `updated_at`.

### Reproduce

Run in a third fresh directory:

```sh
dolt init
dolt sql <<'SQL'
CREATE TABLE tasks (
  id CHAR(26) PRIMARY KEY,
  title VARCHAR(512),
  status ENUM('open','done','blocked'),
  notes TEXT NULL,
  created_at DATETIME,
  updated_at DATETIME,
  FULLTEXT KEY ft_tasks (title, notes)
);
INSERT INTO tasks
VALUES (
  '01TASK00000000000000000000',
  'original',
  'open',
  NULL,
  '2026-08-02 11:00:00',
  '2026-08-02 11:00:00'
);
SQL
dolt add .
dolt commit -m "Create task baseline"
dolt branch task-edit

dolt sql -q \
  "UPDATE tasks SET status='done'
    WHERE id='01TASK00000000000000000000'"
dolt add tasks
dolt commit -m "Complete task"

dolt checkout task-edit
dolt sql -q \
  "UPDATE tasks SET title='edited'
    WHERE id='01TASK00000000000000000000'"
dolt add tasks
dolt commit -m "Edit task"

dolt checkout main
dolt merge --no-edit task-edit
```

### Captured result

The merge exited 0 and committed itself. Hashes and the date line are
per-run; the `Author:` line is elided:

```text
Updating r82nvs8had211lpmvju37sruus3a92qt..ntvk8eue6t1g801ssr73rfbnlg0t7r3b
commit r82nvs8had211lpmvju37sruus3a92qt (HEAD -> main)
Merge: 1asjn7tkdj5gjivp9l1u8209r5pkr4l4 ntvk8eue6t1g801ssr73rfbnlg0t7r3b
Date:  Sun Aug 02 14:35:45 -0400 2026

	Merge branch 'task-edit' into main

tasks | 1 *
1 tables changed, 0 rows added(+), 1 rows modified(*), 0 rows deleted(-)
```

Both surfaces, the ordinary row, and the working state were queried with:

```sh
dolt sql -r csv -q 'SELECT COUNT(*) AS conflict_rows FROM dolt_conflicts_tasks'
dolt sql -r csv -q 'SELECT * FROM dolt_conflicts_tasks'
dolt sql -r csv -q \
  'SELECT COUNT(*) AS violation_rows FROM dolt_constraint_violations_tasks'
dolt sql -r csv -q \
  'SELECT id,title,status,notes,created_at,updated_at FROM tasks'
dolt status
```

```csv
conflict_rows
0
```

`dolt_conflicts_tasks` returned its header and no rows:

```csv
from_root_ish,base_id,base_title,base_status,base_notes,base_created_at,base_updated_at,our_id,our_title,our_status,our_notes,our_created_at,our_updated_at,our_diff_type,their_id,their_title,their_status,their_notes,their_created_at,their_updated_at,their_diff_type,dolt_conflict_id
```

```csv
violation_rows
0
```

```csv
id,title,status,notes,created_at,updated_at
01TASK00000000000000000000,edited,done,,2026-08-02 11:00:00,2026-08-02 11:00:00
```

```text
On branch main
nothing to commit, working tree clean
```

Dolt v1.88.1 merged the two modifications cell by cell: the merged row
carries theirs' `title` and ours' `status`, and `updated_at` keeps its base
value because neither branch changed it. No conflict row, no constraint
violation, no unmerged state — and no review opportunity, since the merge
concluded on its own.

So §4's conflict follows from `updated_at`. Same two branches, same
disjoint `status`/`title` edits; the only difference is whether both sides
also wrote a different value into that one shared cell. Dolt does not
conflict on a row because two branches modified it — it conflicts on a cell
whose two modifications disagree.

For M1 that cuts both ways. Cell-level three-way merge is real, so task
edits made on different machines converge without review when they land on
different columns. But a `tasks` write path that stamps `updated_at` on
every mutation reintroduces the conflict for every same-row race, and the
clean merge is unreviewed: §4's data loss (`done` → `open`) is prevented by
the conflict existing, whereas the per-cell merge applies both writes with
no operator in the loop. Which of the two the product wants for a direct
lane is a §6.3 policy question this spike does not decide.

## 6. Scope consequence

This spike changes only the §6.3 design assumption:

- a nonzero merge requires inspection of both Dolt surfaces;
- a conflict-resolver exit code cannot clear a UNIQUE violation;
- `updated_at` is evidence for review, not a whole-row winner selector;
- merge concludes only after the relevant conflict and violation tables are
  empty and table constraints verify;
- cell-level three-way merge holds: disjoint cell edits to one row merge
  clean and unreviewed, so what reaches the review lane is decided by which
  columns each write path touches.

M1 review, elicitation, sync, and merge machinery remain unimplemented.
