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
- For repository transfers requested by the operator, use `repo_pull` and
  `repo_push` over the configured remote. Before issue #137 these tools were
  absent; now compatible divergence merges committed history as the requesting
  agent. Actual conflicts need explicit human form choices over the displayed
  rows and blame; never synthesize a confirmation or choose by timestamp.
  Every choice stays pending until the complete merge validates and commits as
  user. Preserve done tasks unless the human explicitly reopens them; fact
  losers are superseded and retained. Follow nextCursor after nine modern
  forms to reach remaining conflicts within two minutes; legacy clients receive
  one complete form. Expiry, cancellation, restart or changed heads needs fresh
  review. Without form support, report `memdolt pull --json` and
  `memdolt pull --resolve <file>`. Inspect local/remote main after an unknown
  reply; never replay automatically. Transfer does not flush queued notes,
  promote pending/global proposals or refresh derived/rendered files.
  Before issue #137's cycle-1 correction, malformed Unicode could be replaced
  during JSON decoding and manual note choices could rewrite five provenance
  fields. Both now refuse: retain session/agent/provider/model/variant metadata
  as well as actor identity. Unchanged nullable fields and choosing an existing
  complete side remain valid. Absent row images are omitted; nullable cells in
  present rows remain explicit. The shipped stdio reader checks raw Unicode
  before SDK conversion; valid U+FFFD is allowed, and text a client already
  replaced before transmission cannot be reconstructed.
