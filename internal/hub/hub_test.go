package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/servercfg"
	"github.com/dolthub/dolt/go/libraries/events"
)

func fixtureConfig() Config {
	c := DefaultConfig()
	c.IPv4, c.IPv6 = "100.90.0.1", "fd7a:115c:a1e0::1"
	return c
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	// macOS /var is an alias; fixtures use its actual root, as renderer tests do.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHubArtifactsAndNativeYAML(t *testing.T) {
	c := fixtureConfig()
	files, err := c.artifacts()
	if err != nil {
		t.Fatal(err)
	}
	native, err := servercfg.NewYamlConfig(files["config.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	if err := servercfg.ValidateConfig(native); err != nil {
		t.Fatal(err)
	}
	if native.Host() != c.IPv4 || native.Port() != c.SQLPort || *native.RemotesapiPort() != c.RemotesPort ||
		*native.RemotesapiReadOnly() || native.DataDir() != c.DataDir || native.PrivilegeFilePath() != c.DataDir+"/.doltcfg/privileges.db" {
		t.Fatalf("unexpected native YAML: %s", files["config.yaml"])
	}
	unit := string(files[serverUnit])
	for _, text := range []string{
		"User=memdolt-hub", "Group=memdolt-hub", "After=network-online.target memdolt-hub-boundary.service tailscaled.service",
		"BindsTo=memdolt-hub-boundary.service tailscaled.service", "Restart=on-failure", "StartLimitBurst=3",
		"ReadWritePaths=/srv/memdolt-hub", "CapabilityBoundingSet=\n", "TimeoutStartSec=50",
	} {
		if !strings.Contains(unit, text) {
			t.Errorf("server unit lacks %q", text)
		}
	}
	preflight, ready, start := strings.Index(unit, "ExecStartPre=+"), strings.Index(unit, " hub ready "), strings.Index(unit, "ExecStart=")
	if preflight < 0 || ready < preflight || start < ready || strings.Contains(unit, "--files-only") {
		t.Fatalf("unsafe startup ordering: %s", unit)
	}
	boundary := string(files[boundaryUnit])
	if strings.Index(boundary, "--files-only") > strings.Index(boundary, "ExecStart=") ||
		!strings.Contains(boundary, "ExecStartPost=/usr/local/bin/memdolt hub preflight --config /etc/memdolt-hub/hub.json --json") ||
		strings.Contains(boundary, "ExecStop=") || strings.Contains(boundary, "nftables.service") {
		t.Fatalf("unsafe boundary lifecycle: %s", boundary)
	}
	for _, text := range []string{"create table inet memdolt_hub", "hook input priority -10", "ip6 saddr", "tcp dport { 3306, 50051 } drop"} {
		if !bytes.Contains(files["private.nft"], []byte(text)) {
			t.Errorf("firewall lacks %q", text)
		}
	}
	for name, data := range files {
		if name != "SETUP.md" && (bytes.Contains(data, []byte("password:")) || bytes.Contains(data, []byte("DOLT_ROOT_PASSWORD="))) {
			t.Fatalf("credential setting in %s", name)
		}
	}
}

func TestHubHostileConfigRefusesBeforeOutput(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"shell path":          func(c *Config) { c.DoltPath = "/opt/dolt;id" },
		"unit injection":      func(c *Config) { c.User = "user\nExecStart=/bin/sh" },
		"unit expansion":      func(c *Config) { c.NetworkService = "%n.service" },
		"path expansion":      func(c *Config) { c.DataDir = "/srv/$HOME" },
		"path traversal":      func(c *Config) { c.ConfigDir = "/etc/../srv" },
		"store collision":     func(c *Config) { c.DataDir = "/srv/project/.memdolt/dolt" },
		"overlap":             func(c *Config) { c.DataDir = c.ConfigDir + "/data" },
		"root":                func(c *Config) { c.User = "root" },
		"loopback":            func(c *Config) { c.Interface = "lo" },
		"interface injection": func(c *Config) { c.Interface = "tailscale0\" accept" },
		"ipv4 family":         func(c *Config) { c.IPv4 = c.IPv6; c.AllowIPv4 = c.AllowIPv6 },
		"ipv6 family":         func(c *Config) { c.IPv6 = c.IPv4; c.AllowIPv6 = c.AllowIPv4 },
		"ipv4 mapped":         func(c *Config) { c.IPv6 = "::ffff:100.90.0.1" },
		"public":              func(c *Config) { c.IPv4 = "8.8.8.8"; c.AllowIPv4 = "8.0.0.0/8" },
		"all addresses":       func(c *Config) { c.AllowIPv4 = "0.0.0.0/0" },
		"outside prefix":      func(c *Config) { c.AllowIPv4 = "10.0.0.0/8" },
		"noncanonical":        func(c *Config) { c.AllowIPv4 = "100.90.0.1/10" },
		"duplicate ports":     func(c *Config) { c.SQLPort = c.RemotesPort },
		"privileged port":     func(c *Config) { c.SQLPort = 22 },
		"unbounded retry":     func(c *Config) { c.ReadySeconds = 3600 },
		"format":              func(c *Config) { c.Format = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			c := fixtureConfig()
			change(&c)
			output := filepath.Join(fixtureDir(t), "bundle")
			if result, err := Init(output, c); err == nil || result.Status != "failed" {
				t.Fatalf("hostile config accepted: %+v %v", result, err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("refused config created output: %v", err)
			}
		})
	}
}

