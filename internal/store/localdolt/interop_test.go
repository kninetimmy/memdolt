package localdolt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
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

func legacyInteropFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "memhub-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func interopTestFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(interopTempDir(t), "memory.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func interopCaptured(t *testing.T, s *Store) InteropBundle {
	t.Helper()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := captureInterop(context.Background(), conn, interopHooks{})
	if err = errors.Join(err, conn.Close()); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestInteropLegacyAndNativeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := renderStore(t)
	fixture := legacyInteropFixture(t)
	file := interopTestFile(t, fixture)
	before := internalCount(t, s, "SELECT COUNT(*) FROM dolt_log")
	result, err := s.ImportMemory(ctx, ImportMemoryOptions{File: file, FromMemhub: true})
	if err != nil || result.Status != "imported" || result.MainCommit == "" || len(result.CreatedProposals) != 2 || len(result.RemainingProposals) != 0 {
		t.Fatalf("legacy import=%+v, %v", result, err)
	}
	if got := internalCount(t, s, "SELECT COUNT(*) FROM dolt_log"); got != before+1 {
		t.Fatalf("fabricated historical main commits: %d -> %d", before, got)
	}
	var author, message string
	if err := s.db.QueryRowContext(ctx, "SELECT committer, message FROM dolt_log WHERE commit_hash = ?", result.MainCommit).Scan(&author, &message); err != nil || author != "user" || !strings.Contains(message, result.SourceDigest) {
		t.Fatalf("import authorship=%s %s: %v", author, message, err)
	}
	mapped := map[string]string{}
	for _, identity := range result.IdentityMap {
		mapped[identity.Table+":"+identity.SourceID] = identity.Value
		if identity.Table == "commands" {
			if identity.Key != "kind" || identity.Value != "build" {
				t.Fatalf("invented command ULID: %+v", identity)
			}
		} else if identity.Key != "id" || !validInteropID(identity.Value) {
			t.Fatalf("invalid ULID mapping: %+v", identity)
		}
	}
	var by, note, rawActor string
	if err := s.db.QueryRowContext(ctx, "SELECT superseded_by FROM facts WHERE id = ?", mapped["facts:11"]).Scan(&by); err != nil || by != mapped["facts:12"] {
		t.Fatalf("fact supersession=%s, %v", by, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT superseded_by FROM decisions WHERE id = ?", mapped["decisions:21"]).Scan(&by); err != nil || by != mapped["decisions:22"] {
		t.Fatalf("decision supersession=%s, %v", by, err)
	}
	var session, agent, provider, model, variant sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT text, actor_raw, session_id, agent_id, provider_id, model_id, variant FROM session_notes WHERE id = ?", mapped["session_notes:51"]).Scan(&note, &rawActor, &session, &agent, &provider, &model, &variant); err != nil || note != "Exact original note.\r\n\r\nNo invented attribution. " || rawActor != "cli" || session.String != "ses_synthetic" || provider.String != "synthetic-provider" || model.String != "synthetic-model" || variant.String != "precise" {
		t.Fatalf("note/provenance lost: %q %s %+v %+v %+v %+v %+v, %v", note, rawActor, session, agent, provider, model, variant, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT session_id, agent_id, provider_id, model_id, variant FROM session_notes WHERE id = ?", mapped["session_notes:52"]).Scan(&session, &agent, &provider, &model, &variant); err != nil || session.Valid || !agent.Valid || agent.String != "" || provider.Valid || model.Valid || variant.Valid {
		t.Fatalf("NULL/empty provenance lost: %+v %+v %+v %+v %+v, %v", session, agent, provider, model, variant, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT text FROM session_notes WHERE id = ?", mapped["session_notes:import/genesis"]).Scan(&note); err != nil || !strings.Contains(note, result.SourceDigest) || !strings.Contains(note, `"writes_log": 2`) || !strings.Contains(note, `"rejected": 1`) || strings.Contains(note, "synthetic-old-host") {
		t.Fatalf("dishonest/host-bound genesis note: %s, %v", note, err)
	}
	if got := internalCount(t, s, "SELECT COUNT(*) FROM facts"); got != 2 {
		t.Fatalf("pending fact promoted: %d", got)
	}
	for _, proposal := range result.CreatedProposals {
		diff, err := s.ProposalDiff(ctx, proposal.ID)
		if err != nil || diff.Proposal.Commit != proposal.Commit || diff.Parent != result.MainCommit {
			t.Fatalf("unreviewable imported proposal: %+v %v", diff, err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT committer FROM DOLT_LOG(?) LIMIT 1", proposal.Commit).Scan(&author); err != nil || author != "user" {
			t.Fatalf("invented historical proposal author: %s, %v", author, err)
		}
	}
	// Fields that memhub never had must survive native interop without loss.
	if _, err := s.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), Message: "native durable fields", Text: []string{"evidence", "alternatives"}, Statements: []store.Statement{
		{SQL: "UPDATE facts SET evidence = '' WHERE id = ?", Args: []any{mapped["facts:12"]}},
		{SQL: "UPDATE decisions SET evidence = ?, alternatives_rejected = ? WHERE id = ?", Args: []any{"PR 140", "Other approach\n\nwas rejected.", mapped["decisions:22"]}},
	}}); err != nil {
		t.Fatal(err)
	}
	base, cfg := s.paths.Base(), s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if reopened.paths.Base() != base {
		t.Fatal("reopen changed destination")
	}
	first := interopCaptured(t, reopened)
	export := filepath.Join(interopTempDir(t), "native.json")
	if result, err := reopened.ExportMemory(ctx, ExportMemoryOptions{File: export}); err != nil || !result.Written {
		t.Fatalf("export=%+v, %v", result, err)
	}
	target := renderStore(t)
	imported, err := target.ImportMemory(ctx, ImportMemoryOptions{File: export})
	if err != nil || imported.Status != "imported" || len(imported.CreatedProposals) != 2 {
		t.Fatalf("native import=%+v, %v", imported, err)
	}
	second := interopCaptured(t, target)
	if !reflect.DeepEqual(first.Tables, second.Tables) {
		t.Fatal("native round trip changed canonical rows, counts or NULLs")
	}
	for i, p := range first.Proposals {
		if !reflect.DeepEqual(p.Metadata, second.Proposals[i].Metadata) || !reflect.DeepEqual(p.Changes, second.Proposals[i].Changes) {
			t.Fatal("native round trip lost pending payload")
		}
	}
	if original, err := os.ReadFile(file); err != nil || string(original) != string(fixture) {
		t.Fatal("import modified its source bundle")
	}
}

