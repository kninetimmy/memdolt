# memdolt server instructions v1

- On turn one, read `.memdolt/rendered/PROJECT.md` once for project context.
- Recall relevant memory before reading `PROJECT_LEDGER.md`; use the ledger only
  when recall is empty or the user explicitly asks for it.
- Use `render` to refresh these local files at the configured output directory
  (default `.memdolt/rendered`). It reads one committed main snapshot and does
  not flush queued notes or promote proposals. Inspect reported outputs and
  backups before retrying any partial failure or unknown response.
- To find code by intent, use `locate` before grep. Use grep only to confirm or
  narrow the files that locate returns.
  Before issue #138 this named an absent tool; after it the real local locator
  lazily refreshes tracked source in separate SQLite and returns at most six
  lines and 400 characters per snippet. It never writes memory or flushes notes.
  Treat breadcrumbs as leads: metadata-preserving edits can remain unseen, and
  fusion has no nonsense floor. Report path, deny-rule and model failures.
- Never write durable facts or decisions directly. Stage claims with
  `propose_fact`, `propose_decision`, or `propose_supersede`; a human promotes
  them through review.
  Before issue #139 ordinary human fact/decision CLI commands were deferred;
  after it they exist for trusted terminal use. Agents must still use the
  reviewed lane, including for verification, summary edits and supersession.
  Source labels and `--actor user` do not authorize bypassing that gate.
- File facts under an existing dotted namespace such as `build.*`,
  `convention.*`, `env.*`, or `gotcha.*` instead of inventing an ad hoc key.
- Facts state what is true. Decisions record what was chosen and why. If the
  claim has a “because,” file it as a decision.
