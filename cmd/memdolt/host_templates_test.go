package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestTrackedHostRegistrationsUseNativeCoexistingShapes(t *testing.T) {
	var claude struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readRepoJSON(t, ".mcp.json", &claude)
	server, ok := claude.MCPServers["memdolt"]
	if !ok || server.Command != "memdolt" || !reflect.DeepEqual(server.Args, []string{"serve"}) {
		t.Fatalf(".mcp.json memdolt registration = %+v, present %t", server, ok)
	}

	var openCode struct {
		MCP struct {
			Servers map[string]struct {
				Type     string   `json:"type"`
				Command  []string `json:"command"`
				Disabled bool     `json:"disabled"`
			} `json:"servers"`
		} `json:"mcp"`
		Commands map[string]json.RawMessage `json:"commands"`
		Skills   []string                   `json:"skills"`
	}
	readRepoJSON(t, "opencode.json", &openCode)
	openCodeServer, ok := openCode.MCP.Servers["memdolt"]
	if !ok || openCodeServer.Type != "local" || openCodeServer.Disabled ||
		!reflect.DeepEqual(openCodeServer.Command, []string{"memdolt", "serve"}) {
		t.Fatalf("opencode.json memdolt registration = %+v, present %t", openCodeServer, ok)
	}
	if got := sortedRawKeys(openCode.Commands); !reflect.DeepEqual(got, []string{"memdolt-catch-up", "memdolt-check-init", "memdolt-eval-locate", "memdolt-global", "memdolt-init-project", "memdolt-locate", "memdolt-recall", "memdolt-wrap-up"}) {
		t.Fatalf("OpenCode commands = %q, want namespaced shipped workflows", got)
	}
	for name, raw := range openCode.Commands {
		var command struct {
			Template string `json:"template"`
		}
		if err := json.Unmarshal(raw, &command); err != nil {
			t.Fatal(err)
		}
		if want := "Use the " + name + " skill. Arguments: $ARGUMENTS"; command.Template != want {
			t.Errorf("OpenCode %s template = %q, want %q", name, command.Template, want)
		}
	}
	if !reflect.DeepEqual(openCode.Skills, []string{"templates/skills/opencode"}) {
		t.Fatalf("OpenCode skill roots = %q", openCode.Skills)
	}
}

func TestCoreSkillTemplatesMatchAcrossHostsAndUseImplementedTools(t *testing.T) {
	want := []string{"memdolt-catch-up", "memdolt-check-init", "memdolt-eval-locate", "memdolt-global", "memdolt-init-project", "memdolt-locate", "memdolt-recall", "memdolt-wrap-up"}
	sets := map[string][]string{
		"claude":   flatSkillNames(t, repoFile("templates", "skills", "claude")),
		"codex":    directorySkillNames(t, repoFile("templates", "skills", "codex")),
		"opencode": directorySkillNames(t, repoFile("templates", "skills", "opencode")),
	}
	for host, got := range sets {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s skills = %q, want %q", host, got, want)
		}
	}

	for _, path := range allSkillFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		if filepath.Base(path) == "SKILL.md" {
			name = filepath.Base(filepath.Dir(path))
		}
		frontmatter := strings.SplitN(strings.ReplaceAll(text, "\r\n", "\n"), "---\n", 3)
		if len(frontmatter) != 3 || frontmatter[0] != "" || !strings.Contains(frontmatter[1], "name: "+name+"\n") {
			t.Errorf("%s frontmatter must match entry point %q", path, name)
		}
		// Before #155 this list also forbade doc_add and repo_status even
		// though both were implemented. Only genuinely deferred tools remain.
		// Before #173 history was deferred; its native reader now ships.
		for _, deferredTool := range []string{
			"`archive_transcript`",
		} {
			if strings.Contains(text, deferredTool) {
				t.Errorf("%s invokes deferred tool %s", path, deferredTool)
			}
		}
	}

	for _, path := range []string{
		repoFile("templates", "skills", "claude", "memdolt-wrap-up.md"),
		repoFile("templates", "skills", "codex", "memdolt-wrap-up", "SKILL.md"),
		repoFile("templates", "skills", "opencode", "memdolt-wrap-up", "SKILL.md"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		for _, required := range []string{"explicit per-item approval", "propose_fact", "propose_decision", "Never promote", "`render`", "committed main", "backups before retrying"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing proposal/review boundary phrase %q", path, required)
			}
		}
		for _, required := range []string{"`repo_pull`", "`repo_push`", "Never synthesize confirmation", "nextCursor", "pending/global proposals remain excluded"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing transfer boundary %q", path, required)
			}
		}
		for _, required := range []string{"memdolt state show --dir", "memdolt arch show --dir", "approval for each changed narrative", "narratives require no write", "body on stdin", "confirmed narrative write"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing narrative boundary %q", path, required)
			}
		}
		actor := "codex"
		if strings.Contains(filepath.ToSlash(path), "/claude/") {
			actor = "Claude Code"
		} else if strings.Contains(filepath.ToSlash(path), "/opencode/") {
			actor = "opencode"
		}
		for _, kind := range []string{"state", "arch"} {
			write := "memdolt " + kind + " set --dir <repository> --actor \"" + actor + "\" --json"
			at := strings.Index(text, write)
			if at < 0 || at > strings.Index(text, "7. Call `render`") {
				t.Errorf("%s must write %s with its host actor before render", path, kind)
			}
		}
	}

	openCodeWrapUp, err := os.ReadFile(repoFile("templates", "skills", "opencode", "memdolt-wrap-up", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"OpenCode host context",
		"memdolt opencode session-info <current-session-id> --json",
		"opencode2 api get \"/api/session/<current-session-id>\"",
		"`data.id`", "session_id", "agent_id", "provider_id", "model_id", "variant",
		"canonical `agent:opencode`", "raw `cli`",
		"cannot authenticate", "origin of that ID", "Never discover or guess another session",
		"Make the first durable write the approved session summary",
	} {
		if !strings.Contains(string(openCodeWrapUp), required) {
			t.Errorf("OpenCode wrap-up is missing %q", required)
		}
	}
	text := string(openCodeWrapUp)
	if strings.Index(text, "memdolt opencode wrap-up-note") > strings.Index(text, "memdolt state set") {
		t.Fatal("OpenCode narrative update precedes the first verified summary write")
	}
}

