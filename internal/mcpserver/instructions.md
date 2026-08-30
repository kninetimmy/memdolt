# memdolt server instructions v1

- On turn one, read `.memdolt/rendered/PROJECT.md` once for project context.
- Recall relevant memory before reading `PROJECT_LEDGER.md`; use the ledger only
  when recall is empty or the user explicitly asks for it.
- To find code by intent, use `locate` before grep. Use grep only to confirm or
  narrow the files that locate returns.
- Never write durable facts or decisions directly. Stage claims with
  `propose_fact`, `propose_decision`, or `propose_supersede`; a human promotes
  them through review.
- File facts under an existing dotted namespace such as `build.*`,
  `convention.*`, `env.*`, or `gotcha.*` instead of inventing an ad hoc key.
- Facts state what is true. Decisions record what was chosen and why. If the
  claim has a “because,” file it as a decision.
