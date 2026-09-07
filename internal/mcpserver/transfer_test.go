package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func mcpPullFixture(t *testing.T, conflicts int) (string, *elicitationBackend, *localdolt.Store, []string) {
	t.Helper()
	ctx := context.Background()
	base, a := initializedElicitationBackend(t)
	path := filepath.ToSlash(t.TempDir())
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	remote := (&url.URL{Scheme: "file", Path: path}).String()
	if _, err := a.AddRemote(ctx, localdolt.Remote{Name: "origin", URL: remote}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range conflicts {
		task, _, err := memory.New(a.Store, memory.UserActor).AddTask(ctx, "shared", "base")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	if _, err := a.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := localdolt.Config{BaseDir: t.TempDir(), Actor: memory.UserActor.CommitAuthor()}
	if _, err := localdolt.Clone(ctx, cfg, remote, ""); err != nil {
		t.Fatal(err)
	}
	b, err := localdolt.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	if conflicts == 0 {
		if _, _, err := memory.New(a.Store, memory.UserActor).AddTask(ctx, "local", "independent"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := memory.New(b, memory.UserActor).AddTask(ctx, "remote", "independent"); err != nil {
			t.Fatal(err)
		}
	} else {
		for i, st := range []*localdolt.Store{a.Store, b} {
			var statements []store.Statement
			for _, id := range ids {
				statements = append(statements, store.Statement{SQL: "UPDATE tasks SET notes = ?, updated_at = ? WHERE id = ?", Args: []any{[]string{"ours", "theirs"}[i], []string{"2026-01-02 00:00:00", "2026-01-03 00:00:00"}[i], id}})
			}
			if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), Message: "synthetic conflicting task edits", NoText: true, Statements: statements}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := b.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	return base, a, b, ids
}

func mcpPullSnapshot(t *testing.T, st *localdolt.Store) string {
	t.Helper()
	return testText(t, st, "SELECT CONCAT(DOLT_HASHOF('main'), '/', DOLT_HASHOF_DB('WORKING'), '/', DOLT_HASHOF_DB('STAGED'))")
}

func pullFormResponse(t *testing.T, req *mcp.ElicitRequest, ids []string) *mcp.ElicitResult {
	t.Helper()
	encoded, err := json.Marshal(req.Params.RequestedSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{}
	for field := range schema.Properties {
		index, err := strconv.Atoi(strings.TrimPrefix(field, "choice_"))
		if err != nil || index < 0 || index >= len(ids) {
			t.Fatalf("unexpected form field %s", field)
		}
		choice, err := json.Marshal(localdolt.PullChoice{Conflict: "tasks:row:" + ids[index], Take: "theirs"})
		if err != nil {
			t.Fatal(err)
		}
		content[field] = string(choice)
	}
	return &mcp.ElicitResult{Action: "accept", Content: content}
}

func TestRepoPullMCPAtomicContinuationAndGenuineLegacy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			base, st, _, ids := mcpPullFixture(t, 11)
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			before := mcpPullSnapshot(t, st.Store)
			rounds := 0
			options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				rounds++
				if mcpPullSnapshot(t, st.Store) != before {
					t.Fatal("a partial choice was promoted")
				}
				for _, phrase := range []string{"baseBlame", "oursBlame", "theirsBlame", "synthetic conflicting task edits", "localCommit", "remoteCommit", "No partial promotion"} {
					if !strings.Contains(req.Params.Message, phrase) {
						t.Fatalf("form lacks %s", phrase)
					}
				}
				return pullFormResponse(t, req, ids), nil
			}}
			client, session := connectWithOptions(t, server, &mcp.Implementation{Name: "user", Version: "1"}, legacy, options)
			defer closeSessions(t, client, session)
			output := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
			if !legacy {
				if rounds != 9 || output.NextCursor == "" || output.Result.Status != "conflicted" || mcpPullSnapshot(t, st.Store) != before {
					t.Fatalf("continuation = %+v after %d rounds", output, rounds)
				}
				cursor := output.NextCursor
				output = callAs[repoPullOutput](t, client, "repo_pull", map[string]any{"cursor": cursor})
				callError(t, client, "repo_pull", map[string]any{"cursor": cursor}, "already used")
			}
			if !output.Result.Changed || output.Result.Status != "changed" || output.NextCursor != "" || rounds != map[bool]int{true: 1, false: 11}[legacy] {
				t.Fatalf("complete merge = %+v after %d rounds", output, rounds)
			}
			if testCount(t, st.Store, "SELECT COUNT(*) FROM tasks AS OF 'main' WHERE notes='theirs'") != 11 || testText(t, st.Store, "SELECT committer FROM dolt_log WHERE commit_hash = ?", output.Result.MainCommit) != "user" {
				t.Fatal("atomic choices or human author lost")
			}
		})
	}
}

