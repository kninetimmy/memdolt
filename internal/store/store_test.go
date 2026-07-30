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
		Message:    "note batch (1)",
		Author:     actor,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	cases := map[string]store.CommitRequest{
		"no statements":   {Message: "m", Author: actor},
		"empty statement": {Statements: []store.Statement{{SQL: "  "}}, Message: "m", Author: actor},
		"no message":      {Statements: valid.Statements, Author: actor},
		"multiline message": {
			Statements: valid.Statements,
			Message:    "line one\nline two",
			Author:     actor,
		},
		"invalid author": {Statements: valid.Statements, Message: "m"},
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
