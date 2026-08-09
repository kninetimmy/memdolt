package main

import (
	"os"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/layout"
)

// theSecret stands in for whatever a deny-list exists to keep out of a
// project's memory. Nothing here lets it reach an error message.
const theSecret = "AKIAIOSFODNN7EXAMPLE"

// denyRule matches theSecret and nothing else these tests write.
const denyRule = `AKIA[0-9A-Z]{16}`

// TestDirectLaneWritesAreScannedByTheDenyList runs PRD §11.3's deny-list
// through the direct lanes as a user meets them: a note, a task's notes and
// a narrative body, each carrying a secret, each refused at the CLI.
//
// The store-level test in internal/store/localdolt proves the check itself.
// This one proves the lanes reach it — that the text an agent typed is
// declared on the way down rather than dropped, which is the difference
// between a deny-list that is enforced and one that is merely implemented.
func TestDirectLaneWritesAreScannedByTheDenyList(t *testing.T) {
	base := initStore(t)
	writeDenyList(t, base, denyRule)

	for name, args := range map[string][]string{
		"note text":  {"note", "add", "the key is " + theSecret},
		"task notes": {"task", "add", "rotate the key", "--notes", theSecret},
		"narrative":  {"state", "set", "deploying with " + theSecret},
	} {
		err := runMemdoltErr(t, append(args, "--dir", base)...)
		if !strings.Contains(err, "deny_list.patterns[0]") {
			t.Errorf("%s: refusal %q does not name the rule that matched", name, err)
		}
		if strings.Contains(err, theSecret) {
			t.Errorf("%s: refusal %q repeats the secret back to the caller", name, err)
		}
	}

	// Refused means not written: the lanes' own listings are empty.
	if notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")); len(notes.Notes) != 0 {
		t.Fatalf("refused notes left %d row(s) behind", len(notes.Notes))
	}
	if tasks := decodeJSON[taskList](t, runMemdolt(t, "task", "list", "--dir", base, "--json")); len(tasks.Tasks) != 0 {
		t.Fatalf("refused tasks left %d row(s) behind", len(tasks.Tasks))
	}

	// The rule bounds the write, not the lane: text it does not match is
	// recorded as it always was (acceptance criterion 4's other half).
	runMemdolt(t, "note", "add", "the build is green", "--dir", base)
	if notes := decodeJSON[noteList](t, runMemdolt(t, "note", "list", "--dir", base, "--json")); len(notes.Notes) != 1 {
		t.Fatalf("after one allowed note the lane holds %d row(s), want 1", len(notes.Notes))
	}
}

// writeDenyList configures deny-list rules for the repository at base.
// Patterns are TOML literal strings: a regular expression's backslashes are
// not valid escapes in a basic string.
func writeDenyList(t *testing.T, base string, patterns ...string) {
	t.Helper()
	paths, err := layout.New(base)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}

	body := "[deny_list]\npatterns = [\n"
	for _, pattern := range patterns {
		if strings.Contains(pattern, "'") {
			t.Fatalf("test pattern %q cannot be written as a TOML literal string", pattern)
		}
		body += "  '" + pattern + "',\n"
	}
	body += "]\n"

	if err := os.WriteFile(paths.ConfigFile(), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", paths.ConfigFile(), err)
	}
}