func TestInteropPrevalidationAndDestinationRefusals(t *testing.T) {
	ctx := context.Background()
	fixture := legacyInteropFixture(t)
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"unknown version", func(v map[string]any) { v["memhub_export_version"] = 2 }},
		{"future schema", func(v map[string]any) { v["source_schema_version"] = "999" }},
		{"unknown field", func(v map[string]any) { v["transcript_archive"] = "never-open-this" }},
		{"duplicate ID", func(v map[string]any) { v["facts"].([]any)[1].(map[string]any)["id"] = 11 }},
		{"dangling supersession", func(v map[string]any) { v["facts"].([]any)[0].(map[string]any)["superseded_by"] = 999 }},
		{"cyclic supersession", func(v map[string]any) { v["facts"].([]any)[1].(map[string]any)["superseded_by"] = 11 }},
		{"duplicate live key", func(v map[string]any) { v["facts"].([]any)[0].(map[string]any)["superseded_by"] = nil }},
		{"duplicate command kind", func(v map[string]any) {
			v["commands"] = append(v["commands"].([]any), map[string]any{"id": 42, "kind": "build", "cmdline": "other build", "last_exit_code": nil, "last_run_at": nil, "success_count": 0, "fail_count": 0})
		}},
		{"unsupported pending", func(v map[string]any) { v["pending_writes"].([]any)[0].(map[string]any)["kind"] = "task" }},
		{"global pending", func(v map[string]any) {
			v["pending_writes"].([]any)[0].(map[string]any)["payload_json"] = `{"key":"global.fact","value":"never promoted","target":"global"}`
		}},
		{"legacy supersede", func(v map[string]any) {
			p := v["pending_writes"].([]any)[2].(map[string]any)
			p["status"], p["reviewed_at"] = "pending", nil
		}},
		{"missing mandatory field", func(v map[string]any) { delete(v["tasks"].([]any)[0].(map[string]any), "title") }},
		{"fractional timestamp", func(v map[string]any) {
			v["tasks"].([]any)[0].(map[string]any)["created_at"] = "2026-01-01T00:00:00.1Z"
		}},
		{"oversized integer", func(v map[string]any) { v["commands"].([]any)[0].(map[string]any)["success_count"] = int64(2147483648) }},
		{"oversized annotation", func(v map[string]any) {
			v["pending_writes"].([]any)[0].(map[string]any)["provenance_json"] = `{"blob":"` + strings.Repeat("x", 65000) + `"}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := renderStore(t)
			before := transferMain(t, s)
			var input map[string]any
			if err := json.Unmarshal(fixture, &input); err != nil {
				t.Fatal(err)
			}
			tc.edit(input)
			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.ImportMemory(ctx, ImportMemoryOptions{File: interopTestFile(t, data), FromMemhub: true})
			if err == nil || result.Status != "refused" || result.MainCommit != "" || len(result.CreatedProposals) != 0 || transferMain(t, s) != before || internalCount(t, s, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatalf("invalid import mutated target: %+v, %v", result, err)
			}
		})
	}
	for _, state := range []string{"dirty", "staged", "nonempty", "pending", "schema", "denied", "denied provenance", "denied nested provenance"} {
		t.Run(state, func(t *testing.T) {
			s := renderStore(t)
			input := fixture
			switch state {
			case "dirty", "staged":
				if _, err := s.db.Exec("INSERT INTO tasks (id, title) VALUES (?, ?)", newID(), "dirty task"); err != nil {
					t.Fatal(err)
				}
				if state == "staged" {
					if _, err := s.db.Exec("CALL DOLT_ADD('.')"); err != nil {
						t.Fatal(err)
					}
				}
			case "nonempty":
				if _, _, err := memory.New(s, memory.UserActor).AddTask(ctx, "existing", ""); err != nil {
					t.Fatal(err)
				}
			case "pending":
				if _, err := s.ProposeFact(ctx, Proposal{Actor: s.cfg.Actor, Target: TargetRepo, Rationale: "existing"}, Fact{Key: "existing.pending", Value: "existing"}); err != nil {
					t.Fatal(err)
				}
			case "schema":
				if _, err := s.Commit(ctx, store.CommitRequest{Author: s.cfg.Actor, Message: "unsupported schema", NoText: true, Statements: []store.Statement{{SQL: "UPDATE meta SET v='3' WHERE k='schema_version'"}}}); err != nil {
					t.Fatal(err)
				}
			case "denied", "denied provenance", "denied nested provenance":
				pattern := "Exact original note"
				if state == "denied provenance" {
					pattern = "synthetic-provider"
				}
				if state == "denied nested provenance" {
					pattern = "blocked-provenance"
					var document map[string]any
					if err := json.Unmarshal(input, &document); err != nil {
						t.Fatal(err)
					}
					document["pending_writes"].([]any)[0].(map[string]any)["provenance_json"] = `{"value":"\u0062locked-provenance"}`
					var err error
					input, err = json.Marshal(document)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(s.paths.ConfigFile(), []byte("[deny_list]\npatterns = ['"+pattern+"']\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, dirty := transferMain(t, s), internalCount(t, s, "SELECT COUNT(*) FROM dolt_status")
			result, err := s.ImportMemory(ctx, ImportMemoryOptions{File: interopTestFile(t, input), FromMemhub: true})
			if err == nil || result.MainCommit != "" || transferMain(t, s) != before || internalCount(t, s, "SELECT COUNT(*) FROM dolt_status") != dirty {
				t.Fatalf("destination refusal=%+v, %v", result, err)
			}
		})
	}
}

func TestInteropRetainsConfirmedPartialProgress(t *testing.T) {
	for _, phase := range []string{"after main", "second proposal", "main finalize", "proposal finalize"} {
		t.Run(phase, func(t *testing.T) {
			s := renderStore(t)
			hooks := interopHooks{}
			failure := errors.New("synthetic import finalization failure")
			switch phase {
			case "after main":
				hooks.afterMain = func() error { return failure }
			case "second proposal":
				hooks.beforeProposal = func(i int) error {
					if i == 1 {
						return failure
					}
					return nil
				}
			case "main finalize":
				hooks.finalizeMain = func(tx *sql.Tx) error { return errors.Join(tx.Commit(), failure) }
			case "proposal finalize":
				hooks.finalizeStage = func(tx *sql.Tx) error { return errors.Join(tx.Commit(), failure) }
			}
			result, err := s.importMemory(context.Background(), ImportMemoryOptions{File: interopTestFile(t, legacyInteropFixture(t)), FromMemhub: true}, hooks)
			if !errors.Is(err, failure) || result.Status != "partial" || result.MainCommit == "" || len(result.IdentityMap) == 0 || transferMain(t, s) != result.MainCommit {
				t.Fatalf("lost committed import result: %+v, %v", result, err)
			}
			want := 0
			if phase == "second proposal" || phase == "proposal finalize" {
				want = 1
			}
			if len(result.CreatedProposals) != want || len(result.RemainingProposals) != 2-want {
				t.Fatalf("wrong exact prefix/suffix: %+v", result)
			}
			pending, err := s.PendingProposals(context.Background())
			if err != nil || len(pending) != want {
				t.Fatalf("partial proposals=%+v, %v", pending, err)
			}
			if want == 1 && !slices.ContainsFunc(pending, func(p PendingProposal) bool {
				return p.ID == result.CreatedProposals[0].ID && p.Commit == result.CreatedProposals[0].Commit
			}) {
				t.Fatal("reported proposal prefix differs from durable branches")
			}
		})
	}
}

func TestInteropOlderLegacyDefaultsAndMalformedJSON(t *testing.T) {
	var input map[string]any
	if err := json.Unmarshal(legacyInteropFixture(t), &input); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session_notes", "project_state", "project_arch"} {
		delete(input, field)
	}
	for _, fact := range input["facts"].([]any) {
		delete(fact.(map[string]any), "kind")
	}
	for _, decision := range input["decisions"].([]any) {
		delete(decision.(map[string]any), "summary")
		delete(decision.(map[string]any), "source")
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, _, err := decodeMemhubExport(data)
	if err != nil || len(bundle.Tables["session_notes"]) != 0 || len(bundle.Tables["project_state"]) != 0 || interopValue(bundle.Tables["decisions"][0], "source") != "user" || bundle.Tables["decisions"][0]["summary"] != nil {
		t.Fatalf("older-v1 defaults failed: %+v, %v", bundle, err)
	}
	for _, malformed := range [][]byte{[]byte(`{"a":1,"a":2}`), []byte(`{"a":{"b":1,"b":2}}`), []byte(`{} {}`), []byte(`{"a":`), {0xff}, []byte(`{"text":"\ud800"}`), []byte(`{"text":"\udc00"}`), []byte(`{"text":"\ud800\u0041"}`)} {
		var value any
		if err := decodeInteropJSON(malformed, &value); err == nil {
			t.Fatalf("accepted malformed/ambiguous JSON: %q", malformed)
		}
	}
	for _, stamp := range []string{"2026-01-01T00:00:00.1Z", "2026-01-01T00:00:00.0000000001Z"} {
		if _, err := memhubTimestamp(stamp); err == nil {
			t.Fatal("silently truncated fractional timestamp")
		}
	}
	invalid := emptyInteropRow("tasks")
	invalid["id"], invalid["notes"] = interopString(newID()), interopString(string([]byte{0xff}))
	if err := validateInteropRow("tasks", invalid); err == nil {
		t.Fatal("would export malformed native text through lossy json.Marshal")
	}
	var opaque map[string]string
	if err := decodeInteropJSON([]byte(`{"Model":"\ud83d\ude00\ufffd","model":"literal \\ud800"}`), &opaque); err != nil || opaque["Model"] != "😀�" || opaque["model"] != `literal \ud800` {
		t.Fatalf("rewrote valid Unicode or opaque case-sensitive keys: %+v, %v", opaque, err)
	}
}

func TestInteropUnicodeAndCaseAliasesRefuseBeforeWrites(t *testing.T) {
	ctx := context.Background()
	legacy := legacyInteropFixture(t)
	source := renderStore(t)
	if _, err := source.ImportMemory(ctx, ImportMemoryOptions{File: interopTestFile(t, legacy), FromMemhub: true}); err != nil {
		t.Fatal(err)
	}
	native, err := json.Marshal(interopCaptured(t, source))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, replacement string
		legacy                 bool
	}{
		{"native raw UTF-8", "Pending fact remains unaccepted", "bad\xfftext", false},
		{"native pending surrogate", "Pending fact remains unaccepted", `\ud800`, false},
		{"legacy note surrogate", "Exact original note.", `\udc00`, true},
		{"legacy pending payload surrogate", "Pending fact remains unaccepted", `\\ud800`, true},
		{"legacy pending provenance surrogate", `\"synthetic\"`, `\"\\ud800\"`, true},
		{"native header alias", `"memdolt_export_version":1`, `"memdolt_export_version":1,"MEMDOLT_EXPORT_VERSION":1`, false},
		{"native proposal alias", `"head":`, `"HEAD":`, false},
		{"native change alias", `"from":`, `"FROM":`, false},
		{"legacy header alias", `"memhub_export_version": 1`, `"memhub_export_version": 1,"MEMHUB_EXPORT_VERSION":1`, true},
		{"legacy project alias", `"root_path_at_export":`, `"ROOT_PATH_AT_EXPORT":`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := native
			if tc.legacy {
				data = legacy
			}
			changed := bytes.Replace(data, []byte(tc.old), []byte(tc.replacement), 1)
			if bytes.Equal(data, changed) {
				t.Fatal("fixture replacement did not reach the intended field")
			}
			file := interopTestFile(t, changed)
			target := renderStore(t)
			before := transferMain(t, target)
			result, err := target.ImportMemory(ctx, ImportMemoryOptions{File: file, FromMemhub: tc.legacy})
			if err == nil || result.Status != "refused" || result.MainCommit != "" || len(result.CreatedProposals) != 0 || transferMain(t, target) != before || internalCount(t, target, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatalf("malformed input mutated destination: %+v, %v", result, err)
			}
			if after, err := os.ReadFile(file); err != nil || !bytes.Equal(after, changed) {
				t.Fatal("import rewrote malformed source bytes")
			}
		})
	}
	valid := bytes.Replace(native, []byte("Pending fact remains unaccepted"), []byte(`\ud83d\ude00\ufffd`), 1)
	target := renderStore(t)
	result, err := target.ImportMemory(ctx, ImportMemoryOptions{File: interopTestFile(t, valid)})
	if err != nil || len(result.CreatedProposals) != 2 {
		t.Fatalf("valid surrogate pair refused: %+v, %v", result, err)
	}
	diff, err := target.ProposalDiff(ctx, result.CreatedProposals[0].ID)
	if err != nil || !slices.ContainsFunc(diff.Changes, func(change ProposalChange) bool { return change.Table == "facts" && change.To["value"] == "😀�" }) {
		t.Fatalf("valid proposal Unicode changed: %+v, %v", diff, err)
	}
}

