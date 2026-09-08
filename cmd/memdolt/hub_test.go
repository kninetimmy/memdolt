package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/hub"
)

func TestHubCLIArtifactLifecycle(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle")
	args := []string{"hub", "init", "--output", out, "--ipv4", "100.90.0.1", "--ipv6", "fd7a:115c:a1e0::1", "--json"}
	first := decodeJSON[hub.InitResult](t, runMemdolt(t, args...))
	if first.Status != "written" || len(first.Files) != 6 {
		t.Fatalf("init result: %+v", first)
	}
	second := decodeJSON[hub.InitResult](t, runMemdolt(t, args...))
	if second.Status != "unchanged" {
		t.Fatalf("repeat init: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(dir, ".memdolt")); !os.IsNotExist(err) {
		t.Fatalf("hub init touched local memory: %v", err)
	}
	text, err := runMemdoltResult(t, "hub", "status", "--config", filepath.Join(out, "hub.json"), "--json")
	var report hub.Report
	if err == nil || json.Unmarshal([]byte(text), &report) != nil || report.OK || len(report.Checks) == 0 {
		t.Fatalf("undeployed status claimed success or lacked JSON: %s %v", text, err)
	}
	text, err = runMemdoltResult(t, "hub", "init", "--output", out, "--ipv4", "8.8.8.8", "--ipv6", "fd7a:115c:a1e0::1", "--json")
	var failed hub.InitResult
	if err == nil || json.Unmarshal([]byte(text), &failed) != nil || failed.Status != "failed" || failed.Error == "" {
		t.Fatalf("refused init missing structured error: %s %v", text, err)
	}
	if help := runMemdolt(t, "hub", "init", "--help"); !strings.Contains(help, "--output") || !strings.Contains(help, "--network-service") {
		t.Fatal("hub help omits explicit destinations/network dependency")
	}
}
