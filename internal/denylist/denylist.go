// Package denylist refuses memory writes whose text matches a rule the
// repository configured (PRD §11.3's `[deny_list]`).
//
// # What the deny-list is for
//
// PRD §13.3 states the stance it implements: memdolt keeps secrets out of
// the store rather than promising to delete them afterwards, because a
// secret that reaches a Dolt commit is removable only by the manual,
// exceptional, git-filter-branch-class history rewrite documented there.
// A refused write is cheap; a rewritten history is not. That is why every
// failure mode here refuses the write.
//
// # What a rule is
//
// A rule is a Go regular expression, matched unanchored against the text a
// write declares — a fact's key and value, a decision's title and
// rationale, a task's or note's text, a narrative body. Regular
// expressions rather than plain substrings, because a substring can never
// fail to be evaluated: "a deny-list that cannot be evaluated refuses the
// write" needs a rule that can be wrong, and an invalid pattern is exactly
// that case.
//
// Go's regexp is RE2, so a rule runs in time linear in the text it scans.
// A pathological pattern in the config file cannot turn a write into a
// hang, only into a refusal.
//
// # What an error may say
//
// A refusal names the rule that matched and never the text that matched
// it. The matched text is, by construction, the thing the rule exists to
// keep out of the store — repeating it in an error would put it in the
// caller's logs, its terminal scrollback and its agent transcript, which
// is most of what committing it would have done.
package denylist

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/BurntSushi/toml"
)

// ErrDenied reports a write refused by a deny-list rule. Callers identify
// the condition with errors.Is; *DeniedError carries the detail.
var ErrDenied = errors.New("write matches a deny-list rule")

// DeniedError names the configured rule a write matched.
//
// It deliberately carries no copy of the matched text; see the package
// documentation. Index is the rule's position in `deny_list.patterns`, so
// that an operator can find it in a config file that has several.
type DeniedError struct {
	Index   int
	Pattern string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("%v: deny_list.patterns[%d] = %q", ErrDenied, e.Index, e.Pattern)
}

// Unwrap makes errors.Is(err, ErrDenied) report true.
func (e *DeniedError) Unwrap() error { return ErrDenied }

// List is a compiled deny-list. A nil *List is a repository with no
// deny-list configured, and denies nothing.
type List struct {
	rules []*regexp.Regexp
}

// Compile turns configured patterns into a deny-list.
//
// It fails on the first pattern that is not a valid regular expression,
// and returns no list at all rather than a partial one: a caller that
// enforced the rules that did compile would be enforcing a deny-list its
// operator never wrote.
//
// No patterns means no deny-list, reported as a nil *List.
func Compile(patterns []string) (*List, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	rules := make([]*regexp.Regexp, 0, len(patterns))
	for i, pattern := range patterns {
		rule, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("denylist: deny_list.patterns[%d] = %q is not a valid regular expression: %w", i, pattern, err)
		}
		rules = append(rules, rule)
	}
	return &List{rules: rules}, nil
}

// Load reads the `[deny_list]` table from the TOML config file at path and
// compiles it (PRD §11.3):
//
//	[deny_list]
//	patterns = [
//	  '(?i)\bAKIA[0-9A-Z]{16}\b',
//	  '-----BEGIN [A-Z ]*PRIVATE KEY-----',
//	]
//
// Patterns are ordinarily written as TOML *literal* strings, in single
// quotes, because a regular expression's backslashes are not valid escapes
// in a TOML basic string.
//
// A config file that is not there is not a deny-list that cannot be
// evaluated: it is a repository that configured none, and Load reports a
// nil *List and no error. Every other failure — a file that cannot be
// read, TOML that does not parse, a `[deny_list]` table that decodes no
// `patterns` key, a pattern that does not compile — is an error, and a
// caller that fails closed on it refuses the write.
//
// # A table with no patterns key
//
// `pattern = [...]`, `patterns_ = [...]` and a bare `[deny_list]` all
// decode to zero patterns, which read from the decoded struct alone is
// indistinguishable from a repository that configured no deny-list: a
// typo would disable enforcement and say nothing. Load takes the table's
// presence as an operator who meant to configure rules, and refuses. An
// explicit `patterns = []` is the other case — the key is decoded, so the
// empty list is a decision somebody wrote down, and it loads as no
// deny-list.
//
// Load keys on the table's name, so it cannot see past a misspelling of
// *that*: `[denylist]` is a config file with no `[deny_list]` table in
// it, and loads as no deny-list. Closing that would mean refusing on any
// key Load did not decode, which the rest of PRD §11.3's unread config
// would trip on every write.
//
// This refusal binds Load alone, not every entry point of its kind:
// Compile takes patterns a caller already holds, where zero of them means
// zero and is no evidence of a typo, so Compile(nil) is still a nil *List
// and no error. Only the path that reads a config file can tell an empty
// deny-list from an unread one.
//
// Load reads the whole config file but takes only `[deny_list]` from it.
// The rest of PRD §11.3's keys have no reader yet; when the config layer
// that owns them lands, it hands its patterns to Compile and this function
// goes away.
func Load(path string) (*List, error) {
	var cfg struct {
		DenyList struct {
			Patterns []string `toml:"patterns"`
		} `toml:"deny_list"`
	}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("denylist: read %s: %w", path, err)
	}
	if md.IsDefined("deny_list") && !md.IsDefined("deny_list", "patterns") {
		return nil, fmt.Errorf(
			"denylist: %s: a [deny_list] table is present with no patterns decoded: "+
				"check the spelling of the patterns key, or write patterns = [] "+
				"to configure an empty deny-list deliberately", path)
	}
	list, err := Compile(cfg.DenyList.Patterns)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list, nil
}

// Check refuses the first text that matches a rule, with a *DeniedError
// naming the rule (errors.Is(err, ErrDenied) reports true). It reports nil
// for a nil list, for no texts, and for texts nothing matches.
func (l *List) Check(texts ...string) error {
	if l == nil {
		return nil
	}
	for _, text := range texts {
		for i, rule := range l.rules {
			if rule.MatchString(text) {
				return &DeniedError{Index: i, Pattern: rule.String()}
			}
		}
	}
	return nil
}
