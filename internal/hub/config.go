// Package hub generates and checks a native Linux Dolt deployment. It never
// opens a local memory store, installs services, or applies firewall rules.
package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
)

const Version = "1.88.1"

const (
	manifestName = "hub.json"
	tableName    = "memdolt_hub"
	serverUnit   = "memdolt-hub.service"
	boundaryUnit = "memdolt-hub-boundary.service"
)

// Config contains only nonsecret deployment settings, independent of repository
// topology/config.toml. All interpolated values pass Validate first.
type Config struct {
	Format         int    `json:"format"`
	DoltPath       string `json:"dolt_path"`
	MemdoltPath    string `json:"memdolt_path"`
	NFTPath        string `json:"nft_path"`
	ConfigDir      string `json:"config_dir"`
	DataDir        string `json:"data_dir"`
	User           string `json:"user"`
	NetworkService string `json:"network_service"`
	Interface      string `json:"interface"`
	IPv4           string `json:"ipv4"`
	IPv6           string `json:"ipv6"`
	AllowIPv4      string `json:"allow_ipv4"`
	AllowIPv6      string `json:"allow_ipv6"`
	SQLPort        int    `json:"sql_port"`
	RemotesPort    int    `json:"remotes_port"`
	ReadySeconds   int    `json:"ready_seconds"`
}

func DefaultConfig() Config {
	return Config{Format: 1, DoltPath: "/usr/local/bin/dolt", MemdoltPath: "/usr/local/bin/memdolt",
		NFTPath: "/usr/sbin/nft", ConfigDir: "/etc/memdolt-hub", DataDir: "/srv/memdolt-hub",
		User: "memdolt-hub", NetworkService: "tailscaled.service", Interface: "tailscale0",
		AllowIPv4: "100.64.0.0/10", AllowIPv6: "fd7a:115c:a1e0::/48",
		SQLPort: 3306, RemotesPort: 50051, ReadySeconds: 30}
}

var (
	linuxPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	userName  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
	service   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}\.service$`)
	ifaceName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)
)

func (c Config) Validate() error {
	if c.Format != 1 {
		return errors.New("unsupported hub configuration format (require 1)")
	}
	paths := []string{c.DoltPath, c.MemdoltPath, c.NFTPath, c.ConfigDir, c.DataDir}
	for i, p := range paths {
		if !linuxPath.MatchString(p) || p == "/" || path.Clean(p) != p {
			return errors.New("hub paths must be canonical absolute Linux paths using only letters, digits, underscore, dot, slash and hyphen")
		}
		for _, part := range strings.Split(p, "/") {
			switch strings.ToLower(part) {
			case ".git", ".memdolt", ".memhub", ".orchestrator":
				return errors.New("hub deployment paths collide with protected repository metadata")
			}
		}
		for _, other := range paths[:i] {
			if p == other || strings.HasPrefix(p, other+"/") || strings.HasPrefix(other, p+"/") {
				return errors.New("hub binary, configuration and data destinations must be distinct and nonoverlapping")
			}
		}
	}
	if !userName.MatchString(c.User) || c.User == "root" || c.User == "nobody" {
		return errors.New("hub user must name a dedicated unprivileged account")
	}
	if !service.MatchString(c.NetworkService) || c.NetworkService == serverUnit || c.NetworkService == boundaryUnit {
		return errors.New("hub network service must name a separate, non-templated systemd .service unit")
	}
	if !ifaceName.MatchString(c.Interface) || c.Interface == "lo" {
		return errors.New("hub interface must name a private Linux network interface (not lo)")
	}
	for i, pair := range []struct{ address, prefix string }{{c.IPv4, c.AllowIPv4}, {c.IPv6, c.AllowIPv6}} {
		addr, err := netip.ParseAddr(pair.address)
		if err != nil || addr.String() != pair.address || addr.Zone() != "" || addr.Is4In6() || addr.Is4() != (i == 0) {
			return errors.New("hub addresses must be canonical private IPv4 and IPv6 literals without zones")
		}
		prefix, err := netip.ParsePrefix(pair.prefix)
		if err != nil || prefix.Masked().String() != pair.prefix || !prefix.Contains(addr) {
			return errors.New("hub allow prefixes must be canonical networks containing their private addresses")
		}
		private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
		if i == 1 {
			private = []string{"fc00::/7"}
		}
		allowed := false
		for _, block := range private {
			p := netip.MustParsePrefix(block)
			allowed = allowed || p.Contains(prefix.Addr()) && p.Bits() <= prefix.Bits()
		}
		if !allowed {
			return errors.New("hub IPv4 must use RFC1918/CGNAT and IPv6 must use ULA; public or all-address allow prefixes are refused")
		}
	}
	if c.SQLPort < 1024 || c.SQLPort > 65535 || c.RemotesPort < 1024 || c.RemotesPort > 65535 || c.SQLPort == c.RemotesPort {
		return errors.New("hub SQL and remotes ports must be distinct unprivileged ports (1024..65535)")
	}
	if c.ReadySeconds < 1 || c.ReadySeconds > 120 {
		return errors.New("hub readiness timeout must be 1..120 seconds")
	}
	return nil
}

func (c Config) artifacts() (map[string][]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	manifest, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{manifestName: append(manifest, '\n')}
	files["config.yaml"] = fmt.Appendf(nil, `# Generated by memdolt hub init; native Dolt %s only. No credentials here.
