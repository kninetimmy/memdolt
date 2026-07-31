//go:build golden

package golden

import (
	"context"
	"database/sql"
	"fmt"
)

// seedRows is this rig's hermetic fixture corpus, ported faithfully from
// memhub v0.2.0's tests/retrieval/retrieval_golden_hermetic.rs
// (seed_hermetic_corpus + seed_global_store): the same titles/rationales/
// bodies those functions pass to fact::add / decision::add / task::add /
// doc::add, copied verbatim so this rig's fixture reproduces the content
// the golden set's matchers were written against, not paraphrased text
// that merely happens to pass. Every row a golden query targets (§1 of
// this issue's acceptance criteria) is present, including all three
// doc_chunk rows chunk_markdown actually produces from the ingested
// markdown (not just the one a golden query targets — see the doc_chunk
// rows below) and the two global-scope rows (seeded here as scope-tagged
// rows in the same store — see schema.go's doc comment).
//
// decision.summary strings are the real backfilled summaries memhub's
// production baseline carries for the four "semantic-" queries that target
// them (decisions 28, 66, 33, 64 in memhub's own numbering) — without them
// the cross-encoder rerank score for those paraphrases falls below
// min_rerank_score and the query returns zero hits, exactly as documented
// in retrieval_golden_hermetic.rs.
func seedRows() []corpusRow {
	return []corpusRow{
		// -- facts (repo scope) ----------------------------------------
		{sourceType: "fact", id: "fact-build", scope: "repo",
			title: "build-command", body: "cargo build"},
		{sourceType: "fact", id: "fact-test", scope: "repo",
			title: "test-command", body: "cargo test"},

		// -- decisions (repo scope) -------------------------------------
		{sourceType: "decision", id: "dec-recall-readonly", scope: "repo",
			title: "memhub recall is read-only and never writes to writes_log",
			body: "Recall fetches FTS hits per source table, computes brute-force " +
				"cosine over the active-model embeddings (hybrid only), blends " +
				"them via the scoring config, and returns a ranked bundle. No row " +
				"in writes_log, no durable mutation, no pending_writes entry. " +
				"Addendum §8 says 'read-only' but codifying it as a decision " +
				"because the natural temptation when adding observability would " +
				"be to log every recall call, and that would distort memhub stats " +
				"and writes_log activity metrics. Logging belongs to writers; " +
				"recall is a reader."},
		{sourceType: "decision", id: "dec-stale-embed", scope: "repo",
			title: "Stale embedding warnings surface before /reindex, never auto-run",
			body: "When recall detects a content-hash mismatch on a row's embedding " +
				"it returns a stale_embeddings warning instead of silently " +
				"degrading. The agent must surface that warning and ask the user " +
				"before invoking /reindex; recall results stay usable meanwhile."},
		{sourceType: "decision", id: "dec-prefer-recall", scope: "repo",
			title: "Agents prefer recall over reading PROJECT_LEDGER.md",
			body: "Load-bearing rule for the token-savings win. Encoded in CLAUDE.md " +
				"and the existing skills: at session start read PROJECT.md only; " +
				"reach for the ledger only after recall comes up short."},
		{sourceType: "decision", id: "dec-fts5", scope: "repo",
			title: "FTS5 virtual tables attached to source tables",
			body: "Contentless FTS5 over facts.body, decisions.rationale, and " +
				"tasks.body. Triggers keep FTS indexes synced with source on " +
				"insert/update/delete. No data duplication."},
		{sourceType: "decision", id: "dec-bundle-bge", scope: "repo",
			title: "Bundle BGE-small via build.rs auto-download into OUT_DIR",
			body: "build.rs fetches model.onnx + 4 tokenizer files from " +
				"BAAI/bge-small-en-v1.5@main on first build, verifies each against " +
				"a pinned SHA256, and short-circuits on cache hit. Rejected the " +
				"manual-fetch-script alternative because contributor ergonomics " +
				"favor zero-setup cargo build. model.onnx SHA256 (828e1496...cf35, " +
				"133 MB) came from HF's x-linked-etag; tokenizer file hashes " +
				"computed locally over downloaded bytes. Decision 23 said " +
				"'bundled in binary' without specifying how; this is the " +
				"implementation choice."},
		{sourceType: "decision", id: "dec-eager-embed", scope: "repo",
			title: "Eager-embed on writes inside the same transaction",
			body: "Fact, decision, and task add paths re-embed the affected row " +
				"synchronously. ~50ms write overhead is acceptable for the " +
				"consistency guarantee. Avoids a background queue and stale-index " +
				"window."},
		{sourceType: "decision", id: "dec-recall-metric", scope: "repo",
			title: "Eval metric: Recall@3 via tests/retrieval_golden.json",
			body: "Single-number test: across the golden queries, what fraction had " +
				"the expected row in the top 3 results?"},
		{sourceType: "decision", id: "dec-source-vocab", scope: "repo",
			title: "Source vocabulary is writer-enforced, schema stays unconstrained TEXT",
			body: "The source-vocabulary addendum already specified writer " +
				"enforcement of the user / git / observed / agent:<id> / " +
				"user+agent:<id> vocabulary; until this session, CLI and " +
				"acceptance paths accepted any string, so typos like " +
				"user+agnet:codex silently persisted. validate_source() now lives " +
				"in commands::mod and is called from fact::add_in_tx and " +
				"decision::add_with_decided_at_in_tx, so both CLI writes and " +
				"review acceptance go through the same gate. The schema stays " +
				"unconstrained TEXT on purpose: enforcement lives at the writer " +
				"layer where it can evolve without migrations."},
		{sourceType: "decision", id: "dec-content-hash-drift", scope: "repo",
			title: "content_hash drift detection per embedding",
			body: "Store a hash of source body alongside each vector. Mismatch on " +
				"read marks the embedding stale and triggers re-embed on next " +
				"eager-embed pass or forces a /reindex prompt.",
			summary: "How does memhub know when a stored embedding has gone stale " +
				"or out of date relative to the source row it embeds?"},
		{sourceType: "decision", id: "dec-mcp-trust-split", scope: "repo",
			title: "MCP tool trust split: direct writes for intent, staged writes for claims",
			body: "Tasks are intent that the user prunes; session notes are scratch; " +
				"render regenerates from the DB — all low-trust and worth direct " +
				"MCP tools. Facts and decisions are claims about reality and need " +
				"the user-approval staging gate; bypassing it via direct MCP " +
				"fact_add / decision_add would erode the 'agents are untrusted " +
				"writers' principle that makes memhub trustworthy as a " +
				"multi-agent store. Codified by which MCP tools exist (task_add, " +
				"task_done, list_facts, render direct; propose_fact, " +
				"propose_decision staged)."},
		{sourceType: "decision", id: "dec-export-cross-machine", scope: "repo",
			title: "Export format covers session_notes, project_state, and project_arch alongside durable rows",
			body: "Cross-machine memory transfer use case: exporting only " +
				"facts/decisions/tasks/commands/pending_writes/writes_log lost " +
				"the 'what I was thinking when I left off' context (session_notes) " +
				"and the current-state narratives (project_state, project_arch) " +
				"that drive .memhub/rendered/PROJECT.md. Added as additive fields " +
				"with #[serde(default)] in src/export/v1.rs so older exports " +
				"continue to import cleanly. Excluded by design: commits, files, " +
				"chunks, embeddings (all derivable on the target via memhub " +
				"index).",
			summary: "Can I move my project memory between machines? How does " +
				"cross-machine transfer of memhub state work, and what does " +
				"the export format include?"},
		{sourceType: "decision", id: "dec-zero-result", scope: "repo",
			title: "Zero-result behavior: empty bundle, no automatic fallback",
			body: "When recall finds no matches it returns an empty results array, " +
				"not an automatic dump of PROJECT_LEDGER.md. The agent decides " +
				"whether to read the ledger as fallback based on the question.",
			summary: "What does memhub return when a query matches nothing? What " +
				"is the empty-bundle or zero-result behavior?"},
		{sourceType: "decision", id: "dec-machine-local", scope: "repo",
			title: "Memhub runtime and render output are machine-local by default",
			body: "Cross-machine Git conflicts showed that committing DB-derived " +
				"render output makes generated local state look like shared " +
				"source. Going forward, .memhub/ stays ignored, render defaults " +
				"to .memhub/rendered/, legacy agent_docs/PROJECT*.md render paths " +
				"are ignored, and repos must explicitly opt in if they want " +
				"tracked rendered markdown.",
			summary: "Which files and directories stay local to your laptop and " +
				"are not pushed to git? What does memhub keep machine-local " +
				"by default?"},

		// -- tasks (repo scope) -------------------------------------------
		{sourceType: "task", id: "task-pr6-eval", scope: "repo",
			title: "PR6: eval harness — golden queries + /eval-recall skill",
			body: "tests/retrieval_golden.json with 12 seeded queries. memhub " +
				"eval retrieval command computes Recall@3. /eval-recall skill " +
				"invokes it and reports the number. Acceptance gate for M8: " +
				"harness exists and reports a baseline."},

		// -- doc chunk (repo scope) -----------------------------------------
		// memhub's doc::add runs the ingested markdown through
		// chunk_markdown (src/commands/doc.rs ~line 541): one chunk per
		// ATX heading section, heading line kept in the body, heading_path
		// carrying the full ancestor breadcrumb. The source markdown
		// (retrieval_golden_hermetic.rs's seed_hermetic_corpus) is:
		//
		//   # Rust Code Style Guide
		//
		//   ## Error Handling
		//
		//   New fallible functions return `crate::Result<T>`. Never call
		//   `unwrap()` outside tests. Convert an IO failure with `map_err`
		//   into `MemhubError::InvalidInput` and propagate it upward with
		//   the `?` operator so the caller decides how to recover.
		//
		//   ## Naming
		//
		//   Functions are snake_case verbs; modules are nouns. Avoid
		//   abbreviations in any public API signature.
		//
		// which chunk_markdown splits into exactly three chunks (verified
		// by hand-tracing that function against this text) — all three are
		// seeded here, not just the one the golden queries target, because
		// a faithful port means the *other* rows a real ingest would also
		// produce are present in the candidate pool too. title is
		// pre-hydrated as "{doc title} — {heading_path}", mirroring
		// recall.rs's load_source_row for DocChunk; headingPath is the raw
		// breadcrumb chunk_markdown produces, which is what
		// doc_chunk_embed_text (persist.rs) and the FULLTEXT/BM25 lexical
		// gather actually index — see pipeline.go's embedText.
		{sourceType: "doc_chunk", id: "doc-chunk-title", scope: "repo", ord: 0,
			title:       "Rust Code Style Guide — Rust Code Style Guide",
			headingPath: "Rust Code Style Guide",
			body:        "# Rust Code Style Guide"},
		{sourceType: "doc_chunk", id: "doc-error-handling", scope: "repo", ord: 1,
			title:       "Rust Code Style Guide — Rust Code Style Guide > Error Handling",
			headingPath: "Rust Code Style Guide > Error Handling",
			body: "## Error Handling\n\nNew fallible functions return `crate::Result<T>`. Never call " +
				"`unwrap()` outside tests. Convert an IO failure with `map_err` " +
				"into `MemhubError::InvalidInput` and propagate it upward with " +
				"the `?` operator so the caller decides how to recover."},
		{sourceType: "doc_chunk", id: "doc-naming", scope: "repo", ord: 2,
			title:       "Rust Code Style Guide — Rust Code Style Guide > Naming",
			headingPath: "Rust Code Style Guide > Naming",
			body: "## Naming\n\nFunctions are snake_case verbs; modules are nouns. Avoid " +
				"abbreviations in any public API signature."},

		// -- global-scope rows (Wave 4 R10 / issue #74's seed_global_store) --
		{sourceType: "fact", id: "global-fact-signing-key", scope: "global",
			title: "global-git-signing-key",
			body: "Sign every commit on this machine with GPG key 4F2C9B1E, " +
				"regardless of which repo it is in"},
		{sourceType: "decision", id: "global-dec-editor-indent", scope: "global",
			title: "Machine-wide editor default: 2-space indent for every repo",
			body: "Applies across every project on this machine regardless of a " +
				"given repo's own style guide; promoted to global scope so it " +
				"never needs to be re-declared per repo."},
	}
}