func TestRepoPullPushMCPCompatibleAttributionAndFallback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("compatible/legacy=%t", legacy), func(t *testing.T) {
			base, st, _, _ := mcpPullFixture(t, 0)
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			client, session := connect(t, server, &mcp.Implementation{Name: "cli", Version: "1"}, legacy)
			defer closeSessions(t, client, session)
			pulled := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
			if !pulled.Result.Changed || testText(t, st.Store, "SELECT committer FROM dolt_log WHERE commit_hash = ?", pulled.Result.MainCommit) != "agent:opencode" {
				t.Fatalf("compatible attribution = %+v", pulled)
			}
			pushed := callAs[localdolt.TransferResult](t, client, "repo_push", map[string]any{})
			if pushed.RemoteCommit != pulled.Result.MainCommit || !pushed.Changed {
				t.Fatalf("push = %+v", pushed)
			}
		})
	}
	for _, capability := range []string{"none", "url", "missing identity"} {
		t.Run(capability, func(t *testing.T) {
			base, st, _, _ := mcpPullFixture(t, 1)
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			options := &mcp.ClientOptions{}
			name := "test"
			if capability == "url" {
				options.Capabilities = &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}}
			}
			if capability == "missing identity" {
				name = ""
				options.ElicitationHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					t.Fatal("unattributed dialog")
					return nil, nil
				}
			}
			client, session := connectWithOptions(t, server, &mcp.Implementation{Name: name, Version: "1"}, false, options)
			defer closeSessions(t, client, session)
			before := mcpPullSnapshot(t, st.Store)
			output := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
			if output.Result.Status != "conflicted" || !strings.Contains(output.Result.Remedy, "memdolt pull --resolve") || mcpPullSnapshot(t, st.Store) != before {
				t.Fatalf("fallback = %+v", output)
			}
		})
	}
}

