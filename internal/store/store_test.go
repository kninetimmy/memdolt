package store_test

import (
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

func TestActorValidateRejectsAuthorshipDelimiters(t *testing.T) {
	valid := store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid actor rejected: %v", err)
	}
	if got, want := valid.String(), "agent:claude-code <claude@memdolt.invalid>"; got != want {
		t.Fatalf("actor string = %q, want %q", got, want)
	}

	cases := map[string]store.Actor{
		"empty name":        {Name: "", Email: "a@b.invalid"},
		"blank name":        {Name: "   ", Email: "a@b.invalid"},
		"empty email":       {Name: "user", Email: ""},
		"angle in name":     {Name: "user <root", Email: "a@b.invalid"},
		"angle in email":    {Name: "user", Email: "a@b.invalid> x <c"},
		"newline in name":   {Name: "user\nroot", Email: "a@b.invalid"},
		"carriage in email": {Name: "user", Email: "a@b.invalid\rx"},
	}
	for name, actor := range cases {
		if err := actor.Validate(); err == nil {
			t.Errorf("%s: actor %+v was accepted", name, actor)
		}
	}
}

func TestCommitRequestValidate(t *testing.T) {
	actor := store.Actor{Name: "user", Email: "user@memdolt.invalid"}
	valid := store.CommitRequest{
		Statements: []store.Statement{{SQL: "INSERT INTO t (k) VALUES (?)", Args: []any{"k"}}},
		Text:       []string{"the note this write records"},
		Message:    "note batch (1)",
		Author:     actor,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	// The other half of the declaration: a write that says it has no text
	// for the deny-list to scan is valid too.
	declaresNone := valid
	declaresNone.Text, declaresNone.NoText = nil, true
	if err := declaresNone.Validate(); err != nil {
		t.Fatalf("request declaring NoText rejected: %v", err)
	}

	cases := map[string]store.CommitRequest{
		"no statements":   {Text: valid.Text, Message: "m", Author: actor},
		"empty statement": {Statements: []store.Statement{{SQL: "  "}}, Text: valid.Text, Message: "m", Author: actor},
		"no message":      {Statements: valid.Statements, Text: valid.Text, Author: actor},
		"multiline message": {
			Statements: valid.Statements,
			Text:       valid.Text,
			Message:    "line one\nline two",
			Author:     actor,
		},
		"invalid author": {Statements: valid.Statements, Text: valid.Text, Message: "m"},

		// The deny-list tripwire (PRD §11.3). A request that declares
		// neither text nor NoText is the shape a lane takes when nobody
		// thought about the deny-list at all, and it is refused rather
		// than committed unscanned.
		"declares neither text nor NoText": {
			Statements: valid.Statements,
			Message:    "m",
			Author:     actor,
		},
		"declares both text and NoText": {
			Statements: valid.Statements,
			Text:       valid.Text,
			NoText:     true,
			Message:    "m",
			Author:     actor,
		},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil {
			t.Errorf("%s: request was accepted", name)
		}
	}
}

func TestErrNotOpenIsDistinct(t *testing.T) {
	if strings.TrimSpace(store.ErrNotOpen.Error()) == "" {
		t.Fatal("ErrNotOpen must carry a message")
	}
	if store.ErrLocked == nil {
		t.Fatal("ErrLocked must be a usable sentinel")
	}
}