// insertRows writes rows into the disposable store's PRD §6.1-shaped
// tables. facts/decisions carry the `scope` column this rig adds (see
// schema.go); tasks and the doc_chunks/documents pair have none, since no
// golden query targets a global task or doc.
func insertRows(ctx context.Context, db *sql.DB, rows []corpusRow) error {
	var docID string
	for _, r := range rows {
		switch r.sourceType {
		case "fact":
			if _, err := db.ExecContext(ctx,
				"INSERT INTO facts (id, `key`, value, source, scope, verified_at, created_at) "+
					"VALUES (?, ?, ?, 'user', ?, NOW(), NOW())",
				r.id, r.title, r.body, r.scope); err != nil {
				return fmt.Errorf("insert fact %s: %w", r.id, err)
			}
		case "decision":
			var summary any
			if r.summary != "" {
				summary = r.summary
			}
			if _, err := db.ExecContext(ctx,
				"INSERT INTO decisions (id, title, rationale, summary, status, source, scope, decided_at) "+
					"VALUES (?, ?, ?, ?, 'active', 'user+agent:claude-code', ?, NOW())",
				r.id, r.title, r.body, summary, r.scope); err != nil {
				return fmt.Errorf("insert decision %s: %w", r.id, err)
			}
		case "task":
			if _, err := db.ExecContext(ctx,
				"INSERT INTO tasks (id, title, status, notes, created_at, updated_at) "+
					"VALUES (?, ?, 'open', ?, NOW(), NOW())",
				r.id, r.title, r.body); err != nil {
				return fmt.Errorf("insert task %s: %w", r.id, err)
			}
		case "doc_chunk":
			if docID == "" {
				docID = "doc-code-style-guide"
				if _, err := db.ExecContext(ctx,
					"INSERT INTO documents (id, path, title, content_hash, byte_len, source, ingested_at) "+
						"VALUES (?, 'code-style-guide.md', 'Rust Code Style Guide', ?, 0, 'cli:user', NOW())",
					docID, fmt.Sprintf("%064d", 0)); err != nil {
					return fmt.Errorf("insert document: %w", err)
				}
			}
			if _, err := db.ExecContext(ctx,
				"INSERT INTO doc_chunks (id, doc_id, ord, heading_path, body) VALUES (?, ?, ?, ?, ?)",
				r.id, docID, r.ord, r.headingPath, r.body); err != nil {
				return fmt.Errorf("insert doc_chunk %s: %w", r.id, err)
			}
		default:
			return fmt.Errorf("insert: unknown source type %q", r.sourceType)
		}
	}
	return nil
}
