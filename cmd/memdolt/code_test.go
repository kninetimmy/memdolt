package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/codeindex"
)

func TestCodeCLIWorksWithoutDoltOrOwnerRouting(t *testing.T) {
	base := scratchDir(t)
	for _, args := range [][]string{{"init", "--quiet"}} {
		if out, err := exec.Command("git", append([]string{"-C", base}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	before := decodeJSON[codeindex.StatusReport](t, runMemdolt(t, "code", "status", "--dir", base, "--json"))
	if before.Exists {
		t.Fatal("new code index exists")
	}
	if _, err := os.Stat(filepath.Join(base, ".memdolt")); !os.IsNotExist(err) {
		t.Fatalf("status created metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "source.go"), []byte("package code\n// Locate the canary.\nfunc Canary() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", base, "add", "--", "source.go").CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if err := os.Mkdir(filepath.Join(base, ".memdolt"), 0o700); err != nil {
		t.Fatal(err)
	}
	// These synthetic records are deliberately not valid owner protocol JSON.
	// The local code commands must never probe/route/open a Dolt owner.
	for _, name := range []string{"server.pid", "LOCK", "embeddings.sqlite", "other.sqlite"} {
		if err := os.WriteFile(filepath.Join(base, ".memdolt", name), []byte("synthetic unrelated sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	indexed := decodeJSON[codeindex.RefreshSummary](t, runMemdolt(t, "code", "index", "--dir", base, "--json"))
	if indexed.NewFiles != 1 || indexed.ChunksTotal != 1 || !indexed.Committed {
		t.Fatalf("index=%+v", indexed)
	}
	located := decodeJSON[codeindex.Response](t, runMemdolt(t, "locate", "Canary", "--dir", base, "--json"))
	if len(located.Results) != 1 || located.Results[0].Path != "source.go" || *located.Results[0].Symbol != "Canary" || located.Results[0].StartLine != 2 || located.Results[0].EndLine != 3 {
		t.Fatalf("locate=%+v", located)
	}
	if human := runMemdolt(t, "locate", "Canary", "--dir", base, "--no-refresh"); !strings.Contains(human, "source.go:2-3 [Canary]") {
		t.Fatalf("human output=%q", human)
	}
	golden := `{"version":1,"queries":[{"id":"match","kind":"match","query":"Canary","path_contains":["SOURCE.GO"],"symbol_contains":"canary"},{"id":"empty","kind":"empty","query":"zxqv"}]}`
	if err := os.WriteFile(filepath.Join(base, "golden.json"), []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	eval := decodeJSON[codeindex.EvalSummary](t, runMemdolt(t, "eval", "locate", "--golden", "golden.json", "--dir", base, "--json"))
	if eval.MatchPassesAtK != 1 || eval.EmptyPasses != 1 || eval.K != 3 {
		t.Fatalf("eval=%+v", eval)
	}
	removed := decodeJSON[codeindex.RemoveResult](t, runMemdolt(t, "code", "rm", "--dir", base, "--json"))
	if !removed.Removed {
		t.Fatalf("remove=%+v", removed)
	}
	if text := runMemdoltErr(t, "locate", "Canary", "--dir", base, "--no-refresh"); !strings.Contains(text, "code index is missing") {
		t.Fatalf("missing error=%s", text)
	}
	for _, name := range []string{"server.pid", "LOCK", "embeddings.sqlite", "other.sqlite"} {
		raw, err := os.ReadFile(filepath.Join(base, ".memdolt", name))
		if err != nil || string(raw) != "synthetic unrelated sentinel" {
			t.Fatalf("changed %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, ".memdolt", "dolt")); !os.IsNotExist(err) {
		t.Fatalf("code opened or created Dolt: %v", err)
	}
	for _, args := range [][]string{{"locate", "", "--dir", base}, {"locate", "x", "--limit", "-1", "--dir", base}, {"eval", "locate", "--golden", "golden.json", "--k", "0", "--dir", base}} {
		runMemdoltErr(t, args...)
	}
}

func TestLocatorSkillTemplatesAndProtectedGoldenCommand(t *testing.T) {
	for _, host := range []string{"claude", "codex", "opencode"} {
		for _, name := range []string{"locate", "eval-locate"} {
			path := repoFile("templates", "skills", host, "memdolt-"+name, "SKILL.md")
			if host == "claude" {
				path = repoFile("templates", "skills", host, "memdolt-"+name+".md")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, phrase := range []string{"floor", "corpus"} {
				if name == "locate" && phrase == "corpus" {
					continue
				}
				if !strings.Contains(string(raw), phrase) {
					t.Errorf("%s misses %s", path, phrase)
				}
			}
		}
	}
	raw, err := os.ReadFile(repoFile(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	command := "go test -tags golden,gms_pure_go ./tests/golden/... -run TestLocateGolden -count=1 -timeout 30m"
	if !strings.Contains(string(raw), "- name: Golden locator gate\n        if: matrix.os == 'ubuntu-latest'\n        run: "+command) &&
		!strings.Contains(strings.ReplaceAll(string(raw), "\r\n", "\n"), "- name: Golden locator gate\n        if: matrix.os == 'ubuntu-latest'\n        run: "+command) {
		t.Fatal("protected Ubuntu locator command missing")
	}
}