func TestHubBundlePreservation(t *testing.T) {
	c := fixtureConfig()
	output := filepath.Join(fixtureDir(t), "bundle")
	first, err := Init(output, c)
	if err != nil || first.Status != "written" || len(first.Files) != 6 {
		t.Fatalf("init: %+v %v", first, err)
	}
	if got, err := load(filepath.Join(output, manifestName), false); err != nil || got != c {
		t.Fatalf("load: %+v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(output, "private.nft"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Init(output, c); err != nil || again.Status != "unchanged" || len(again.Files) != 0 {
		t.Fatalf("idempotent init: %+v %v", again, err)
	}
	after, err := os.Stat(filepath.Join(output, "private.nft"))
	if err != nil || !os.SameFile(info, after) || info.ModTime() != after.ModTime() {
		t.Fatal("identical file was replaced")
	}
	for _, target := range []string{"private.nft", "foreign.txt", manifestName} {
		t.Run(target, func(t *testing.T) {
			out := filepath.Join(fixtureDir(t), "bundle")
			if _, err := Init(out, c); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(out, target)
			if err := os.WriteFile(p, []byte("existing private user content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Init(out, c); err == nil {
				t.Fatal("modified or foreign target accepted")
			}
			if raw, err := os.ReadFile(p); err != nil || string(raw) != "existing private user content" {
				t.Fatal("modified file lost")
			}
			if _, err := load(filepath.Join(out, manifestName), false); err == nil {
				t.Fatal("modified bundle passed inspection")
			}
		})
	}
	for _, target := range []string{"relative", filepath.Join(fixtureDir(t), ".git", "bundle"), filepath.Join(fixtureDir(t), "missing", "bundle")} {
		if _, err := Init(target, c); err == nil {
			t.Fatalf("unsafe output %s accepted", target)
		}
	}
}

func TestHubBundleLinkRefusal(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(map[bool]string{false: "symbolic", true: "hard"}[hard], func(t *testing.T) {
			dir := fixtureDir(t)
			out := filepath.Join(dir, "bundle")
			if _, err := Init(out, fixtureConfig()); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(out, "private.nft")
			foreign := filepath.Join(dir, "foreign")
			if err := os.Rename(original, foreign); err != nil {
				t.Fatal(err)
			}
			link := os.Symlink
			if hard {
				link = os.Link
			}
			if err := link(foreign, original); err != nil {
				t.Skipf("fixture links unavailable: %v", err)
			}
			if _, err := Init(out, fixtureConfig()); err == nil {
				t.Fatal("linked artifact accepted")
			}
			if _, err := load(filepath.Join(out, manifestName), false); err == nil {
				t.Fatal("linked artifact passed inspection")
			}
			if !hard {
				alias := filepath.Join(dir, "alias")
				if err := os.Symlink(out, alias); err != nil {
					t.Fatal(err)
				}
				if _, err := Init(filepath.Join(alias, "new"), fixtureConfig()); err == nil {
					t.Fatal("linked ancestor accepted")
				}
			}
		})
	}
}

func TestAppliedBoundaryRefusesWeakenedRules(t *testing.T) {
	c := fixtureConfig()
	raw, err := json.Marshal(map[string]any{"nftables": expectedNFT(c)})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNFT(raw, c); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ old, replacement string }{
		{`"drop":null`, `"accept":null`},
		{`"hook":"input"`, `"hook":"forward"`},
		{`"prio":-10`, `"prio":0`},
		{`"family":"inet"`, `"family":"ip"`},
		{`"name":"memdolt_hub"`, `"name":"memdolt_hub","flags":["dormant"]`},
		{`"100.90.0.1"`, `"100.90.0.9"`},
		{`"tailscale0"`, `"eth0"`},
		{`3306,50051`, `3306,60000`},
		{`"len":10`, `"len":0`},
		{`"protocol":"ip6"`, `"protocol":"ip"`},
	} {
		changed := bytes.ReplaceAll(raw, []byte(change.old), []byte(change.replacement))
		if bytes.Equal(raw, changed) {
			t.Fatalf("ineffective mutation %s", change.old)
		}
		if err := checkNFT(changed, c); err == nil {
			t.Errorf("weakened rule accepted: %s", change.replacement)
		}
	}
	for _, missing := range []string{`{}`, `{"nftables":[]}`, `{"nftables":null}`, "permission denied", string(raw[:len(raw)-1])} {
		if err := checkNFT([]byte(missing), c); err == nil {
			t.Fatal("missing/malformed protection accepted")
		}
	}
}

