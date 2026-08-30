package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

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
