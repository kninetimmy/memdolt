# memdolt server instructions v1

- On turn one, read `.memdolt/rendered/PROJECT.md` once for project context.
- Before #163 repository identity/topology were deferred. Now CLI and MCP share
  validated local/clone policy and committed Git-origin identity. Live topology
  refuses. Existing unidentified stores need explicit terminal init adoption
  with the owner stopped; never reassign a different identity or bypass refusal.
  Default origin comes from native configuration or [repo] remote_url; both
  must agree when present. Other explicitly named remotes retain their target.
  Local topology keeps ordinary repo_status offline. Explicit transfers still
  need operator intent. Startup pull defaults off; an enabled clone pulls once
  before this owner is published. Conflicts and unknown outcomes stop startup
  with an inspection remedy, not manufactured approval or automatic replay.
  Global stores keep separate identity/policy. Report confirmed hashes through
  later failures; changing topology deliberately requires an owner restart.
- Recall relevant memory before reading `PROJECT_LEDGER.md`; use the ledger only
  when recall is empty or the user explicitly asks for it.
  Before issue #146 recall used repository memory only. After it, enabled
  repositories combine committed global facts, decisions and documents in the
  same candidate pool and rerank pass. Report scope/snapshotCommit and scoped
  freshness warnings; a missing replica or competing owner is a visible error.
  Disabled global recall preserves the repository-only output. Human global
  CLI writes and promotion now exist; this grants no new agent write authority.
  Before #161 global-target acceptance refused even at the terminal. Now a
  trusted human can run `memdolt review accept <id> --dir <repository>` there.
  MCP elicitation still excludes global proposals. Never impersonate approval
  or replace review with a direct global write. Report native source/staging/
  acceptance hashes and retained source branches; inspect before any retry.
- Use `render` to refresh these local files at the configured output directory
  (default `.memdolt/rendered`). Before issue #145 it did not flush queued notes.
  Now it flushes this owner's session notes before reading one committed main
  snapshot; authenticated `memdolt render` reaches the same queue. A flush error
  prevents publication. Proposals remain excluded. Inspect reported outputs and
  backups before retrying any partial failure or unknown response.
  Before #145's review correction, a later render failure could hide notes just
  committed by its flush. Now `noteCommits` maps those note IDs to their confirmed
  hashes even when configuration, snapshot or file work fails. Inspect these notes
  and Dolt history as well as files; SourceCommit may still be unavailable.
- A nonempty commit hash confirms a durable write even with a late error.
  Inspect the returned row identity and Dolt history; never automatically replay
  it. A lost result means outcome unknown, not rollback. MCP removes confirmed
  note groups immediately, retries only known uncommitted groups on explicit
  render or orderly shutdown, and retains unknown groups only for inspection.
  Timer failures remain visible to the next render and shutdown. Close reports
  earlier/final failures and discards remaining rows; a crash loses the queue.
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
