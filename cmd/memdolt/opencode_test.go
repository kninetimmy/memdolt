package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	opencodehost "github.com/kninetimmy/memdolt/internal/opencode"
)

func TestOpenCodeWrapUpNoteVerifiesBeforeOpeningTheStore(t *testing.T) {
	base := scratchDir(t)
	_, err := logOpenCodeWrapUpNote(
		context.Background(), base, "ses_current", "must not be written",
		func(context.Context, string) (opencodehost.SessionInfo, error) {
			return opencodehost.SessionInfo{}, errors.New("identity unavailable")
		},
	)
	if err == nil {
		t.Fatal("wrap-up note succeeded without verified identity")
	}
	if _, statErr := os.Stat(pathsFor(t, base).Dir()); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("failed identity verification opened or created the store: %v", statErr)
	}
}

func TestOpenCodeWrapUpNoteStoresExactMetadataAndCLIProvenance(t *testing.T) {
	base := initStore(t)
	result, err := logOpenCodeWrapUpNote(
		context.Background(), base, "ses_current", "verified OpenCode wrap-up",
		func(_ context.Context, id string) (opencodehost.SessionInfo, error) {
			if id != "ses_current" {
				t.Fatalf("verifier received id %q", id)
			}
			return opencodehost.SessionInfo{
				SessionID: "ses_current", AgentID: "build", ProviderID: "openai",
				ModelID: "gpt-5.6", Variant: "xhigh",
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("log verified wrap-up note: %v", err)
	}
	if result.Actor != "agent:opencode" || result.ActorRaw != "cli" ||
		result.Text != "verified OpenCode wrap-up" || result.SessionID != "ses_current" ||
		result.AgentID != "build" || result.ProviderID != "openai" ||
		result.ModelID != "gpt-5.6" || result.Variant != "xhigh" {
		t.Fatalf("wrap-up note = %+v, want exact metadata and actor provenance", result)
	}

	for query, want := range map[string]string{
		"SELECT actor FROM session_notes":       "agent:opencode",
		"SELECT actor_raw FROM session_notes":   "cli",
		"SELECT text FROM session_notes":        "verified OpenCode wrap-up",
		"SELECT session_id FROM session_notes":  "ses_current",
		"SELECT agent_id FROM session_notes":    "build",
		"SELECT provider_id FROM session_notes": "openai",
		"SELECT model_id FROM session_notes":    "gpt-5.6",
		"SELECT variant FROM session_notes":     "xhigh",
	} {
		got := queryStrings(t, base, query)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want [%q]", query, got, want)
		}
	}
	if got := queryStrings(t, base,
		"SELECT committer FROM dolt_log WHERE message LIKE 'note add %'"); len(got) != 1 || got[0] != "agent:opencode" {
		t.Fatalf("wrap-up commit author = %q, want [agent:opencode]", got)
	}
}

func TestOpenCodeCLIRefusesBeforeStoreOpen(t *testing.T) {
	for _, tc := range []struct {
		name, id, response string
		failed             bool
	}{
		{"invalid id", "ses/bad", `{"data":{"id":"ses_current"}}`, false},
		{"failed API", "ses_current", `{"data":{"id":"ses_current"}}`, true},
		{"malformed JSON", "ses_current", `{"data":`, false},
		{"missing identity", "ses_current", `{"data":{"agent":"build"}}`, false},
		{"mismatched identity", "ses_current", `{"data":{"id":"ses_other"}}`, false},
		{"uppercase sibling overrides mismatched identity", "ses_current", `{"data":{"id":"ses_other","ID":"ses_current"}}`, false},
		{"uppercase envelope and identity", "ses_current", `{"DATA":{"ID":"ses_current"}}`, false},
		{"wrong-case data", "ses_current", `{"Data":{"id":"ses_current"}}`, false},
		{"wrong-case identity", "ses_current", `{"data":{"Id":"ses_current"}}`, false},
		{"malformed metadata", "ses_current", `{"data":{"id":"ses_current","model":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeOpenCodeAPI(t, tc.response, tc.failed)
			base := scratchDir(t)
			runMemdoltErr(t, "opencode", "wrap-up-note", tc.id, "must not be written", "--dir", base)
			if _, err := os.Stat(pathsFor(t, base).Dir()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("identity refusal opened or created the store: %v", err)
			}
			_, err := os.Stat(filepath.Join(bin, "called"))
			if tc.name == "invalid id" {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("invalid ID invoked a process: %v", err)
				}
			} else if err != nil {
				t.Fatalf("API process did not run: %v", err)
			}
		})
	}
}

func TestOpenCodeCLIRecordsVerifiedMetadataDirectlyAndThroughOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owner"}[routed], func(t *testing.T) {
			fakeOpenCodeAPI(t, `{
  "data": {
    "id":"ses_current", "ID":"ses_other",
    "agent":"user", "Agent":"ignored",
    "model": {
      "providerID":" provider ", "ProviderID":"ignored",
      "id":"model", "ID":"ignored", "variant":"max", "Variant":"ignored"
    },
    "Model":false
  },
  "DATA":{"id":"ses_other"}, "unknown":42
}`, false)
			base := initStore(t)
			if routed {
				serveStore(t, base)
			}
			info := decodeJSON[opencodehost.SessionInfo](t, runMemdolt(t,
				"opencode", "session-info", "ses_current", "--json"))
			note := decodeJSON[noteInfo](t, runMemdolt(t,
				"opencode", "wrap-up-note", "ses_current", "a summary\nwith two lines", "--dir", base, "--json"))
			if info.SessionID != "ses_current" || note.SessionID != info.SessionID || note.AgentID != "user" ||
				note.ProviderID != " provider " || note.ModelID != "model" || note.Variant != "max" ||
				note.Actor != "agent:opencode" || note.ActorRaw != "cli" || note.Text != "a summary\nwith two lines" {
				t.Fatalf("CLI changed verified metadata, text or actor: %+v / %+v", info, note)
			}
			listed := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json"))
			if len(listed.Notes) != 1 || listed.Notes[0] != note.Note {
				t.Fatalf("stored note = %+v, want %+v", listed, note.Note)
			}
		})
	}
}

func TestNoteProvenanceIsScannedForSingleAndBatchedOwnerWrites(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "owner"}[routed], func(t *testing.T) {
			ctx := context.Background()
			base := initStore(t)
			writeDenyList(t, base, denyRule)
			if routed {
				serveStore(t, base)
			}
			st, err := openCommandStore(ctx, base, openCodeCLIActor.CommitAuthor())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			}()
			lanes := memory.New(st, openCodeCLIActor)
			for _, provenance := range []memory.NoteProvenance{
				{SessionID: theSecret}, {AgentID: theSecret}, {ProviderID: theSecret},
				{ModelID: theSecret}, {Variant: theSecret},
			} {
				_, _, err := lanes.LogNoteWithProvenance(ctx, "safe text", provenance)
				if err == nil || !strings.Contains(err.Error(), "deny_list.patterns[0]") || strings.Contains(err.Error(), theSecret) {
					t.Fatalf("single-note provenance scan = %v", err)
				}
				note, err := lanes.PrepareNote("safe text")
				if err != nil {
					t.Fatal(err)
				}
				note.NoteProvenance = provenance
				_, err = lanes.CommitNotes(ctx, []memory.Note{note})
				if err == nil || !strings.Contains(err.Error(), "deny_list.patterns[0]") || strings.Contains(err.Error(), theSecret) {
					t.Fatalf("batch provenance scan = %v", err)
				}
			}
			for _, invalid := range []string{strings.Repeat("m", 256), "\xff"} {
				provenance := memory.NoteProvenance{ModelID: invalid}
				if _, _, err := lanes.LogNoteWithProvenance(ctx, "safe text", provenance); err == nil {
					t.Fatal("single-note write accepted malformed provenance")
				}
				note, err := lanes.PrepareNote("safe text")
				if err != nil {
					t.Fatal(err)
				}
				note.NoteProvenance = provenance
				if _, err := lanes.CommitNotes(ctx, []memory.Note{note}); err == nil {
					t.Fatal("batch write accepted malformed provenance")
				}
			}
			if notes, err := lanes.Notes(ctx, 10); err != nil || len(notes) != 0 {
				t.Fatalf("refused writes left notes: %+v / %v", notes, err)
			}

			first, err := lanes.PrepareNote("first note")
			if err != nil {
				t.Fatal(err)
			}
			first.NoteProvenance = memory.NoteProvenance{SessionID: "ses_batch", AgentID: "build"}
			second, err := lanes.PrepareNote("ordinary note")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lanes.CommitNotes(ctx, []memory.Note{first, second}); err != nil {
				t.Fatal(err)
			}
			notes, err := lanes.Notes(ctx, 10)
			if err != nil || len(notes) != 2 || notes[0] != second || notes[1] != first {
				t.Fatalf("batch readback = %+v / %v", notes, err)
			}
			rows, err := st.Query(ctx, "SELECT message, committer FROM dolt_log WHERE message LIKE 'note %'")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var message, author string
			if !rows.Next() {
				t.Fatalf("no batch commit: %v", rows.Err())
			}
			if err := rows.Scan(&message, &author); err != nil {
				t.Fatal(err)
			}
			if message != "note batch (2)" || author != "agent:opencode" || rows.Next() || rows.Err() != nil {
				t.Fatalf("batch changed commit count or attribution: %q / %q / %v", message, author, rows.Err())
			}
		})
	}
}

// The fake API is a real child process found on PATH. Its response is read
// from a file so JSON content never becomes shell source, even in a fixture.
func fakeOpenCodeAPI(t *testing.T, response string, fail bool) string {
	bin := scratchDir(t)
	writeTestFile(t, filepath.Join(bin, "session.json"), response)
	program, script := "opencode2", "#!/bin/sh\n"+
		"test \"$1\" = api && test \"$2\" = get && test \"$3\" = /api/session/ses_current || exit 90\n"+
		"printf called > \"$(dirname \"$0\")/called\"\n"+
		"cat \"$(dirname \"$0\")/session.json\"\n"
	if runtime.GOOS == "windows" {
		program, script = "opencode2.cmd", "@echo off\r\n"+
			"if not \"%~1\"==\"api\" exit /b 90\r\n"+
			"if not \"%~2\"==\"get\" exit /b 90\r\n"+
			"if not \"%~3\"==\"/api/session/ses_current\" exit /b 90\r\n"+
			"echo called>\"%~dp0called\"\r\n"+
			"type \"%~dp0session.json\"\r\n"
		if fail {
			script += "exit /b 1\r\n"
		}
	} else if fail {
		script += "exit 1\n"
	}
	if err := os.WriteFile(filepath.Join(bin, program), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}
