package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestLaneReadsPreserveDirtyAndProposalData(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			runMemdolt(t, "note", "add", "committed note", "--dir", base)
			runMemdolt(t, "state", "set", "committed state", "--dir", base)
			runMemdolt(t, "arch", "set", "committed arch", "--dir", base)
			runMemdolt(t, "command", "record", "test", "committed command", "--dir", base)
			st := openInitializedStore(t, base)
			var committedMain string
			humanRow(t, st, "SELECT DOLT_HASHOF('main')", nil, &committedMain)
			pending, err := st.ProposeFact(context.Background(), localdolt.Proposal{
				Rationale: "pending lane control", Actor: cliStagingActor, Target: localdolt.TargetRepo,
			}, localdolt.Fact{Key: "pending.lane", Value: "pending fact"})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db := openRepoFixtureDB(t, base)
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []store.Statement{
				{SQL: "CALL DOLT_CHECKOUT(?)", Args: []any{pending.Branch}},
				{SQL: "UPDATE session_notes SET text = ?", Args: []any{"proposal note"}},
				{SQL: "UPDATE project_state SET body = ?", Args: []any{"proposal state"}},
				{SQL: "UPDATE project_arch SET body = ?", Args: []any{"proposal arch"}},
				{SQL: "UPDATE commands SET cmdline = ?", Args: []any{"proposal command"}},
				{SQL: "CALL DOLT_COMMIT('-Am', 'pending lane fixture')"},
				{SQL: "CALL DOLT_CHECKOUT('main')"},
				{SQL: "UPDATE session_notes SET text = ?", Args: []any{"dirty note"}},
				{SQL: "UPDATE project_state SET body = ?", Args: []any{"dirty state"}},
				{SQL: "CALL DOLT_ADD('session_notes', 'project_state')"},
				{SQL: "UPDATE project_arch SET body = ?", Args: []any{"dirty arch"}},
				{SQL: "UPDATE commands SET cmdline = ?", Args: []any{"dirty command"}},
			} {
				if _, err := conn.ExecContext(context.Background(), statement.SQL, statement.Args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if routed {
				serveStore(t, base)
			}
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			humanInspect(t, base, func(st commandStore) {
				var main, captured string
				humanRow(t, st, "SELECT DOLT_HASHOF('main')", nil, &main)
				humanRow(t, st, "SELECT cmdline FROM commands AS OF 'main'", nil, &captured)
				if main != committedMain || captured != "committed command" {
					t.Fatal("fixture changed committed main")
				}
			})
			notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")).Notes
			if len(notes) != 1 || notes[0].Text != "committed note" {
				t.Errorf("note list exposed working/proposal data: %+v", notes)
			}
			for _, kind := range []string{"state", "arch"} {
				narrative := decodeJSON[memory.Narrative](t, runMemdolt(t, kind, "show", "--dir", base, "--json"))
				if narrative.Body != "committed "+kind {
					t.Errorf("%s show exposed working/proposal data: %+v", kind, narrative)
				}
				history := decodeJSON[narrativeHistory](t, runMemdolt(t, kind, "history", "--dir", base, "--json")).History
				if len(history) != 1 || !reflect.DeepEqual(history[0], narrative) {
					t.Errorf("%s history exposed working/proposal data: %+v", kind, history)
				}
				if human := runMemdolt(t, kind, "history", "--dir", base); !strings.Contains(human, narrative.Body) || strings.Contains(human, "dirty") || strings.Contains(human, "proposal") {
					t.Fatal(human)
				}
			}
			command := decodeJSON[memory.Command](t, runMemdolt(t, "command", "get", "test", "--dir", base, "--json"))
			if command.Cmdline != "committed command" {
				t.Errorf("command get exposed working/proposal data: %+v", command)
			}
			commands := decodeJSON[commandList](t, runMemdolt(t, "command", "list", "--dir", base, "--json")).Commands
			if len(commands) != 1 || commands[0] != command {
				t.Fatal(commands)
			}
			if filtered := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--actor", "user", "--since-days", "1", "--dir", base, "--json")).Notes; !reflect.DeepEqual(filtered, notes) {
				t.Fatal(filtered)
			}
			for _, args := range invalidLaneInputs() {
				for _, jsonFlag := range [][]string{nil, {"--json"}} {
					args := append(append(append([]string{}, args...), "--dir", base), jsonFlag...)
					if out, err := runMemdoltResult(t, args...); err == nil || out != "" {
						t.Fatalf("invalid input produced success: %v = %q %v", args, out, err)
					}
				}
			}
			humanInspect(t, base, func(st commandStore) {
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
					t.Fatalf("reads changed branches or working/staged data: %v -> %v", before, after)
				}
			})
		})
	}
}

type narrativeHistory struct {
	History []memory.Narrative `json:"history"`
}

type commandList struct {
	Commands []memory.Command `json:"commands"`
}

