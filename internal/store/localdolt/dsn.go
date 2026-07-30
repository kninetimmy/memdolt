package localdolt

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/kninetimmy/memdolt/internal/store"
)

// buildDSN renders the embedded driver's data source name for a Dolt data
// directory.
//
// The driver's parser (github.com/dolthub/driver ParseDataSource) is
// literal: it strips the "file://" prefix, takes everything up to the
// first '?' as the directory, and never percent-decodes that part. So the
// path goes in verbatim with forward slashes — "file:///home/x/.memdolt/dolt"
// on Unix, "file://C:/x/.memdolt/dolt" on Windows — and a path containing
// '?' cannot be expressed at all. Only the query string is encoded.
func buildDSN(dataDir string, actor store.Actor, database string) (string, error) {
	if err := actor.Validate(); err != nil {
		return "", fmt.Errorf("dolt commit identity: %w", err)
	}
	if database == "" {
		return "", fmt.Errorf("database name is required")
	}

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve dolt data directory %q: %w", dataDir, err)
	}
	slashed := filepath.ToSlash(abs)
	if strings.Contains(slashed, "?") {
		return "", fmt.Errorf(
			"dolt data directory %q contains '?', which the embedded driver's data source parser treats as the start of the query string",
			abs)
	}

	params := url.Values{}
	params.Set("commitname", actor.Name)
	params.Set("commitemail", actor.Email)
	params.Set("database", database)

	return "file://" + slashed + "?" + params.Encode(), nil
}

// quoteIdentifier renders a MySQL identifier safely. memdolt's database
// name is "memory" (PRD §5.3), which the parser treats as a reserved word,
// so it always has to be quoted.
func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
