package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if got := sortedRawKeys(openCode.Commands); !reflect.DeepEqual(got, []string{"check-init", "recall", "wrap-up"}) {
		t.Fatalf("OpenCode commands = %q, want active core skills only", got)
	}
	if !reflect.DeepEqual(openCode.Skills, []string{"templates/skills/opencode"}) {
		t.Fatalf("OpenCode skill roots = %q", openCode.Skills)
	}
}

func TestCoreSkillTemplatesMatchAcrossHostsAndStayInsideM3(t *testing.T) {
	want := []string{"check-init", "recall", "wrap-up"}
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
		for _, deferredTool := range []string{
			"`locate`", "`doc_add`", "`render`", "`repo_status`", "`repo_pull`",
			"`repo_push`", "`history`", "`archive_transcript`",
		} {
			if strings.Contains(text, deferredTool) {
				t.Errorf("%s invokes deferred tool %s", path, deferredTool)
			}
		}
	}

	for _, path := range []string{
		repoFile("templates", "skills", "claude", "wrap-up.md"),
		repoFile("templates", "skills", "codex", "wrap-up", "SKILL.md"),
		repoFile("templates", "skills", "opencode", "wrap-up", "SKILL.md"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		for _, required := range []string{"explicit per-item approval", "propose_fact", "propose_decision", "Never promote"} {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing proposal/review boundary phrase %q", path, required)
			}
		}
	}

	openCodeWrapUp, err := os.ReadFile(repoFile("templates", "skills", "opencode", "wrap-up", "SKILL.md"))
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
	} {
		if !strings.Contains(string(openCodeWrapUp), required) {
			t.Errorf("OpenCode wrap-up is missing %q", required)
		}
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