func manualPullCall(t *testing.T, client *mcp.ClientSession, state string, responses mcp.InputResponseMap, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "repo_pull", Arguments: args, RequestState: state, InputResponses: responses})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRepoPullStateAttacksAndStorageFailuresNeverPromote(t *testing.T) {
	for _, failure := range []string{"missing", "forged", "expired", "replay", "remote", "user", "action", "actor", "cancel", "decline", "incomplete", "malformed", "consume failure", "insert failure", "next insert failure", "restart", "local changed", "remote changed"} {
		t.Run(failure, func(t *testing.T) {
			base, st, remote, ids := mcpPullFixture(t, 2)
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			options := &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}, ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				return &mcp.ElicitResult{Action: "cancel"}, nil
			}}
			client, session := connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false, options)
			defer closeSessions(t, client, session)
			before := mcpPullSnapshot(t, st.Store)
			if failure == "insert failure" {
				if err := tools.elicit.Close(); err != nil {
					t.Fatal(err)
				}
			}
			initial := manualPullCall(t, client, "", nil, map[string]any{})
			if failure == "insert failure" {
				if !initial.IsError || mcpPullSnapshot(t, st.Store) != before {
					t.Fatal("failed state insert promoted")
				}
				return
			}
			if !initial.NeedsInput() {
				t.Fatalf("no dialog: %+v", initial)
			}
			state := initial.RequestState
			choice, err := json.Marshal(localdolt.PullChoice{Conflict: "tasks:row:" + ids[0], Take: "theirs"})
			if err != nil {
				t.Fatal(err)
			}
			response := &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_0": string(choice)}}
			args := map[string]any{}
			switch failure {
			case "missing":
				state = ""
			case "forged":
				state = strings.Repeat("x", 43)
			case "expired":
				if err := tools.elicit.expire(context.Background(), state); err != nil {
					t.Fatal(err)
				}
			case "replay":
				used := manualPullCall(t, client, state, mcp.InputResponseMap{pullResponseID: &mcp.ElicitResult{Action: "cancel"}}, args)
				if used.IsError {
					t.Fatal(used.GetError())
				}
			case "remote":
				args["remote"] = "another"
			case "user":
				args["user"] = "another"
			case "action":
				if _, err := tools.elicit.db.Exec("UPDATE pending_elicitations SET action = 'review_accept_one'"); err != nil {
					t.Fatal(err)
				}
			case "actor":
				if _, err := tools.elicit.db.Exec("UPDATE pending_elicitations SET actor_name = 'agent:other'"); err != nil {
					t.Fatal(err)
				}
			case "cancel", "decline":
				response.Action, response.Content = failure, nil
			case "incomplete":
				response.Content = map[string]any{}
			case "malformed":
				response.Content["choice_0"] = `{"take":"ours","take":"theirs"}`
			case "consume failure":
				if err := tools.elicit.Close(); err != nil {
					t.Fatal(err)
				}
			case "next insert failure":
				if _, err := tools.elicit.db.Exec("CREATE TRIGGER fail_state BEFORE INSERT ON pending_elicitations BEGIN SELECT RAISE(ABORT, 'synthetic insert failure'); END"); err != nil {
					t.Fatal(err)
				}
			case "restart":
				if err := tools.elicit.Close(); err != nil {
					t.Fatal(err)
				}
				tools.elicit, err = newElicitationStateStore()
				if err != nil {
					t.Fatal(err)
				}
			case "local changed", "remote changed":
				writer := st.Store
				if failure == "remote changed" {
					writer = remote
				}
				if _, _, err := memory.New(writer, memory.UserActor).AddTask(context.Background(), "interleaved", "must survive"); err != nil {
					t.Fatal(err)
				}
				if failure == "remote changed" {
					if _, err := remote.Push(context.Background(), localdolt.TransferOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				before = mcpPullSnapshot(t, st.Store)
			}
			result := manualPullCall(t, client, state, mcp.InputResponseMap{pullResponseID: response}, args)
			if failure == "local changed" || failure == "remote changed" {
				if !result.NeedsInput() {
					t.Fatalf("second choice not reachable: %+v", result)
				}
				choice, err := json.Marshal(localdolt.PullChoice{Conflict: "tasks:row:" + ids[1], Take: "theirs"})
				if err != nil {
					t.Fatal(err)
				}
				result = manualPullCall(t, client, result.RequestState, mcp.InputResponseMap{pullResponseID: &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_1": string(choice)}}}, args)
			}
			wantError := failure != "cancel" && failure != "decline"
			if result.IsError != wantError || mcpPullSnapshot(t, st.Store) != before {
				t.Fatalf("%s = %+v; main/roots changed or wrong outcome", failure, result)
			}
		})
	}
}

type pullLateFailureBackend struct{ Backend }

func (s pullLateFailureBackend) Pull(ctx context.Context, opts localdolt.TransferOptions) (localdolt.TransferResult, error) {
	result, err := s.Backend.Pull(ctx, opts)
	if result.Changed {
		err = errors.Join(err, errors.New("synthetic late bookkeeping failure"))
	}
	return result, err
}

func TestRepoPullMCPRetainsConfirmedResultOnLateFailure(t *testing.T) {
	base, st, _, _ := mcpPullFixture(t, 0)
	server := New("test")
	tools := RegisterTools(server, base, pullLateFailureBackend{st})
	defer func() {
		if err := tools.Close(); err != nil {
			t.Error(err)
		}
	}()
	client, session := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
	defer closeSessions(t, client, session)
	response := manualPullCall(t, client, "", nil, map[string]any{})
	encoded, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output repoPullOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if !response.IsError || !output.Result.Changed || output.Result.MainCommit != testText(t, st.Store, "SELECT DOLT_HASHOF('main')") || !strings.Contains(output.Result.Error, "late bookkeeping") {
		t.Fatalf("late response = %+v, %+v", response, output)
	}
}