func TestInteropProtectsOpenedCredentialsAndOutputSources(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	credential := []byte("synthetic owner credential; never read into a bundle")
	if err := os.WriteFile(s.paths.PidFile(), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(interopTempDir(t), "innocent.json")
	if err := os.Link(s.paths.PidFile(), alias); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ImportMemory(ctx, ImportMemoryOptions{File: alias, FromMemhub: true}); err == nil || !strings.Contains(err.Error(), "owner metadata") || result.MainCommit != "" {
		t.Fatalf("read owner credential alias: %+v, %v", result, err)
	}
	if result, err := s.ExportMemory(ctx, ExportMemoryOptions{File: alias}); err == nil || !strings.Contains(err.Error(), "owner metadata") || result.Written {
		t.Fatalf("replaced owner credential alias: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(s.paths.PidFile()); err != nil || string(got) != string(credential) {
		t.Fatal("interop changed owner credentials")
	}
	file := interopTestFile(t, []byte("ordinary user source"))
	if result, err := s.ExportMemory(ctx, ExportMemoryOptions{File: file}); err == nil || result.Written {
		t.Fatalf("overwrote unrecognized source: %+v, %v", result, err)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "ordinary user source" {
		t.Fatal("preparation failure lost source")
	}
}

func TestInteropExportPinsMainAndProposalHeadsBeforeReads(t *testing.T) {
	s := renderStore(t)
	ctx := context.Background()
	_, main, err := memory.New(s, memory.UserActor).AddTask(ctx, "captured task", "original main")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ProposeFact(ctx, Proposal{Actor: s.cfg.Actor, Rationale: "captured proposal", Target: TargetRepo}, Fact{Key: "captured.fact", Value: "original proposal"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(interopTempDir(t), "pinned.json")
	result, err := s.exportMemory(ctx, ExportMemoryOptions{File: file}, interopHooks{afterCapture: func() {
		// Bypass the participating mutex through raw sessions, like foreign Dolt.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.commitConn(ctx, conn, store.CommitRequest{Author: s.cfg.Actor, Message: "foreign main change", Text: []string{"later task"}, Statements: []store.Statement{{SQL: "INSERT INTO tasks (id, title, created_at, updated_at) VALUES (?, 'later task', NOW(), NOW())", Args: []any{newID()}}}})
		if err = errors.Join(err, conn.Close()); err != nil {
			t.Fatal(err)
		}
		if err := s.RunOnBranch(ctx, p.Branch,
			store.Statement{SQL: "UPDATE facts SET value = 'later proposal' WHERE id = ?", Args: []any{p.RowID}},
			store.Statement{SQL: "CALL DOLT_COMMIT('--amend', '-A', '-m', 'foreign amended proposal')"},
		); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil || result.SourceCommit != main || transferMain(t, s) == main {
		t.Fatalf("pinned export=%+v, %v", result, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var bundle InteropBundle
	if err := json.Unmarshal(data, &bundle); err != nil || len(bundle.Tables["tasks"]) != 1 || bundle.Proposals[0].Head != p.Commit || strings.Contains(string(data), "later task") || strings.Contains(string(data), "later proposal") {
		t.Fatalf("mixed export generations: %s, %v", data, err)
	}
}

func TestInteropExportCancellationRemainsVisible(t *testing.T) {
	s := renderStore(t)
	pending, err := s.ProposeDecision(context.Background(), Proposal{Actor: s.cfg.Actor, Target: TargetRepo, Rationale: "keep pending"}, Decision{Title: "cancelled export", Rationale: "read with caller context"})
	if err != nil {
		t.Fatal(err)
	}
	before := statusSnapshot(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	file := filepath.Join(interopTempDir(t), "cancelled.json")
	result, err := s.exportMemory(ctx, ExportMemoryOptions{File: file}, interopHooks{afterCapture: cancel})
	if ctx.Err() != context.Canceled || err == nil || result.Written || result.Status != "refused" {
		t.Fatalf("cancelled export=%+v, %v", result, err)
	}
	for _, path := range []string{file, filepath.Join(filepath.Dir(file), ".cancelled.json.memdolt-export.lock")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled export left output/lock %s: %v", path, err)
		}
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repoDiffRows(ctx, conn, "decisions", transferMain(t, s), pending.Commit)
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "context canceled") || len(changes) != 0 || statusSnapshot(t, s) != before {
		t.Fatalf("cancelled diff lost error or changed repository: %+v, %v", changes, err)
	}
}

func TestInteropNativeSupersedeAndAcceptedMetadata(t *testing.T) {
	ctx := context.Background()
	s := renderStore(t)
	old, err := s.FactAdd(ctx, FactAddOptions{Actor: memory.UserActor, Source: "user", Fact: Fact{Key: "build.mode", Value: "old mode"}})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := s.ProposeSupersede(ctx, Proposal{Actor: s.cfg.Actor, Target: TargetRepo, Rationale: "new mode"}, old.ID, Fact{Key: "build.mode", Value: "new mode", Evidence: "issue 140"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(interopTempDir(t), "supersede.json")
	if _, err := s.ExportMemory(ctx, ExportMemoryOptions{File: file}); err != nil {
		t.Fatal(err)
	}
	target := renderStore(t)
	result, err := target.ImportMemory(ctx, ImportMemoryOptions{File: file})
	if err != nil || len(result.CreatedProposals) != 1 || result.CreatedProposals[0].RowID != proposal.RowID {
		t.Fatalf("supersede import=%+v, %v", result, err)
	}
	if internalCount(t, target, "SELECT COUNT(*) FROM facts") != 1 {
		t.Fatal("import promoted supersession")
	}
	if _, err := target.AcceptProposal(ctx, proposal.ID, memory.UserActor.CommitAuthor(), AcceptOptions{}); err != nil {
		t.Fatal(err)
	}
	if internalCount(t, target, "SELECT COUNT(*) FROM facts") != 2 || internalCount(t, target, "SELECT COUNT(*) FROM proposals") != 1 {
		t.Fatal("ordinary supersede review lost payload or metadata")
	}
	accepted := filepath.Join(interopTempDir(t), "accepted.json")
	if _, err := target.ExportMemory(ctx, ExportMemoryOptions{File: accepted}); err != nil {
		t.Fatal(err)
	}
	final := renderStore(t)
	if _, err := final.ImportMemory(ctx, ImportMemoryOptions{File: accepted}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(interopCaptured(t, target).Tables, interopCaptured(t, final).Tables) {
		t.Fatal("native interop lost committed proposal metadata")
	}
	changed := renderStore(t)
	partial, err := changed.importMemory(ctx, ImportMemoryOptions{File: file}, interopHooks{afterMain: func() error {
		conn, err := changed.db.Conn(ctx)
		if err != nil {
			return err
		}
		_, err = changed.commitConn(ctx, conn, store.CommitRequest{Author: changed.cfg.Actor, Message: "foreign change before proposal cut", Text: []string{"foreign adjustment"}, Statements: []store.Statement{{SQL: "UPDATE facts SET value = ? WHERE id = ?", Args: []any{"foreign adjustment", old.ID}}}})
		return errors.Join(err, conn.Close())
	}})
	if err == nil || partial.MainCommit == "" || partial.Status != "partial" || len(partial.CreatedProposals) != 0 || len(partial.RemainingProposals) != 1 || partial.ProposalResidue != nil {
		t.Fatalf("foreign before-image change was not refused honestly: %+v, %v", partial, err)
	}
	var retained string
	if err := changed.db.QueryRowContext(ctx, "SELECT value FROM facts WHERE id = ?", old.ID).Scan(&retained); err != nil || retained != "foreign adjustment" {
		t.Fatal("imported proposal overwrote a foreign main change")
	}
}
