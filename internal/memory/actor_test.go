package memory_test

import (
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
)

// TestNormalizeActor covers the vocabulary PRD §3.1 requires of a commit
// author: a person is "user", anything else is "agent:<name>", and the
// spellings a caller might use for one agent normalize to one identity so
// that a dolt_log query grouped by author is not answering about three.
func TestNormalizeActor(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		name string
	}{
		{raw: "", name: "user"},
		{raw: "   ", name: "user"},
		{raw: "user", name: "user"},
		{raw: "User", name: "user"},
		{raw: "claude-code", name: "agent:claude-code"},
		{raw: "Claude Code", name: "agent:claude-code"},
		{raw: "agent:claude-code", name: "agent:claude-code"},
		{raw: "agent:Claude Code", name: "agent:claude-code"},
		{raw: "  codex  ", name: "agent:codex"},
		{raw: "Claude   Code/2", name: "agent:claude-code-2"},
		{raw: "opencode_v1.2", name: "agent:opencode_v1.2"},
	} {
		actor, err := memory.NormalizeActor(tc.raw)
		if err != nil {
			t.Fatalf("NormalizeActor(%q): %v", tc.raw, err)
		}
		if actor.Name != tc.name {
			t.Errorf("NormalizeActor(%q).Name = %q, want %q", tc.raw, actor.Name, tc.name)
		}
		// The raw string is kept for §6.1's actor_raw columns, so that
		// normalizing loses nothing.
		if want := strings.TrimSpace(tc.raw); want != "" && actor.Raw != want {
			t.Errorf("NormalizeActor(%q).Raw = %q, want %q", tc.raw, actor.Raw, want)
		}
	}
}

// TestNormalizeActorRefusesWhatItCannotAttribute covers the fail-loudly
// half: an identity that would have to be truncated or invented is an
// error, because a silently shortened or empty attribution is a false one.
func TestNormalizeActorRefusesWhatItCannotAttribute(t *testing.T) {
	for _, raw := range []string{
		"!!!",                         // no identity characters at all
		"agent:???",                   // nor after the prefix
		strings.Repeat("a", 60),       // agent:<60 chars> exceeds actor VARCHAR(64)
		strings.Repeat("verbose", 40), // exceeds actor_raw VARCHAR(255)
	} {
		if actor, err := memory.NormalizeActor(raw); err == nil {
			t.Errorf("NormalizeActor(%q) = %+v, want an error", raw, actor)
		}
	}
}

// TestCommitAuthor covers the rendering dolt_log and dolt_blame are read
// through (§3.1): the normalized name, and an address that is a valid one.
func TestCommitAuthor(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		name  string
		email string
	}{
		{raw: "", name: "user", email: "user@memdolt.invalid"},
		{raw: "Claude Code", name: "agent:claude-code", email: "agent-claude-code@memdolt.invalid"},
	} {
		actor, err := memory.NormalizeActor(tc.raw)
		if err != nil {
			t.Fatalf("NormalizeActor(%q): %v", tc.raw, err)
		}
		author := actor.CommitAuthor()
		if author.Name != tc.name || author.Email != tc.email {
			t.Errorf("NormalizeActor(%q).CommitAuthor() = %q <%s>, want %q <%s>",
				tc.raw, author.Name, author.Email, tc.name, tc.email)
		}
		// store.Actor.Validate is what a commit request runs; an author it
		// rejects would fail every write by this actor.
		if err := author.Validate(); err != nil {
			t.Errorf("NormalizeActor(%q).CommitAuthor() is not a valid commit author: %v", tc.raw, err)
		}
	}
}
