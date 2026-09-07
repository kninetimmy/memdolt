INSERT INTO project_state (id, body, actor, actor_raw, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F01', 'Earlier state', 'user', NULL, '2025-01-01');
INSERT INTO project_state (id, body, actor, actor_raw, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F02', 'Building durable memory

Keep this paragraph and its spacing.', 'agent:codex', 'Codex', '2026-01-01');
INSERT INTO project_arch (id, body, actor, actor_raw, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F03', 'Local owner → committed Dolt

One path for CLI and MCP.', 'agent:opencode', 'cli', '2026-01-01');
INSERT INTO session_notes (id, text, actor, actor_raw, created_at, session_id, agent_id, provider_id, model_id, variant) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F04', 'Recorded session

Second paragraph stays.', 'agent:opencode', 'cli', '2026-01-01', 'ses_fixture', 'build', 'fixture-provider', 'fixture-model', 'precise');
INSERT INTO facts (id, `key`, value, source, kind, evidence, verified_at, created_at, superseded_by) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F05', 'build.command', 'old | command
second line', 'agent:codex', 'command', 'old evidence', NULL, '2025-01-01', '01ARZ3NDEKTSV4RRFFQ69G5F06');
INSERT INTO facts (id, `key`, value, source, kind, evidence, verified_at, created_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F06', 'build.command', 'go test ./...', 'user', 'command', 'AGENTS.md', '2020-01-01', '2025-01-02');
INSERT INTO decisions (id, title, rationale, summary, alternatives_rejected, evidence, status, source, decided_at, superseded_by) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F07', 'Earlier choice', 'Because the original tool fit.

Keep historical rationale.', 'Historical summary', 'shared writable files', 'prior evidence', 'superseded', 'agent:codex', '2025-01-01', '01ARZ3NDEKTSV4RRFFQ69G5F08');
INSERT INTO decisions (id, title, rationale, status, source, decided_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F08', 'Use the owner', 'Because one process owns storage.', 'active', 'user', '2026-01-01');
INSERT INTO tasks (id, title, status, notes, created_at, updated_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F09', 'Open task', 'open', 'Do this first.

Preserve detail.', '2025-01-01', '2025-01-01');
INSERT INTO tasks (id, title, status, notes, created_at, updated_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F0A', 'Blocked task', 'blocked', 'Waiting for evidence.', '2025-01-01', '2026-01-01');
INSERT INTO tasks (id, title, status, notes, created_at, updated_at) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F0B', 'Done task', 'done', NULL, '2025-01-01', '2026-02-01');