func TestVersionAndBoundedReadiness(t *testing.T) {
	for input, want := range map[string]string{"dolt version 1.88.1\n": Version, "dolt version 1.88.2": "1.88.2", "7.18": "", "dolt version secret-password": ""} {
		got, err := doltVersion([]byte(input))
		if got != want || (err == nil) != (want == Version) || (err != nil && strings.Contains(err.Error(), "secret-password")) {
			t.Fatalf("version %q: %s %v", input, got, err)
		}
	}
	c := fixtureConfig()
	c.ReadySeconds = 1
	calls := 0
	if err := waitReady(context.Background(), c, func(Config) error {
		calls++
		if calls == 1 {
			return errors.New("boot race")
		}
		return nil
	}); err != nil || calls != 2 {
		t.Fatalf("readiness retry: %d %v", calls, err)
	}
	start := time.Now()
	if err := waitReady(context.Background(), c, func(Config) error { return errors.New("still absent") }); err == nil || !strings.Contains(err.Error(), "still absent") || time.Since(start) > 3*time.Second {
		t.Fatalf("readiness did not visibly bound failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitReady(ctx, c, func(Config) error { return errors.New("absent") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("readiness ignored cancellation: %v", err)
	}
}

func TestHubProbeOutputBound(t *testing.T) {
	var out boundedOutput
	// A Reader-only source makes io.Copy test the destination's optimized
	// ReaderFrom path too: embedding bytes.Buffer would bypass the Write bound.
	_, err := io.Copy(&out, io.LimitReader(strings.NewReader(strings.Repeat("x", 1<<20+1)), 1<<20+1))
	if err == nil || out.buf.Len() > 1<<20 {
		t.Fatalf("probe output exceeded its bound: %d %v", out.buf.Len(), err)
	}
}

func TestHubNativeProbeArtifacts(t *testing.T) {
	// The pinned CLI constructs this emitter before swapping in NullEmitter.
	// Exercise the actual constructor to bind probe cleanup to its real files.
	dir := fixtureDir(t)
	_ = events.NewFileEmitter(dir, ".dolt")
	entries, err := os.ReadDir(filepath.Join(dir, ".dolt", "eventsData"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "dolt.lock" {
		t.Fatalf("native emitter artifacts changed: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".dolt", "eventsData", "dolt.lock"))
	if err != nil || string(data) != "lockfile for dolt \n" {
		t.Fatalf("native emitter lock changed: %q %v", data, err)
	}
}

func TestUnsupportedHubPlatformReportsUnknownWithoutExecution(t *testing.T) {
	out := filepath.Join(fixtureDir(t), "bundle")
	if _, err := Init(out, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"status", "preflight", "ready"} {
		report, err := inspect(context.Background(), filepath.Join(out, manifestName), mode, false, "windows",
			func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("executed on unsupported platform")
				return nil, nil
			},
			func(Config) error { t.Fatal("inspected addresses on unsupported platform"); return nil },
			func(context.Context, string) error { t.Fatal("contacted listener on unsupported platform"); return nil })
		if err == nil || report.OK || report.Version != "" || len(report.Checks) != 7 || report.Checks[2].Status != "unknown" {
			t.Fatalf("unsupported report: %+v %v", report, err)
		}
	}
}