func TestRepoPullContinuationExpiryCancelAndMintFailureKeepAllChoicesPending(t *testing.T) {
	for _, failure := range []string{"expired cursor", "cancel continuation", "cursor insert"} {
		t.Run(failure, func(t *testing.T) {
			base, st, _, ids := mcpPullFixture(t, 10)
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			before := mcpPullSnapshot(t, st.Store)
			rounds := 0
			options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				rounds++
				if rounds == 9 && failure == "cursor insert" {
					if _, err := tools.elicit.db.Exec("CREATE TRIGGER fail_cursor BEFORE INSERT ON pending_elicitations BEGIN SELECT RAISE(ABORT, 'synthetic cursor failure'); END"); err != nil {
						t.Fatal(err)
					}
				}
				if rounds == 10 {
					return &mcp.ElicitResult{Action: "cancel"}, nil
				}
				return pullFormResponse(t, req, ids), nil
			}}
			client, session := connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false, options)
			defer closeSessions(t, client, session)
			response := manualPullCall(t, client, "", nil, map[string]any{})
			if failure == "cursor insert" {
				if !response.IsError {
					t.Fatal("cursor storage failure was hidden")
				}
			} else {
				encoded, err := json.Marshal(response.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				var output repoPullOutput
				if err := json.Unmarshal(encoded, &output); err != nil {
					t.Fatal(err)
				}
				if output.NextCursor == "" || rounds != 9 {
					t.Fatalf("unreachable continuation: %+v", output)
				}
				if failure == "expired cursor" {
					if err := tools.elicit.expire(context.Background(), output.NextCursor); err != nil {
						t.Fatal(err)
					}
					callError(t, client, "repo_pull", map[string]any{"cursor": output.NextCursor}, "expired")
				} else {
					canceled := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{"cursor": output.NextCursor})
					if canceled.Result.Status != "cancel" || rounds != 10 {
						t.Fatalf("cancellation = %+v", canceled)
					}
				}
			}
			if mcpPullSnapshot(t, st.Store) != before || testCount(t, st.Store, "SELECT COUNT(*) FROM tasks AS OF 'main' WHERE notes='ours'") != 10 {
				t.Fatal("continuation failure partially promoted choices")
			}
		})
	}
}

type pullUnicodeWireWriter struct {
	io.WriteCloser
	replacement []byte
	changed     atomic.Bool
}

func (w *pullUnicodeWireWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte("WIRE_UNICODE_MARKER")) {
		return w.WriteCloser.Write(p)
	}
	w.changed.Store(true)
	_, err := w.WriteCloser.Write(bytes.ReplaceAll(p, []byte("WIRE_UNICODE_MARKER"), w.replacement))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestRepoPullRawMCPUnicodeBeforeSDKDecoding(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, replacement := range []string{"bad\xfftext", `bad\ud800text`, `bad\udc00text`, `\ud83d\ude00\ufffd`, "literal �"} {
			t.Run(fmt.Sprintf("legacy=%t/%q", legacy, replacement), func(t *testing.T) {
				base, st, _, _ := mcpPullFixture(t, 1)
				shown, err := st.Pull(context.Background(), localdolt.TransferOptions{})
				if err != nil {
					t.Fatal(err)
				}
				row := maps.Clone(shown.Conflicts[0].Rows[0].Ours)
				marker := "WIRE_UNICODE_MARKER"
				row["notes"] = &marker
				choice, err := json.Marshal(localdolt.PullChoice{Conflict: shown.Conflicts[0].ID, Take: "manual", Row: row})
				if err != nil {
					t.Fatal(err)
				}
				server := New("test")
				tools := RegisterTools(server, base, st)
				defer func() {
					if err := tools.Close(); err != nil {
						t.Error(err)
					}
				}()
				options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_0": string(choice)}}, nil
				}}
				var wire *pullUnicodeWireWriter
				client, session := connectWithWireWriter(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, legacy, options, func(writer io.WriteCloser) io.WriteCloser {
					wire = &pullUnicodeWireWriter{WriteCloser: writer, replacement: []byte(replacement)}
					return wire
				})
				before := mcpPullSnapshot(t, st.Store)
				response, callErr := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "repo_pull", Arguments: map[string]any{}})
				if err := client.Close(); err != nil {
					t.Error(err)
				}
				serverErr := session.Wait()
				if !wire.changed.Load() {
					t.Fatal("fixture did not send altered raw protocol bytes")
				}
				valid := replacement == `\ud83d\ude00\ufffd` || replacement == "literal �"
				if !valid {
					if callErr == nil && response != nil && !response.IsError {
						t.Fatal("malformed wire content was accepted")
					}
					if serverErr == nil || mcpPullSnapshot(t, st.Store) != before {
						t.Fatalf("malformed wire did not fail before promotion: %v", serverErr)
					}
				} else {
					want := "literal �"
					if replacement != want {
						want = "😀�"
					}
					if callErr != nil || serverErr != nil || response.IsError || testText(t, st.Store, "SELECT notes FROM tasks WHERE id = ?", shown.Conflicts[0].Rows[0].ID) != want {
						t.Fatalf("valid wire text changed: %v, %v, %+v", callErr, serverErr, response)
					}
				}
			})
		}
	}
}

