package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/retrieval"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

// Temporary roots can themselves be OS aliases (macOS /var). Fixtures use
// their canonical directory; production bundle paths still refuse links.
func interopTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInteropCLIThroughDirectAndAuthenticatedOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", owner), func(t *testing.T) {
			source, target := initStore(t), initStore(t)
			if owner {
				serveStore(t, source)
				serveStore(t, target)
			}
			runMemdolt(t, "index", "rebuild", "--dir", target)
			seedRender(t, source)
			// Existing document content and local artifacts must survive a fresh
			// memory import, including a real owner credential in the routed case.
			docFile := filepath.Join(interopTempDir(t), "reference.md")
			writeTestFile(t, docFile, "# Retained\n\nExisting target reference document.\n")
			doc := decodeJSON[localdolt.DocResult](t, runMemdolt(t, "doc", "add", docFile, "--dir", target, "--json"))
			configPath := filepath.Join(target, ".memdolt", "config.toml")
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(target, ".memdolt", "local-sentinel"), "retain local artifacts")
			indexPath := filepath.Join(target, ".memdolt", "embeddings.sqlite")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "render", "--dir", target)
			renderPath := filepath.Join(target, ".memdolt", "rendered", "PROJECT.md")
			rendered, err := os.ReadFile(renderPath)
			if err != nil {
				t.Fatal(err)
			}
			before := interopQueryStrings(t, target, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash")
			file := filepath.Join(interopTempDir(t), "interop.json")
			exported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "export", file, "--dir", source, "--json"))
			if !exported.Written || exported.SourceCommit == "" {
				t.Fatalf("CLI export=%+v", exported)
			}
			imported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", file, "--dir", target, "--json"))
			if imported.Status != "imported" || imported.MainCommit == "" || imported.RetainedDocuments != 1 || imported.RetainedDocChunks != doc.Document.ChunkCount || len(imported.CreatedProposals) != 1 {
				t.Fatalf("CLI import=%+v", imported)
			}
			if after := interopQueryStrings(t, target, "SELECT commit_hash FROM dolt_log ORDER BY commit_hash"); len(after) != len(before)+1 {
				t.Fatal("CLI fabricated historical commits")
			}
			shown := runMemdolt(t, "review", "show", imported.CreatedProposals[0].ID, "--dir", target, "--json")
			if !strings.Contains(shown, "NEVER_RENDER_PENDING") {
				t.Fatalf("pending import is not reviewable: %s", shown)
			}
			if facts := interopQueryStrings(t, target, "SELECT `key` FROM facts"); len(facts) != 2 {
				t.Fatalf("import promoted pending payload: %v", facts)
			}
			for _, table := range []string{"facts", "decisions", "tasks", "session_notes", "project_state", "project_arch"} {
				first := interopQueryStrings(t, source, "SELECT id FROM "+table+" ORDER BY id")
				second := interopQueryStrings(t, target, "SELECT id FROM "+table+" ORDER BY id")
				if !reflect.DeepEqual(first, second) {
					t.Fatalf("CLI %s IDs changed", table)
				}
			}
			if actual, err := os.ReadFile(configPath); err != nil || !bytes.Equal(actual, config) {
				t.Fatal("import changed local config")
			}
			for path, before := range map[string][]byte{indexPath: index, renderPath: rendered} {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
					t.Fatalf("import changed existing derived/render artifact %s", path)
				}
			}
			if actual, err := os.ReadFile(filepath.Join(target, ".memdolt", "local-sentinel")); err != nil || string(actual) != "retain local artifacts" {
				t.Fatal("import changed local artifact")
			}
			if text := runMemdolt(t, "doc", "show", doc.Document.ID, "--dir", target); !strings.Contains(text, "Existing target reference") {
				t.Fatal("import lost retained document")
			}
			response := decodeJSON[retrieval.Response](t, runMemdolt(t, "recall", "Open task", "--source-type", "task", "--mode", "fts", "--provenance", "--dir", target, "--json"))
			if len(response.Results) == 0 || response.Results[0].LastChanged == nil || response.Results[0].LastChanged.Hash != imported.MainCommit {
				t.Fatalf("production recall lost import provenance: %+v", response)
			}
			// A real CLI review can accept the ordinary imported proposal; the
			// explicit synthetic force skips inference but retains all other guards.
			runMemdolt(t, "review", "accept", imported.CreatedProposals[0].ID, "--force", "--dir", target)
			if facts := interopQueryStrings(t, target, "SELECT `key` FROM facts"); len(facts) != 3 {
				t.Fatal("ordinary CLI review did not promote the imported proposal")
			}
		})
	}
}