// Seed tied timestamps, old/future notes, exact actor variants and NULL/full
// provenance in native committed rows so ordering/filter checks do not race a clock.
func seedLaneReadRows(t *testing.T, base string) ([]memory.Note, map[string][]memory.Narrative) {
	t.Helper()
	st, err := openCommandStore(context.Background(), base, cliActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	anchor := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	var statements []store.Statement
	var notes []memory.Note
	history := map[string][]memory.Narrative{}
	for i := 1; i <= 27; i++ {
		id, created := fmt.Sprintf("%026d", i), anchor.Add(time.Duration(i/2)*time.Second)
		for _, kind := range []string{"state", "arch"} {
			narrative := memory.Narrative{Kind: memory.NarrativeKind(kind), ID: id, Body: fmt.Sprintf("%s version %d\nfull body: café", kind, i), Actor: "agent:claude-code", ActorRaw: "Claude Code", CreatedAt: created}
			statements = append(statements, store.Statement{
				SQL:  "INSERT INTO project_" + kind + " (id, body, actor, actor_raw, created_at) VALUES (?, ?, ?, ?, ?)",
				Args: []any{id, narrative.Body, narrative.Actor, narrative.ActorRaw, created},
			})
			history[kind] = append(history[kind], narrative)
		}
		note := memory.Note{ID: id, Text: fmt.Sprintf("note %d\nfull note: café", i), Actor: "agent:codex", ActorRaw: "Codex", CreatedAt: created}
		switch i {
		case 1:
			note.CreatedAt = anchor.Add(-48 * time.Hour)
		case 24:
			note.Actor = "agent:Codex"
		case 25:
			note.Actor = "agent:codex "
		case 26:
			note.Actor = "agent:codex' OR 1=1 --"
		case 27:
			note.Actor = ""
			note.CreatedAt = anchor.Add(2 * time.Hour)
		}
		metadata := []any{nil, nil, nil, nil, nil}
		if i%2 == 0 {
			note.NoteProvenance = memory.NoteProvenance{SessionID: "session", AgentID: "agent", ProviderID: "provider", ModelID: "model", Variant: "variant"}
			metadata = []any{"session", "agent", "provider", "model", "variant"}
		}
		statements = append(statements, store.Statement{
			SQL:  "INSERT INTO session_notes (id, actor, actor_raw, text, created_at, session_id, agent_id, provider_id, model_id, variant) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			Args: append([]any{id, note.Actor, note.ActorRaw, note.Text, note.CreatedAt}, metadata...),
		})
		notes = append(notes, note)
	}
	if _, err := st.Commit(context.Background(), store.CommitRequest{Statements: statements, Text: []string{"native lane read fixture"}, Message: "seed lane read fixture", Author: cliActor}); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(notes)
	for _, entries := range history {
		slices.Reverse(entries)
	}
	return notes, history
}

func TestNarrativeHistoryAndNoteFiltersDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			base := initStore(t)
			// Empty output is observable in both formats, including a JSON [] rather than null.
			if routed {
				serveStore(t, base)
			}
			for _, args := range [][]string{{"note", "list"}, {"state", "history"}, {"arch", "history"}, {"command", "list"}} {
				if out := runMemdolt(t, append(args, "--dir", base, "--json")...); !strings.Contains(out, ":[]") {
					t.Fatal(out)
				}
				if out := runMemdolt(t, append(args, "--dir", base)...); !strings.HasPrefix(out, "no ") {
					t.Fatal(out)
				}
			}
			notes, histories := seedLaneReadRows(t, base)
			for _, file := range []string{"config.toml", "embeddings.sqlite", "code_index.sqlite"} {
				content := "untouched derived fixture"
				if file == "config.toml" {
					content = "[deny_list]\npatterns=[]\n"
				}
				writeTestFile(t, filepath.Join(base, ".memdolt", file), content)
			}
			var before [][]string
			humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
			for _, kind := range []string{"state", "arch"} {
				for _, limit := range []int{0, 1, 26, 99, store.DefaultMaxRows} {
					args := []string{kind, "history", "--dir", base, "--json"}
					want := 25
					if limit != 0 {
						args = append(args, "--limit", strconv.Itoa(limit))
						want = min(limit, 27)
					}
					got := decodeJSON[narrativeHistory](t, runMemdolt(t, args...)).History
					if !reflect.DeepEqual(got, histories[kind][:want]) {
						t.Fatalf("%s history limit %d = %+v, want %+v", kind, limit, got, histories[kind][:want])
					}
				}
				human := runMemdolt(t, kind, "history", "--limit", "1", "--dir", base)
				newest := histories[kind][0]
				for _, want := range []string{newest.ID, newest.Body, newest.Actor, newest.ActorRaw, stamp(newest.CreatedAt)} {
					if !strings.Contains(human, want) {
						t.Fatalf("history human output omits %q: %s", want, human)
					}
				}
			}
			for _, tc := range []struct {
				flags []string
				want  []memory.Note
			}{
				{nil, notes[:25]},
				{[]string{"--limit", "27"}, notes},
				{[]string{"--limit", strconv.Itoa(store.DefaultMaxRows)}, notes},
				{[]string{"--actor", "agent:codex", "--since-days", "1", "--limit", "3"}, notes[4:7]},
				{[]string{"--actor", "agent:Codex"}, notes[3:4]},
				{[]string{"--actor", "agent:codex "}, notes[2:3]},
				{[]string{"--actor", "agent:codex' OR 1=1 --"}, notes[1:2]},
				{[]string{"--actor", ""}, notes[:1]},
				{[]string{"--actor", "Codex"}, []memory.Note{}},
				{[]string{"--actor", "agent:CODEX"}, []memory.Note{}},
				{[]string{"--since-days", "0"}, notes[:1]},
				{[]string{"--since-days", "106751", "--limit", "27"}, notes},
			} {
				args := append([]string{"note", "list", "--dir", base, "--json"}, tc.flags...)
				got := decodeJSON[noteList](t, runMemdolt(t, args...)).Notes
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("note list %v = %+v, want %+v", tc.flags, got, tc.want)
				}
			}
			if human := runMemdolt(t, "note", "list", "--actor", "agent:codex", "--since-days", "1", "--limit", "1", "--dir", base); !strings.Contains(human, "note 23 full note: café") {
				t.Fatal(human)
			}
			humanInspect(t, base, func(st commandStore) {
				unfiltered, err := memory.New(st, memory.UserActor).Notes(context.Background(), 27)
				if err != nil || !reflect.DeepEqual(unfiltered, notes) {
					t.Fatalf("public Notes changed shape: %+v %v", unfiltered, err)
				}
				if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
					t.Fatal("lane reads changed durable/working state")
				}
			})
			for _, file := range []string{"config.toml", "embeddings.sqlite", "code_index.sqlite"} {
				got, err := os.ReadFile(filepath.Join(base, ".memdolt", file))
				want := "untouched derived fixture"
				if file == "config.toml" {
					want = "[deny_list]\npatterns=[]\n"
				}
				if err != nil || string(got) != want {
					t.Fatalf("lane reads changed %s: %q %v", file, got, err)
				}
			}
		})
	}
}