func TestRepoPullMCPInnerChoiceUnicodeRefuses(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		base, st, _, _ := mcpPullFixture(t, 1)
		shown, err := st.Pull(context.Background(), localdolt.TransferOptions{})
		if err != nil {
			t.Fatal(err)
		}
		row := maps.Clone(shown.Conflicts[0].Rows[0].Ours)
		marker := "INNER_UNICODE_MARKER"
		row["notes"] = &marker
		choice, err := json.Marshal(localdolt.PullChoice{Conflict: shown.Conflicts[0].ID, Take: "manual", Row: row})
		if err != nil {
			t.Fatal(err)
		}
		choice = bytes.Replace(choice, []byte(marker), []byte(`\ud800`), 1)
		server := New("test")
		tools := RegisterTools(server, base, st)
		options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_0": string(choice)}}, nil
		}}
		client, session := connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, legacy, options)
		before := mcpPullSnapshot(t, st.Store)
		callError(t, client, "repo_pull", map[string]any{}, "surrogate")
		if mcpPullSnapshot(t, st.Store) != before {
			t.Fatal("inner choice replacement changed roots")
		}
		closeSessions(t, client, session)
		if err := tools.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepoPullMCPRefusesManualNoteProvenance(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			base, st, remote, _ := mcpPullFixture(t, 1)
			for i, writer := range []*localdolt.Store{st.Store, remote} {
				if _, err := writer.Commit(context.Background(), store.CommitRequest{Author: memory.UserActor.CommitAuthor(), Message: "note provenance conflict fixture", NoText: true, Statements: []store.Statement{{SQL: "INSERT INTO session_notes (id, actor, actor_raw, text, session_id, provider_id, model_id, variant) VALUES ('note', 'agent:opencode', 'cli', ?, 'ses_original', 'original_provider', NULL, '')", Args: []any{[]string{"ours", "theirs"}[i]}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := remote.Push(context.Background(), localdolt.TransferOptions{}); err != nil {
				t.Fatal(err)
			}
			shown, err := st.Pull(context.Background(), localdolt.TransferOptions{})
			if err != nil || len(shown.Conflicts) != 2 {
				t.Fatalf("note fixture = %+v, %v", shown, err)
			}
			server := New("test")
			tools := RegisterTools(server, base, st)
			defer func() {
				if err := tools.Close(); err != nil {
					t.Error(err)
				}
			}()
			options := &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				content := map[string]any{}
				for i, conflict := range shown.Conflicts {
					field := fmt.Sprintf("choice_%d", i)
					encodedSchema, err := json.Marshal(req.Params.RequestedSchema)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(encodedSchema), field) {
						continue
					}
					choice := localdolt.PullChoice{Conflict: conflict.ID, Take: "ours"}
					if conflict.Table == "session_notes" {
						choice.Take, choice.Row = "manual", maps.Clone(conflict.Rows[0].Ours)
						forged := "fabricated_model"
						choice.Row["model_id"] = &forged
					}
					encoded, err := json.Marshal(choice)
					if err != nil {
						t.Fatal(err)
					}
					content[field] = string(encoded)
				}
				return &mcp.ElicitResult{Action: "accept", Content: content}, nil
			}}
			client, session := connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, legacy, options)
			defer closeSessions(t, client, session)
			before := mcpPullSnapshot(t, st.Store)
			callError(t, client, "repo_pull", map[string]any{}, "provenance column model_id")
			if mcpPullSnapshot(t, st.Store) != before {
				t.Fatal("manual provenance response partially promoted the merge")
			}
		})
	}
}

