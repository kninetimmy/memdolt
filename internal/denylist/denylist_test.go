package denylist_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/denylist"
)

// theSecret stands in for whatever a deny-list exists to keep out of the
// store. No test lets it reach an error message.
const theSecret = "AKIAIOSFODNN7EXAMPLE"

func TestCheckNamesTheRuleAndNotTheMatchedText(t *testing.T) {
	list, err := denylist.Compile([]string{`nothing-matches-this`, `AKIA[0-9A-Z]{16}`})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	err = list.Check("a fact whose value is " + theSecret)
	if !errors.Is(err, denylist.ErrDenied) {
		t.Fatalf("check error = %v, want one matching ErrDenied", err)
	}

	var denied *denylist.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("check error = %v, want a *DeniedError", err)
	}
	if denied.Index != 1 {
		t.Fatalf("matched rule index = %d, want 1 (the rule that matches)", denied.Index)
	}
	if denied.Pattern != `AKIA[0-9A-Z]{16}` {
		t.Fatalf("matched rule = %q, want the configured pattern", denied.Pattern)
	}

	// The refusal has to name the rule an operator can find in the config
	// file, and must not repeat the text the rule exists to keep out.
	if !strings.Contains(err.Error(), `AKIA[0-9A-Z]{16}`) {
		t.Fatalf("refusal %q does not name the rule that matched", err)
	}
	if strings.Contains(err.Error(), theSecret) {
		t.Fatalf("refusal %q repeats the matched text back to the caller", err)
	}
}

func TestCheckScansEveryFieldOfAWrite(t *testing.T) {
	list, err := denylist.Compile([]string{theSecret})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// The match may be in any of the fields a lane declares, not only the
	// first: a fact's key as readily as its value.
	if err := list.Check("deploy.key", theSecret); !errors.Is(err, denylist.ErrDenied) {
		t.Fatalf("check of a later field = %v, want one matching ErrDenied", err)
	}
	if err := list.Check("deploy.key", "an ordinary value"); err != nil {
		t.Fatalf("check of text nothing matches = %v, want nil", err)
	}
}

func TestCompileRefusesAPatternThatIsNotAValidRegularExpression(t *testing.T) {
	list, err := denylist.Compile([]string{`ok`, `a(b`})
	if err == nil {
		t.Fatal("compile accepted a pattern that is not a valid regular expression")
	}
	if list != nil {
		// A partial list would enforce a deny-list its operator never
		// wrote, which is worse than refusing to run.
		t.Fatal("compile returned a list alongside its error")
	}
	if !strings.Contains(err.Error(), "deny_list.patterns[1]") {
		t.Fatalf("compile error %q does not say which pattern is at fault", err)
	}
}

func TestNoPatternsIsNoDenyList(t *testing.T) {
	for name, patterns := range map[string][]string{
		"nil":   nil,
		"empty": {},
	} {
		list, err := denylist.Compile(patterns)
		if err != nil {
			t.Fatalf("%s: compile: %v", name, err)
		}
		if list != nil {
			t.Fatalf("%s: compile returned a list, want nil for no patterns", name)
		}
		if err := list.Check("anything at all", theSecret); err != nil {
			t.Fatalf("%s: an unconfigured deny-list denied %v", name, err)
		}
	}
}

func TestLoadReadsTheDenyListTable(t *testing.T) {
	path := writeConfig(t, `
# The rest of the config file is not this package's business.
[render]
enabled = true

[deny_list]
patterns = [
  '(?i)\bAKIA[0-9A-Z]{16}\b',
  '-----BEGIN [A-Z ]*PRIVATE KEY-----',
]
`)

	list, err := denylist.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := list.Check("aws access key id: " + theSecret); !errors.Is(err, denylist.ErrDenied) {
		t.Fatalf("check = %v, want one matching ErrDenied", err)
	}
	if err := list.Check("an ordinary fact about the build"); err != nil {
		t.Fatalf("check of unremarkable text = %v, want nil", err)
	}
}

// TestLoadOnAMissingFileIsNoDenyList covers the distinction the whole
// fail-closed design rests on: a repository that configured no deny-list
// is not a deny-list that cannot be evaluated.
func TestLoadOnAMissingFileIsNoDenyList(t *testing.T) {
	list, err := denylist.Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("load of an absent config file = %v, want no error", err)
	}
	if list != nil {
		t.Fatal("load of an absent config file returned a list")
	}
}

func TestLoadRefusesADenyListItCannotEvaluate(t *testing.T) {
	cases := map[string]string{
		"toml that does not parse":    "[deny_list\npatterns = ['x']\n",
		"a pattern that is not regex": "[deny_list]\npatterns = ['a(b']\n",
		"a pattern that is not text":  "[deny_list]\npatterns = [42]\n",
	}
	for name, content := range cases {
		list, err := denylist.Load(writeConfig(t, content))
		if err == nil {
			t.Fatalf("%s: load accepted it", name)
		}
		if list != nil {
			t.Fatalf("%s: load returned a list alongside its error", name)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}