func interopQueryStrings(t *testing.T, base, query string) []string {
	t.Helper()
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := st.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestInteropCLILegacyAndFailuresEmitOneResult(t *testing.T) {
	base := initStore(t)
	raw, err := os.ReadFile(repoFile("internal", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(interopTempDir(t), "legacy.json")
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	result := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", "--from-memhub", fixture, "--dir", base, "--json"))
	if result.MainCommit == "" || result.Counts["session_notes"] != 3 {
		t.Fatalf("CLI legacy import=%+v", result)
	}
	for _, args := range [][]string{
		{"import", "--from-memhub", fixture, "--dir", base, "--json"},
		{"import", fixture, "--dir", initStore(t), "--json"},
		{"import", "--dir", initStore(t), "--json"},
		{"export", filepath.Join(base, ".memdolt", "server.pid"), "--dir", base, "--json"},
		{"export", "--dir", base, "--json"},
		{"import", fixture, "--dir", interopTempDir(t), "--json"},
	} {
		var out bytes.Buffer
		cmd := newRootCommand()
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted invalid CLI operation: %v", args)
		}
		var failed localdolt.InteropResult
		if err := json.Unmarshal(out.Bytes(), &failed); err != nil || failed.Status != "refused" || failed.Error == "" || failed.MainCommit != "" {
			t.Fatalf("failure is not one honest JSON result: %s, %v", out.String(), err)
		}
	}
}

func TestInteropCLITaggedMemhubSchemas(t *testing.T) {
	raw, err := os.ReadFile(repoFile("internal", "store", "localdolt", "testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"0023_session_transcripts", "0024_session_note_provenance"} {
		for _, owner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%t", schema, owner), func(t *testing.T) {
				source := decodeJSON[map[string]any](t, string(raw))
				if source["source_schema_version"] != "0024_session_note_provenance" {
					t.Fatal("fixture must reproduce the tagged exporter header; see testdata/README.md")
				}
				data := raw
				if schema == "0023_session_transcripts" {
					source["source_schema_version"], source["exported_by"] = schema, "memhub 0.2.0"
					for _, note := range source["session_notes"].([]any) {
						for _, column := range []string{"session_id", "agent_id", "provider_id", "model_id", "variant"} {
							delete(note.(map[string]any), column)
						}
					}
					data, err = json.Marshal(source)
					if err != nil {
						t.Fatal(err)
					}
				}
				file := filepath.Join(interopTempDir(t), "legacy.json")
				writeTestFile(t, file, string(data))
				target := initStore(t)
				var stop func()
				if owner {
					stop = serveTransferProcess(t, target)
				}
				out, err := interopProcess(t, "import", "--from-memhub", file, "--dir", target, "--json")
				if err != nil {
					t.Fatalf("tagged import: %s, %v", out, err)
				}
				imported := decodeJSON[localdolt.InteropResult](t, out)
				if imported.Status != "imported" || imported.MainCommit == "" || len(imported.IdentityMap) != 17 || len(imported.CreatedProposals) != 2 || len(imported.RemainingProposals) != 0 {
					t.Fatalf("tagged import result=%+v", imported)
				}
				// Import and export must own the store in distinct processes, even
				// through IPC, so Dolt's warmed node cache cannot mask lost payloads.
				if owner {
					stop()
					defer serveTransferProcess(t, target)()
				}
				before := interopRepoSnapshot(t, target)
				native := filepath.Join(interopTempDir(t), "native.json")
				out, err = interopProcess(t, "export", native, "--dir", target, "--json")
				if err != nil {
					t.Fatalf("reopened native export: %s, %v", out, err)
				}
				exported := decodeJSON[localdolt.InteropResult](t, out)
				if !exported.Written || exported.SourceCommit != imported.MainCommit {
					t.Fatalf("native export result=%+v", exported)
				}
				contents, err := os.ReadFile(native)
				if err != nil {
					t.Fatal(err)
				}
				bundle := decodeJSON[localdolt.InteropBundle](t, string(contents))
				if bundle.MainCommit != imported.MainCommit || len(bundle.Proposals) != 2 || len(bundle.Tables["proposals"]) != 0 || len(bundle.Tables["facts"]) != 2 || len(bundle.Tables["decisions"]) != 2 {
					t.Fatal("export lost the import commit or promoted pending memory")
				}
				rows := map[string]localdolt.InteropRow{}
				for table, group := range bundle.Tables {
					key := "id"
					if table == "commands" {
						key = "kind"
					}
					for _, row := range group {
						rows[table+":"+*row[key]] = row
					}
				}
				for i, proposal := range bundle.Proposals {
					created := imported.CreatedProposals[i]
					if proposal.ID != created.ID || proposal.Head != created.Commit || proposal.Parent != imported.MainCommit || len(proposal.Changes) != 1 || proposal.Changes[0].From != nil {
						t.Fatal("export changed pending identity, ancestry or insert shape")
					}
					rows["pending_writes:"+proposal.ID] = proposal.Metadata
					change := proposal.Changes[0]
					rows[change.Table+":"+*change.To["id"]] = change.To
				}
				mapped := map[string]localdolt.InteropRow{}
				for _, identity := range imported.IdentityMap {
					row := rows[identity.Table+":"+identity.Value]
					if row == nil || row[identity.Key] == nil || *row[identity.Key] != identity.Value {
						t.Fatalf("lost row mapping: %+v", identity)
					}
					mapped[identity.Table+":"+identity.SourceID] = row
				}
				if len(rows) != 17 || len(mapped) != 17 || *mapped["commands:41"]["kind"] != "build" || *mapped["facts:11"]["superseded_by"] != *mapped["facts:12"]["id"] || *mapped["decisions:21"]["superseded_by"] != *mapped["decisions:22"]["id"] {
					t.Fatal("lost command key, row identity or supersession mapping")
				}
				for _, note := range source["session_notes"].([]any) {
					original := note.(map[string]any)
					row := mapped[fmt.Sprintf("session_notes:%.0f", original["id"])]
					for _, column := range []string{"text", "actor", "actor_raw", "session_id", "agent_id", "provider_id", "model_id", "variant"} {
						var value any
						if row[column] != nil {
							value = *row[column]
						}
						if value != original[column] {
							t.Fatalf("changed note %v column %s: %v != %v", original["id"], column, value, original[column])
						}
					}
				}
				genesis := *mapped["session_notes:import/genesis"]["text"]
				if !strings.Contains(genesis, fmt.Sprintf(`"source_schema_version": %q`, schema)) || !strings.Contains(genesis, imported.SourceDigest) || strings.Contains(genesis, "synthetic-old-host") {
					t.Fatal("genesis lost the declared schema/digest or imported the host root")
				}
				fact, decision := mapped["facts:pending_writes:81"], mapped["decisions:pending_writes:82"]
				if *fact["key"] != "convention.pending" || *fact["value"] != "Pending fact remains unaccepted" || *fact["kind"] != "convention" || fact["verified_at"] != nil || fact["superseded_by"] != nil ||
					*decision["title"] != "Pending decision remains unaccepted" || *decision["rationale"] != "Original proposed decision reasoning." || *decision["status"] != "active" || decision["summary"] != nil || decision["superseded_by"] != nil {
					t.Fatal("pending payload or exact NULLs changed")
				}
				if interopRepoSnapshot(t, target) != before {
					t.Fatal("native export changed destination memory or refs")
				}
				if after, err := os.ReadFile(file); err != nil || !bytes.Equal(after, data) {
					t.Fatal("import/export modified the legacy source file")
				}
			})
		}
	}
}