log_level: info
data_dir: %s
cfg_dir: %s/.doltcfg
privilege_file: %s/.doltcfg/privileges.db
listener:
  host: "%s"
  port: %d
  socket: %s/dolt.sock
remotesapi:
  port: %d
  read_only: false
`, Version, c.DataDir, c.DataDir, c.DataDir, c.IPv4, c.SQLPort, c.DataDir, c.RemotesPort)
	files["private.nft"] = fmt.Appendf(nil, `# Own only this table. Existing tables refuse atomically; never flush ruleset.
create table inet %s
add chain inet %s ingress { type filter hook input priority -10; policy accept; }
add rule inet %s ingress iifname "lo" tcp dport { %d, %d } accept
add rule inet %s ingress iifname "%s" ip saddr %s ip daddr %s tcp dport { %d, %d } accept
add rule inet %s ingress iifname "%s" ip6 saddr %s ip6 daddr %s tcp dport { %d, %d } accept
add rule inet %s ingress tcp dport { %d, %d } drop
`, tableName, tableName, tableName, c.SQLPort, c.RemotesPort,
		tableName, c.Interface, c.AllowIPv4, c.IPv4, c.SQLPort, c.RemotesPort,
		tableName, c.Interface, c.AllowIPv6, c.IPv6, c.SQLPort, c.RemotesPort,
		tableName, c.SQLPort, c.RemotesPort)
	files[boundaryUnit] = fmt.Appendf(nil, `[Unit]
Description=Private ingress boundary for the native Dolt hub
Before=%s

[Service]
Type=oneshot
RemainAfterExit=yes
User=root
ExecStartPre=%s hub preflight --config %s/hub.json --files-only --json
ExecStart=%s -f %s/private.nft
ExecStartPost=%s hub preflight --config %s/hub.json --json
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
`, serverUnit, c.MemdoltPath, c.ConfigDir, c.NFTPath, c.ConfigDir, c.MemdoltPath, c.ConfigDir)
	files[serverUnit] = fmt.Appendf(nil, `[Unit]
Description=Native Dolt %s memory hub
Wants=network-online.target
BindsTo=%s %s
After=network-online.target %s %s
StartLimitIntervalSec=300
StartLimitBurst=3

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
Environment=HOME=%s
Environment=DOLT_DISABLE_EVENT_FLUSH=1
# Only this read-only applied-firewall check is privileged.
ExecStartPre=+%s hub preflight --config %s/hub.json --json
ExecStartPre=%s hub ready --config %s/hub.json --json
ExecStart=%s sql-server --config %s/config.yaml
TimeoutStartSec=%d
Restart=on-failure
RestartSec=10
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=%s
CapabilityBoundingSet=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK

