---
name: check-init
description: Check whether the current repository's memdolt memory is healthy.
compatibility: codex
---

# Check memdolt

Call the read-only MCP `status` tool and report its schema, store, and warning
fields. If the tool is unavailable, tell the user to run `memdolt doctor`; do
not initialize, migrate, or write memory from this skill.
