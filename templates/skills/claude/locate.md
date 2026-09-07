---
name: locate
description: Find local code by intent with bounded source breadcrumbs.
framework: memdolt
---

Call the MCP `locate` tool with the user's code intent before grep. It lazily
refreshes the local tracked-source index and returns path, line, symbol and kind
breadcrumbs with snippets capped at six lines and 400 characters. Read only the
relevant source ranges to confirm the result; a high rank is a lead, not proof.

Code lives only in `.memdolt/code_index.sqlite`, outside Dolt, recall, exports
and sync. Report warnings and refusals. Millisecond mtime/size is a fast path,
so edits preserving both can remain unseen. The CLI-only `--no-refresh` option
is stale by choice and still enforces source-read safety. Fusion has no score
floor and may return hits for nonsense. Do not write durable memory.