func TestInstalledOnboardingAndCatchUpResourcesResolveOutsideCheckout(t *testing.T) {
	source, err := filepath.Abs(repoFile("templates", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	shared := map[string][]byte{}
	for _, name := range []string{"onboarding", "catch-up"} {
		shared[name], err = os.ReadFile(filepath.Join(source, "memdolt-resources", name+".md"))
		if err != nil {
			t.Fatal(err)
		}
	}
	readme, err := os.ReadFile(repoFile("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"claude", "codex", "opencode"} {
		t.Run(host, func(t *testing.T) {
			// Match README's copy layout beside existing generic memhub skills.
			installed := t.TempDir()
			discovery, suffix := "skills", "/SKILL.md"
			destination := "~/." + host
			switch host {
			case "claude":
				discovery, suffix = "commands", ".md"
			case "opencode":
				destination = "~/.config/opencode"
			}
			for _, path := range []string{destination + "/" + discovery + "/", destination + "/memdolt-resources/"} {
				if !strings.Contains(string(readme), path) {
					t.Errorf("README does not document installed path %s", path)
				}
			}
			const existing = "existing memhub workflow\n"
			var genericPaths []string
			for _, name := range []string{"recall", "catch-up"} {
				path := filepath.Join(installed, discovery, filepath.FromSlash(name+suffix))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
					t.Fatal(err)
				}
				genericPaths = append(genericPaths, path)
			}
			if err := os.CopyFS(filepath.Join(installed, discovery), os.DirFS(filepath.Join(source, host))); err != nil {
				t.Fatal(err)
			}
			if err := os.CopyFS(filepath.Join(installed, "memdolt-resources"), os.DirFS(filepath.Join(source, "memdolt-resources"))); err != nil {
				t.Fatal(err)
			}
			// The target project has no templates/ and is not the source checkout.
			t.Chdir(t.TempDir())
			for _, link := range []struct{ from, label, resource string }{
				{discovery + "/memdolt-init-project" + suffix, "shared onboarding procedure", "onboarding"},
				{discovery + "/memdolt-catch-up" + suffix, "shared catch-up procedure", "catch-up"},
				{"memdolt-resources/onboarding.md", "shared catch-up procedure", "catch-up"},
			} {
				entryPath := filepath.Join(installed, filepath.FromSlash(link.from))
				body, err := os.ReadFile(entryPath)
				if err != nil {
					t.Fatal(err)
				}
				match := regexp.MustCompile(`\[` + regexp.QuoteMeta(link.label) + `\]\(([^)]+)\)`).FindSubmatch(body)
				if len(match) != 2 {
					t.Fatalf("installed %s lacks its %s link", link.from, link.label)
				}
				resolved := filepath.Join(filepath.Dir(entryPath), filepath.FromSlash(string(match[1])))
				if want := filepath.Join(installed, "memdolt-resources", link.resource+".md"); resolved != want {
					t.Fatalf("installed link resolved to %s, want %s", resolved, want)
				}
				got, err := os.ReadFile(resolved)
				if err != nil || string(got) != string(shared[link.resource]) {
					t.Fatalf("installed resource differs or cannot be read: %v", err)
				}
			}
			for _, path := range genericPaths {
				if got, err := os.ReadFile(path); err != nil || string(got) != existing {
					t.Fatalf("copy changed existing memhub workflow: %q, %v", got, err)
				}
			}
		})
	}
}

func repoFile(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func readRepoJSON(t *testing.T, name string, target any) {
	t.Helper()
	raw, err := os.ReadFile(repoFile(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
}

func sortedRawKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func flatSkillNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			names = append(names, strings.TrimSuffix(entry.Name(), ".md"))
		}
	}
	sort.Strings(names)
	return names
}

func directorySkillNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), "SKILL.md")); err == nil {
				names = append(names, entry.Name())
			}
		}
	}
	sort.Strings(names)
	return names
}

func allSkillFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(repoFile("templates", "skills"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "memdolt-resources" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && (entry.Name() == "SKILL.md" || filepath.Ext(entry.Name()) == ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk skill templates: %v", err)
	}
	return files
}