[Install]
WantedBy=multi-user.target
`, Version, boundaryUnit, c.NetworkService, boundaryUnit, c.NetworkService,
		c.User, c.User, c.DataDir, c.DataDir, c.MemdoltPath, c.ConfigDir,
		c.MemdoltPath, c.ConfigDir, c.DoltPath, c.ConfigDir, c.ReadySeconds+20, c.DataDir)
	files["SETUP.md"] = fmt.Appendf(nil, `# Review before installation

Generated for native Dolt %s on Linux with systemd and nftables. Generation
does not install anything. See docs/hub-deployment.md for credentials, checksum
verification, recovery and the remaining physical acceptance gate.

Review every file. Install verified binaries at %s and %s; nft must be at %s.
The binaries and their parent directories must be root-owned and not group/world
writable. Do not use symlinks. Create the dedicated account and an empty data
directory only after inspecting those destinations:

    sudo useradd --system --user-group --home-dir %s --shell /usr/sbin/nologin %s
    sudo install -d -o %s -g %s -m 0700 %s

Copy this complete directory to %s as root-owned files (0644) in a root-owned
directory (0755). Do not replace an existing deployment; inspect it first.
Install the two .service files into /etc/systemd/system, also root-owned 0644.
Enable/start the private-network service %s and confirm %s owns both %s and %s.

Before managed startup, bootstrap native authentication using the runbook's
loopback-only server WITHOUT remotesapi. Persist users/grants in
%s/.doltcfg/privileges.db, then stop that bootstrap process. Never put a password
in this bundle, command arguments, URLs, shell history or diagnostics. SQL users
are independent of the OS account. Create database memory (or seed an existing
ordinary Dolt database with its history). Example NONSECRET grants after creating
password-protected users interactively with client history disabled:

    CREATE DATABASE memory;
    GRANT SELECT ON memory.* TO 'memory_reader'@'%%';
    GRANT CLONE_ADMIN ON *.* TO 'memory_reader'@'%%';
    GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, INDEX ON memory.* TO 'memory_writer'@'%%';
    GRANT CLONE_ADMIN, SUPER ON *.* TO 'memory_writer'@'%%';

Native %s remotes reads require global CLONE_ADMIN and writes require SUPER.
These are broad server privileges, NOT database-scoped remote isolation. Use a
separate server/trust group if that scope is unacceptable. Do not grant writers
GRANT OPTION. SQL-only readers need no CLONE_ADMIN if they never clone/fetch.

Review then explicitly install the boundary and start the server:

    sudo systemd-analyze verify /etc/systemd/system/memdolt-hub-boundary.service /etc/systemd/system/memdolt-hub.service
    sudo systemctl daemon-reload
    sudo systemctl enable --now memdolt-hub.service
    sudo %s hub status --config %s/hub.json --json

The dedicated boundary service atomically CREATES only inet memdolt_hub. An
existing table refuses; it never flushes or replaces any table. Do not enable
the distribution nftables.service with a flush-ruleset configuration. Preserve
this table across other firewall reloads. Stopping the boundary service stops
Dolt through BindsTo but deliberately leaves protection in place. After stopping
Dolt, inspect a leftover table before manually removing only inet memdolt_hub
and restarting the boundary. The guard is an observed startup check, not a live
monitor of other privileged firewall writers. Never remove protection while
Dolt runs. Probe denied and permitted IPv4/IPv6 traffic after every change.
`, Version, c.DoltPath, c.MemdoltPath, c.NFTPath, c.DataDir, c.User, c.User, c.User,
		c.DataDir, c.ConfigDir, c.NetworkService, c.Interface, c.IPv4, c.IPv6, c.DataDir,
		Version, c.MemdoltPath, c.ConfigDir)
	return files, nil
}
