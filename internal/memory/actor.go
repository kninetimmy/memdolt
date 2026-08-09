package memory

import (
	"fmt"
	"strings"

	"github.com/kninetimmy/memdolt/internal/store"
)

// AgentPrefix marks an actor that is not a person (PRD §3.1: user,
// agent:claude-code, agent:codex).
const AgentPrefix = "agent:"

// userActorName is the identity of a person at a terminal.
const userActorName = "user"

// Column widths from §6.1: an actor that does not fit is refused rather
// than truncated, because a silently shortened identity is a false one.
const (
	maxActorName = 64  // actor VARCHAR(64)
	maxActorRaw  = 255 // actor_raw VARCHAR(255)
)

// Actor is a caller's identity in the vocabulary of PRD §3.1.
//
// Name is the normalized form — "user" or "agent:<name>" — and it is what
// a Dolt commit is authored by and what the actor column of a row carries.
// Raw is what the caller supplied, kept for the actor_raw column of §6.1
// so that normalization loses nothing: a note written by "Claude Code"
// records both the identity provenance queries group by and the string
// that produced it.
type Actor struct {
	Name string `json:"name"`
	Raw  string `json:"raw"`
}

// UserActor is the CLI's default identity: someone running a command.
var UserActor = Actor{Name: userActorName, Raw: userActorName}

// NormalizeActor maps a caller-supplied identity onto §3.1's vocabulary.
//
// An empty identity is a person, because that is what an unattributed CLI
// invocation is — the CLI is run by whoever is at the terminal. This is
// not the MCP server's rule, and deliberately not: §7.5 makes a request
// that arrives over MCP with no client attribution agent:unknown, an
// untrusted writer, because a tool call is not a person by default.
//
// Anything else is an agent, named under AgentPrefix. The name is folded
// to lower case and reduced to the characters an identity is compared by,
// so that "Claude Code", "claude-code" and "agent:Claude Code" are one
// actor in dolt_log rather than three.
func NormalizeActor(raw string) (Actor, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return UserActor, nil
	}
	if len(trimmed) > maxActorRaw {
		return Actor{}, fmt.Errorf("actor %q is longer than the %d characters actor_raw holds",
			trimmed, maxActorRaw)
	}

	lowered := strings.ToLower(trimmed)
	if lowered == userActorName {
		return Actor{Name: userActorName, Raw: trimmed}, nil
	}

	slug := slugify(strings.TrimPrefix(lowered, AgentPrefix))
	if slug == "" {
		return Actor{}, fmt.Errorf("actor %q has no name characters to normalize "+
			"(letters, digits, '.', '_' or '-')", trimmed)
	}
	name := AgentPrefix + slug
	if len(name) > maxActorName {
		return Actor{}, fmt.Errorf("actor %q normalizes to %q, longer than the %d characters actor holds",
			trimmed, name, maxActorName)
	}
	return Actor{Name: name, Raw: trimmed}, nil
}

// slugify keeps the identity characters of an already-lowercased string
// and collapses every run of anything else to a single '-'.
func slugify(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out = append(out, r)
		case len(out) > 0 && out[len(out)-1] != '-':
			out = append(out, '-')
		}
	}
	return strings.Trim(string(out), "-")
}

// CommitAuthor renders the actor as Dolt commit authorship, which is what
// makes dolt_log and dolt_blame answer "who wrote this" with no
// application code (§3.1).
//
// The address is in .invalid (RFC 2606) because memdolt asserts an
// identity, not a mailbox, and its local part swaps the ':' of
// agent:claude-code for a '-', which is not a character an address may
// carry.
func (a Actor) CommitAuthor() store.Actor {
	return store.Actor{
		Name:  a.Name,
		Email: strings.ReplaceAll(a.Name, ":", "-") + "@memdolt.invalid",
	}
}
