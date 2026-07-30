package localdolt

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/store"
)

func testActor() store.Actor {
	return store.Actor{Name: "agent:claude-code", Email: "claude@memdolt.invalid"}
}

func TestBuildDSNMatchesTheDriversParser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".memdolt", "dolt")

	dsn, err := buildDSN(dir, testActor(), DatabaseName)
	if err != nil {
		t.Fatalf("build dsn: %v", err)
	}

	const prefix = "file://"
	if !strings.HasPrefix(dsn, prefix) {
		t.Fatalf("dsn %q does not start with %q", dsn, prefix)
	}

	// The driver splits on the first '?' and uses the left side verbatim,
	// without percent-decoding, so that half has to be the real path with
	// forward slashes.
	rest := strings.TrimPrefix(dsn, prefix)
	dirPart, query, found := strings.Cut(rest, "?")
	if !found {
		t.Fatalf("dsn %q has no query string", dsn)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if want := filepath.ToSlash(abs); dirPart != want {
		t.Fatalf("dsn directory = %q, want %q", dirPart, want)
	}
	if runtime.GOOS == "windows" {
		// "file://C:/..." — a third slash would make the drive letter part
		// of a host component the driver does not parse.
		if strings.HasPrefix(dirPart, "/") {
			t.Fatalf("windows dsn directory %q must not start with a slash", dirPart)
		}
	} else if !strings.HasPrefix(dirPart, "/") {
		t.Fatalf("unix dsn directory %q must be absolute", dirPart)
	}

	params, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("parse dsn query %q: %v", query, err)
	}
	for key, want := range map[string]string{
		"commitname":  testActor().Name,
		"commitemail": testActor().Email,
		"database":    DatabaseName,
	} {
		if got := params.Get(key); got != want {
			t.Errorf("dsn param %s = %q, want %q", key, got, want)
		}
	}
}

func TestBuildDSNRejectsPathsTheParserCannotRepresent(t *testing.T) {
	if _, err := buildDSN(filepath.Join(t.TempDir(), "what?now"), testActor(), DatabaseName); err == nil {
		t.Fatal("a data directory containing '?' was accepted")
	}
	if _, err := buildDSN(t.TempDir(), store.Actor{Name: "bad <name", Email: "a@b.invalid"}, DatabaseName); err == nil {
		t.Fatal("an actor that cannot be rendered as authorship was accepted")
	}
	if _, err := buildDSN(t.TempDir(), testActor(), ""); err == nil {
		t.Fatal("an empty database name was accepted")
	}
}

func TestQuoteIdentifier(t *testing.T) {
	// "memory" is a reserved word to the engine's parser; unquoted DDL
	// against it is a syntax error.
	if got, want := quoteIdentifier(DatabaseName), "`memory`"; got != want {
		t.Fatalf("quoteIdentifier(%q) = %q, want %q", DatabaseName, got, want)
	}
	if got, want := quoteIdentifier("we`ird"), "`we``ird`"; got != want {
		t.Fatalf("quoteIdentifier escaped badly: %q, want %q", got, want)
	}
}