func invalidLaneInputs() [][]string {
	args := [][]string{
		{"note", "list", "--since-days", "-1"},
		{"note", "list", "--since-days", "106752"},
		{"note", "list", "--since-days", "9223372036854775807"},
		{"note", "list", "--since-days", "9223372036854775808"},
		{"note", "list", "--since-days", "1.5"},
		{"note", "list", "--actor", string([]byte{0xff})},
		{"command", "verify", "test", "missing exit"},
		{"command", "verify", "deploy", "wrong kind", "--exit-code", "0"},
		{"command", "verify", "test", " ", "--exit-code", "0"},
		{"command", "verify", "test", "wrong flag", "--exit", "0"},
		{"command", "verify", "test", "overflow", "--exit-code", "9223372036854775808"},
		{"command", "list", "extra argument"},
	}
	for _, command := range [][]string{{"note", "list"}, {"state", "history"}, {"arch", "history"}} {
		for _, limit := range []string{"0", "-1", "200001", "9223372036854775807", "9223372036854775808", "1.5"} {
			args = append(args, append(append([]string{}, command...), "--limit", limit))
		}
	}
	return args
}

func TestLaneReadsDoNotInitializeMissingMemory(t *testing.T) {
	for _, existing := range []string{"", ".memdolt/dolt/memory/.dolt/noms"} {
		t.Run(existing, func(t *testing.T) {
			base := scratchDir(t)
			if existing != "" {
				if err := os.MkdirAll(filepath.Join(base, existing), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before := repoFiles(t, base)
			for _, args := range [][]string{{"note", "list"}, {"state", "show"}, {"arch", "show"}, {"state", "history"}, {"arch", "history"}, {"command", "list"}, {"command", "get", "test"}} {
				if err := runMemdoltErr(t, append(args, "--dir", base, "--json")...); !strings.Contains(err, "memdolt init") {
					t.Fatal(err)
				}
				if after := repoFiles(t, base); !reflect.DeepEqual(before, after) {
					t.Fatalf("read initialized absent memory: %v -> %v", before, after)
				}
			}
		})
	}
}

func TestLaneReadsLeaveMCPNotesQueued(t *testing.T) {
	base, session, stop := renderNoteSession(t)
	note := queueRenderNote(t, session)
	var before [][]string
	humanInspect(t, base, func(st commandStore) { before = repoStateSnapshot(t, st) })
	for _, args := range [][]string{{"note", "list"}, {"note", "list", "--actor", note.Actor, "--since-days", "1"}, {"state", "history"}, {"arch", "history"}, {"command", "list"}} {
		if out := runMemdolt(t, append(args, "--dir", base, "--json")...); !strings.Contains(out, ":[]") {
			t.Fatalf("read flushed/exposed a queued note: %s", out)
		}
	}
	humanInspect(t, base, func(st commandStore) {
		if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
			t.Fatal("read changed queued-session main")
		}
	})
	stop()
	listed := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")).Notes
	if len(listed) != 1 || listed[0].ID != note.ID {
		t.Fatalf("read discarded or duplicated the note before orderly flush: %+v", listed)
	}
}
