---
name: recall
description: Recall relevant durable memdolt memory for the current task.
framework: memdolt
---

# Recall memdolt memory

Call the MCP `recall` tool with the user's query. Preserve result provenance
and warnings in the answer. Use `search` only when the user specifically wants
committed decision text. Do not infer an empty result means the memory is true
or false, and do not write anything.
