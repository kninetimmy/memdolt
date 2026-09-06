package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/cmd/dolt/cli"

	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestCloneCommandJSONHumanAndReopen(t *testing.T) {
	source := initStore(t)
	runMemdolt(t, "task", "add", "clone command task", "--dir", source)
	runMemdolt(t, "note", "add", "clone command note", "--dir", source)
	remotePath := filepath.ToSlash(scratchDir(t))
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}
	remote := "file://" + remotePath
	dsn := "file://" + filepath.ToSlash(filepath.Join(source, ".memdolt", "dolt")) +
		"?commitname=user&commitemail=user%40memdolt.invalid&database=memory"
	db, err := sql.Open("dolt", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []store.Statement{
		{SQL: "CALL DOLT_REMOTE('add', 'origin', ?)", Args: []any{remote}},
		{SQL: "CALL DOLT_PUSH('origin', 'main')"},
	} {
		if _, err := db.Exec(statement.SQL, statement.Args...); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Capture the actual global writer the pinned clone engine uses. Setting
	// Cobra's writer alone would miss progress leaking directly to stdout.
	oldProgress := cli.CliOut
	progress := &bytes.Buffer{}
	cli.CliOut = progress
	defer func() { cli.CliOut = oldProgress }()
	base := scratchDir(t)
	out := runMemdolt(t, "clone", remote, "--dir", base, "--json")
	var result localdolt.CloneResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.MainCommit == "" || result.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("clone stdout = %q, %v", out, err)
	}
	if progress.Len() != 0 {
		t.Fatalf("Dolt progress escaped: %q", progress)
	}
	var status repoStatusReport
	if err := json.Unmarshal([]byte(runMemdolt(t, "repo", "status", "--dir", base, "--json")), &status); err != nil || status.MainCommit != result.MainCommit || !status.Clean {
		t.Fatalf("reopened status = %+v, %v", status, err)
	}
	if out := runMemdolt(t, "task", "list", "--dir", base); !strings.Contains(out, "clone command task") {
		t.Fatalf("reopened tasks = %q", out)
	}
	if out := runMemdolt(t, "note", "list", "--dir", base); !strings.Contains(out, "clone command note") {
		t.Fatalf("reopened notes = %q", out)
	}
	if out := runMemdolt(t, "clone", remote, "--dir", scratchDir(t)); !strings.Contains(out, "cloned memdolt store at") || !strings.Contains(out, result.MainCommit) {
		t.Fatalf("human clone = %q", out)
	}
	for _, args := range [][]string{
		{"clone", remote, "--dir", base, "--json"},
		{"clone", "http://user:secret@host/db", "--dir", scratchDir(t), "--json"},
		{"clone", "http://host/db", "--user", "", "--dir", scratchDir(t), "--json"},
		{"clone", "file:///definitely-absent-memdolt-remote", "--dir", scratchDir(t), "--json"},
	} {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		err := root.Execute()
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("failed clone output=%q, err=%v", out.String(), err)
		}
	}
}

func TestCloneRefusesManagedSymlinksAndNonemptyDirectories(t *testing.T) {
	for _, name := range []string{"state link", "data link", "data file", "nonempty data"} {
		t.Run(name, func(t *testing.T) {
			base, elsewhere := scratchDir(t), scratchDir(t)
			state := filepath.Join(base, ".memdolt")
			data := filepath.Join(state, "dolt")
			if name == "state link" {
				if err := os.Symlink(elsewhere, state); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "data link":
					if err := os.Symlink(elsewhere, data); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				case "data file":
					if err := os.WriteFile(data, []byte("existing data file"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "nonempty data":
					if err := os.Mkdir(data, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(data, "foreign"), []byte("existing content"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			root := newRootCommand()
			root.SetArgs([]string{"clone", "http://127.0.0.1:1/never-contact", "--dir", base, "--json"})
			var output bytes.Buffer
			root.SetOut(&output)
			if err := root.Execute(); err == nil || strings.Contains(err.Error(), "access clone remote") || output.Len() != 0 {
				t.Fatalf("unsafe destination refusal: %v, output=%q", err, output.String())
			}
			entries, err := os.ReadDir(elsewhere)
			if err != nil || len(entries) != 0 {
				t.Fatalf("clone touched symlink target: %v, %v", entries, err)
			}
		})
	}
}