func TestRepoPullMCPLiveKeyResultsOmitAbsentRowImages(t *testing.T) {
	base, st, remote, _ := mcpPullFixture(t, 0)
	ctx := context.Background()
	for _, writer := range []*localdolt.Store{st.Store, remote} {
		if _, err := writer.FactAdd(ctx, localdolt.FactAddOptions{Fact: localdolt.Fact{Key: "collision.key", Value: "retain both rows"}, Source: "user", Actor: memory.UserActor}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := remote.Push(ctx, localdolt.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	server := New("test")
	tools := RegisterTools(server, base, st)
	defer func() {
		if err := tools.Close(); err != nil {
			t.Error(err)
		}
	}()
	client, session := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
	fallback := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
	closeSessions(t, client, session)
	if fallback.Result.Status != "conflicted" || len(fallback.Result.Conflicts) != 1 || len(fallback.Result.Conflicts[0].Rows) != 2 {
		t.Fatalf("typed absent-image fallback = %+v", fallback)
	}
	conflict := fallback.Result.Conflicts[0]
	for _, row := range conflict.Rows {
		if row.Base != nil {
			t.Fatal("new fact has a fabricated base image")
		}
	}
	choice, err := json.Marshal(localdolt.PullChoice{Conflict: conflict.ID, Take: "winner", Winner: conflict.Rows[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_0": string(choice)}}, nil
	}}
	client, session = connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false, options)
	defer closeSessions(t, client, session)
	result := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
	if !result.Result.Changed || testCount(t, st.Store, "SELECT COUNT(*) FROM facts WHERE `key`='collision.key'") != 2 || testCount(t, st.Store, "SELECT COUNT(*) FROM facts WHERE live_key='collision.key'") != 1 {
		t.Fatalf("typed live-key merge = %+v", result)
	}
}

func TestRepoPullMCPAbsentMergedImageRetainsLateResult(t *testing.T) {
	base, st, _, ids := mcpPullFixture(t, 1)
	ctx := context.Background()
	if _, err := st.Commit(ctx, store.CommitRequest{Author: memory.UserActor.CommitAuthor(), Message: "synthetic delete versus edit", NoText: true, Statements: []store.Statement{{SQL: "DELETE FROM tasks WHERE id = ?", Args: []any{ids[0]}}}}); err != nil {
		t.Fatal(err)
	}
	server := New("test")
	tools := RegisterTools(server, base, pullLateFailureBackend{st})
	defer func() {
		if err := tools.Close(); err != nil {
			t.Error(err)
		}
	}()
	client, session := connect(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false)
	fallback := callAs[repoPullOutput](t, client, "repo_pull", map[string]any{})
	closeSessions(t, client, session)
	if len(fallback.Result.Conflicts) != 1 || len(fallback.Result.Conflicts[0].Rows) != 1 {
		t.Fatalf("delete/edit fallback = %+v", fallback)
	}
	row := fallback.Result.Conflicts[0].Rows[0]
	if row.Ours != nil || row.Merged != nil || row.Base == nil || row.Theirs == nil {
		t.Fatalf("absent images changed: %+v", row)
	}
	choice, err := json.Marshal(localdolt.PullChoice{Conflict: fallback.Result.Conflicts[0].ID, Take: "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	options := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"choice_0": string(choice)}}, nil
	}}
	client, session = connectWithOptions(t, server, &mcp.Implementation{Name: "Codex", Version: "1"}, false, options)
	defer closeSessions(t, client, session)
	response := manualPullCall(t, client, "", nil, map[string]any{})
	encoded, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result repoPullOutput
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if !response.IsError || !result.Result.Changed || result.Result.MainCommit != testText(t, st.Store, "SELECT DOLT_HASHOF('main')") || testText(t, st.Store, "SELECT notes FROM tasks WHERE id = ?", ids[0]) != "theirs" {
		t.Fatalf("absent-image confirmed result lost: %+v", result)
	}
}
