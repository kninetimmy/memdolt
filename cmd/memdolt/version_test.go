package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCommandHumanReadable(t *testing.T) {
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute `memdolt version`: %v", err)
	}

	got := out.String()
	if !strings.HasPrefix(got, "memdolt version ") {
		t.Fatalf("expected a human-readable version line, got %q", got)
	}
}

func TestVersionCommandJSON(t *testing.T) {
	root := newRootCommand()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"version", "--json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute `memdolt version --json`: %v", err)
	}

	got := out.Bytes()

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(got), &payload); err != nil {
		t.Fatalf("expected a single valid JSON object, got %q: %v", got, err)
	}

	if v, ok := payload["version"]; !ok || v == "" {
		t.Fatalf("expected a non-empty \"version\" field, got %v", payload)
	}

	// Nothing but the JSON object (plus its trailing newline) should be on
	// stdout.
	if strings.Count(strings.TrimSpace(string(got)), "\n") != 0 {
		t.Fatalf("expected stdout to contain a single line, got %q", got)
	}
}