func TestInteropCLIReexportPendingDecision(t *testing.T) {
	for _, tc := range []struct{ owner, pending bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		t.Run(fmt.Sprintf("owner=%t/pending=%t", tc.owner, tc.pending), func(t *testing.T) {
			source, target := initStore(t), initStore(t)
			ctx := context.Background()
			st, err := openCommandStore(ctx, source, cliActor)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			// The smoke shape has no committed decisions. Keep complete rows in
			// every other memory table, including accepted proposal metadata,
			// empty strings, NULLs, counters and exact multiline provenance.
			if _, err := st.Commit(ctx, store.CommitRequest{
				Author: cliActor, Message: "synthetic native memory", Text: []string{"synthetic fixture"},
				Statements: []store.Statement{
					{SQL: "INSERT INTO facts (id, `key`, value, source, evidence) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F01', 'fixture.command', 'python -m unittest', 'user', '')"},
					{SQL: "INSERT INTO tasks (id, title, status, notes) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F02', 'completed task', 'done', NULL)"},
					{SQL: "INSERT INTO session_notes (id, text, actor, actor_raw, session_id, agent_id) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F03', ?, 'agent:codex', 'Codex', 'ses_fixture', '')", Args: []any{"Synthetic note.\r\n\r\nSpacing and Unicode: café → 🌳. "}},
					{SQL: "INSERT INTO session_notes (id, text, actor) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F04', '', 'user')"},
					{SQL: "INSERT INTO commands (kind, cmdline, last_exit_code, success_count, fail_count) VALUES ('test', 'python -m unittest', -1, 2147483647, 0)"},
					{SQL: "INSERT INTO project_state (id, body, actor) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F05', 'Synthetic current state', 'user')"},
					{SQL: "INSERT INTO project_arch (id, body, actor, actor_raw) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F06', 'Python standard library', 'user', '')"},
					{SQL: "INSERT INTO proposals (id, kind, rationale, actor, target) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5F07', 'fact', 'Synthetic accepted metadata', 'agent:codex', 'repo')"},
				},
			}); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if _, err := st.ProposeDecision(ctx, localdolt.Proposal{
					Actor: store.Actor{Name: "agent:codex", Email: "codex@memdolt.invalid"}, Target: localdolt.TargetRepo, Rationale: "Fixture decision.",
				}, localdolt.Decision{
					Title:     "Synthetic Orchard fixture uses Counter",
					Rationale: "The Python standard library provides deterministic counting without dependencies.",
					Evidence:  "docs/design.md, Fixture decision.",
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.Commit(ctx, store.CommitRequest{
				Author: cliActor, Message: "synthetic memory after proposal", Text: []string{"completed task"},
				Statements: []store.Statement{{SQL: "UPDATE tasks SET updated_at = '2026-09-08 16:29:40'"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(interopTempDir(t), "source.json")
			sourceBefore := interopRepoSnapshot(t, source)
			runMemdolt(t, "export", file, "--dir", source, "--json")
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			want := decodeJSON[localdolt.InteropBundle](t, string(original))
			for table, count := range map[string]int{"facts": 1, "decisions": 0, "tasks": 1, "commands": 1, "session_notes": 2, "project_state": 1, "project_arch": 1, "proposals": 1} {
				if len(want.Tables[table]) != count {
					t.Fatalf("source lost %s rows", table)
				}
			}
			pendingCount := 0
			if tc.pending {
				pendingCount = 1
			}
			if len(want.Proposals) != pendingCount {
				t.Fatal("source lost pending decision")
			}
			if tc.pending {
				proposal := want.Proposals[0]
				if proposal.Parent == want.MainCommit || len(proposal.Changes) != 1 || proposal.Changes[0].Table != "decisions" || proposal.Changes[0].From != nil {
					t.Fatal("fixture lost the older-parent pending decision insert")
				}
				row := proposal.Changes[0].To
				if len(row) != 10 || row["rationale"] == nil || *row["rationale"] != "The Python standard library provides deterministic counting without dependencies." ||
					row["summary"] != nil || row["alternatives_rejected"] != nil || row["superseded_by"] != nil {
					t.Fatal("source changed pending TEXT or NULLs")
				}
			}
			imported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", file, "--dir", target, "--json"))
			// Reopening in this process still shares Dolt's warmed global chunk
			// cache and masked the bug. Both routes need a fresh owning process.
			if tc.owner {
				defer serveTransferProcess(t, target)()
			}
			before := interopRepoSnapshot(t, target)
			reexport := filepath.Join(interopTempDir(t), "reexport.json")
			out, err := interopProcess(t, "export", reexport, "--dir", target, "--json")
			if err != nil {
				t.Fatalf("reexport: %s, %v", out, err)
			}
			result := decodeJSON[localdolt.InteropResult](t, out)
			if !result.Written || result.Status != "exported" || result.SourceCommit != imported.MainCommit || result.Error != "" {
				t.Fatalf("reexport result=%+v", result)
			}
			interopAssertBundle(t, reexport, want, imported)
			if after := interopRepoSnapshot(t, target); after != before {
				t.Fatal("export changed main, proposal heads, pending status or working roots")
			}
			fresh := initStore(t)
			reimported := decodeJSON[localdolt.InteropResult](t, runMemdolt(t, "import", reexport, "--dir", fresh, "--json"))
			final := filepath.Join(interopTempDir(t), "final.json")
			if out, err := interopProcess(t, "export", final, "--dir", fresh, "--json"); err != nil {
				t.Fatalf("export after further import: %s, %v", out, err)
			}
			interopAssertBundle(t, final, want, reimported)
			if after := interopRepoSnapshot(t, source); after != sourceBefore {
				t.Fatal("round trip changed source state")
			}
			if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, original) {
				t.Fatal("round trip changed original bundle")
			}
			foreign := filepath.Join(interopTempDir(t), "keep.txt")
			writeTestFile(t, foreign, "keep existing content")
			for _, invalid := range []string{foreign, filepath.Join(target, ".memdolt", "server.pid"), filepath.Join(interopTempDir(t), "missing", "bundle.json")} {
				out, err := interopProcess(t, "export", invalid, "--dir", target, "--json")
				failed := decodeJSON[localdolt.InteropResult](t, out)
				if err == nil || failed.Written || failed.Status != "refused" || failed.Error == "" {
					t.Fatalf("destination refusal=%+v, %v", failed, err)
				}
			}
			if data, err := os.ReadFile(foreign); err != nil || string(data) != "keep existing content" || interopRepoSnapshot(t, target) != before {
				t.Fatal("refused export changed file or repository")
			}
		})
	}
}

func interopRepoSnapshot(t *testing.T, base string) string {
	t.Helper()
	return runMemdolt(t, "repo", "status", "--local", "--dir", base, "--json") +
		strings.Join(interopQueryStrings(t, base, "SELECT CONCAT(name, ':', hash) FROM dolt_branches ORDER BY name"), "\n") +
		strings.Join(interopQueryStrings(t, base, "SELECT CONCAT(DOLT_HASHOF_DB('WORKING'), ':', DOLT_HASHOF_DB('STAGED'))"), "\n")
}

func interopAssertBundle(t *testing.T, file string, want localdolt.InteropBundle, imported localdolt.InteropResult) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSON[localdolt.InteropBundle](t, string(data))
	if imported.Status != "imported" || got.MainCommit != imported.MainCommit || !reflect.DeepEqual(got.Tables, want.Tables) || len(got.Proposals) != len(want.Proposals) || len(got.Proposals) != len(imported.CreatedProposals) {
		t.Fatal("native round trip changed committed rows, exact NULLs or pending count")
	}
	for i, proposal := range got.Proposals {
		original, created := want.Proposals[i], imported.CreatedProposals[i]
		if proposal.ID != original.ID || proposal.ID != created.ID || proposal.Head != created.Commit || proposal.Parent != imported.MainCommit ||
			!reflect.DeepEqual(proposal.Metadata, original.Metadata) || !reflect.DeepEqual(proposal.Changes, original.Changes) {
			t.Fatal("native round trip changed pending identity, payload or nullable values")
		}
	}
}

func interopProcess(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestInteropHelperProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "MEMDOLT_INTEROP_HELPER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%w: %s", err, &stderr)
	}
	return stdout.String(), err
}

func TestInteropHelperProcess(t *testing.T) {
	if os.Getenv("MEMDOLT_INTEROP_HELPER") != "1" {
		return
	}
	root := newRootCommand()
	root.SetArgs(os.Args[3:])
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
