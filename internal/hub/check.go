package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Report struct {
	OK       bool    `json:"ok"`
	Config   string  `json:"config"`
	Platform string  `json:"platform"`
	Version  string  `json:"observed_dolt_version,omitempty"`
	Checks   []Check `json:"checks"`
}

func (r *Report) add(name string, err error, detail string) {
	c := Check{Name: name, Status: "ok", Detail: detail}
	if err != nil {
		r.OK, c.Status, c.Detail = false, "fail", err.Error()
	}
	r.Checks = append(r.Checks, c)
}

// Inspect checks only the selected deployment. preflight never runs Dolt;
// ready runs its version probe as the service user before waiting for addresses.
// status is an observation, not a promise of authentication or remote reachability.
func Inspect(ctx context.Context, config, mode string, filesOnly bool) (Report, error) {
	return inspect(ctx, config, mode, filesOnly, runtime.GOOS, commandOutput, privateReady, listening)
}

func inspect(ctx context.Context, config, mode string, filesOnly bool, platform string,
	run func(context.Context, string, ...string) ([]byte, error),
	addresses func(Config) error, listen func(context.Context, string) error) (Report, error) {
	report := Report{OK: true, Config: config, Platform: platform, Checks: []Check{}}
	if mode != "status" && mode != "preflight" && mode != "ready" {
		return report, errors.New("unknown hub check mode")
	}
	cfg, err := load(config, platform == "linux")
	report.add("configuration", err, "complete generated bundle matches")
	if err != nil {
		return report, err
	}
	if platform != "linux" {
		err := errors.New("unsupported platform: managed hub startup and live inspection require Linux")
		report.add("platform", err, "")
		for _, name := range []string{"version", "private-addresses", "private-boundary", "sql-listener", "remotes-listener"} {
			report.Checks = append(report.Checks, Check{Name: name, Status: "unknown", Detail: "not observed on this platform"})
		}
		return report, err
	}
	if mode != "ready" {
		err = executable(cfg.NFTPath)
		report.add("nft-executable", err, "root-owned regular executable")
		if err == nil && !filesOnly {
			var raw []byte
			raw, err = run(ctx, cfg.NFTPath, "--json", "--numeric", "list", "table", "inet", tableName)
			if err == nil {
				err = checkNFT(raw, cfg)
			}
			report.add("private-boundary", err, "applied IPv4/IPv6 input protection matches; loopback and configured private ingress only")
		}
		if mode == "preflight" {
			return finish(report)
		}
	}
	if mode == "ready" {
		err = serviceUser(cfg.User)
		report.add("service-user", err, "effective UID/GID match the configured unprivileged account/group")
		if err != nil {
			return finish(report)
		}
	}
	err = executable(cfg.DoltPath)
	if err == nil {
		var raw []byte
		raw, err = run(ctx, cfg.DoltPath, "version")
		if err == nil {
			report.Version, err = doltVersion(raw)
		}
	}
	report.add("version", err, "native release "+Version+"; remotes wire-format metadata is not a release version")
	if mode == "ready" && err != nil {
		return finish(report)
	}
	// Require deliberate native account bootstrap before exposing remotesapi.
	// This proves a private regular privilege file exists, not the grants inside.
	err = privileges(cfg)
	report.add("native-privileges", err, "native privilege file exists; credential/grant correctness requires operator SQL verification")
	if mode == "ready" && err != nil {
		return finish(report)
	}
	if mode == "ready" {
		err = waitReady(ctx, cfg, addresses)
	} else {
		err = addresses(cfg)
	}
	report.add("private-addresses", err, "both configured addresses are assigned to the private interface")
	if mode == "status" {
		for _, target := range []struct{ name, addr string }{
			{"sql-listener", net.JoinHostPort(cfg.IPv4, strconv.Itoa(cfg.SQLPort))},
			{"remotes-listener", net.JoinHostPort(cfg.IPv4, strconv.Itoa(cfg.RemotesPort))},
			{"remotes-ipv6-listener", net.JoinHostPort(cfg.IPv6, strconv.Itoa(cfg.RemotesPort))},
		} {
			err = listen(ctx, target.addr)
			report.add(target.name, err, "TCP connection observed at "+target.addr+"; process identity/authentication not established")
		}
	}
	return finish(report)
}

func finish(report Report) (Report, error) {
	if !report.OK {
		return report, errors.New("hub checks failed; inspect the reported checks before startup")
	}
	return report, nil
}

var releaseLine = regexp.MustCompile(`^dolt version ([0-9]+\.[0-9]+\.[0-9]+)$`)

func doltVersion(raw []byte) (string, error) {
	parts := releaseLine.FindStringSubmatch(strings.TrimSpace(string(raw)))
	if len(parts) != 2 {
		return "", errors.New("unrecognized native Dolt version output (not printed)")
	}
	if parts[1] != Version {
		return parts[1], fmt.Errorf("unsupported native Dolt release %s; measured baseline is exactly %s", parts[1], Version)
	}
	return parts[1], nil
}

func executable(p string) (err error) {
	root, err := openDir(filepath.Dir(p), true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat(filepath.Base(p))
	if err != nil || !info.Mode().IsRegular() || unsafeLink(info) || !rootOwned(info) || info.Mode().Perm()&0o111 == 0 {
		return errors.Join(errors.New("deployment executable must be a root-owned, non-writable, unlinked executable file"), err)
	}
	return nil
}

func serviceUser(name string) error {
	if os.Geteuid() <= 0 || os.Getegid() <= 0 {
		return errors.New("hub ready must run as the configured unprivileged account/group, not root or a root-ID alias")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return errors.New("cannot resolve configured hub service account")
	}
	group, err := user.LookupGroup(name)
	if err != nil {
		return errors.New("cannot resolve configured hub service group")
	}
	if account.Uid != strconv.Itoa(os.Geteuid()) || group.Gid != strconv.Itoa(os.Getegid()) {
		return errors.New("effective UID/GID do not match the configured hub service account/group")
	}
	return nil
}

func privileges(c Config) (err error) {
	root, err := openDir(filepath.Join(c.DataDir, ".doltcfg"), false)
	if err != nil {
		return errors.New("native privileges are not bootstrapped; follow SETUP.md before starting remotesapi")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat("privileges.db")
	if err != nil || !info.Mode().IsRegular() || unsafeLink(info) || info.Mode().Perm()&0o077 != 0 || info.Size() == 0 {
		return errors.New("native privileges.db must be an existing nonempty private regular file")
	}
	f, err := root.Open("privileges.db")
	if err != nil {
		return errors.New("cannot open native privileges.db for identity verification")
	}
	actual, statErr := f.Stat()
	one, linkErr := singleLink(f)
	if err := errors.Join(statErr, linkErr, f.Close()); err != nil || !one || !os.SameFile(info, actual) {
		return errors.New("native privileges.db identity or single-link verification failed")
	}
	return nil
}

func commandOutput(ctx context.Context, binary string, args ...string) (_ []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "DOLT_DISABLE_EVENT_FLUSH=1"}
	if len(args) == 1 && args[0] == "version" {
		// Native VersionCmd otherwise queries GitHub and mixes update warnings
		// into stdout. Isolate its global config and cwd instead of weakening the
		// version grammar or reading/changing the operator's native configuration.
		dir, createErr := os.MkdirTemp("", "memdolt-hub-version-")
		if createErr != nil {
			return nil, createErr
		}
		defer func() { err = errors.Join(err, os.Remove(dir)) }()
		root, openErr := os.OpenRoot(dir)
		if openErr != nil {
			return nil, openErr
		}
		defer func() { err = errors.Join(err, root.Close()) }()
		if err := root.Mkdir("home", 0o700); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, root.Remove("home")) }()
		if err := root.Mkdir("home/.dolt", 0o700); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, root.Remove("home/.dolt")) }()
		if err := root.WriteFile("home/.dolt/config_global.json", []byte(`{"metrics.disabled":"true","versioncheck.disabled":"true"}`), 0o600); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, root.Remove("home/.dolt/config_global.json")) }()
		// The global .dolt must not also be cwd's repository metadata: native
		// startup otherwise treats that directory as a database to discover.
		if err := root.Mkdir("work", 0o700); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, root.Remove("work")) }()
		cmd.Dir = filepath.Join(dir, "work")
		cmd.Env = append(cmd.Env, "HOME="+filepath.Join(dir, "home"), "DOLT_ROOT_PATH="+filepath.Join(dir, "home"))
	}
	var out boundedOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s probe failed (output withheld): %w", filepath.Base(binary), err)
	}
	return out.buf.Bytes(), nil
}

type boundedOutput struct{ buf bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p)+b.buf.Len() > 1<<20 {
		return 0, errors.New("hub probe exceeded one MiB")
	}
	return b.buf.Write(p)
}

func privateReady(c Config) error {
	iface, err := net.InterfaceByName(c.Interface)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return errors.New("private interface is absent or down")
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return errors.New("cannot read private interface addresses")
	}
	v4, v6 := false, false
	for _, address := range addrs {
		p, err := netip.ParsePrefix(address.String())
		if err != nil {
			return errors.New("cannot parse private interface address")
		}
		v4 = v4 || p.Addr().String() == c.IPv4
		v6 = v6 || p.Addr().String() == c.IPv6
	}
	if !v4 || !v6 {
		return errors.New("configured IPv4 and IPv6 addresses are not both assigned to the private interface")
	}
	return nil
}

func waitReady(ctx context.Context, c Config, probe func(Config) error) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.ReadySeconds)*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := probe(c)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("private-address readiness failed within %ds: %w", c.ReadySeconds, errors.Join(ctx.Err(), err))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func listening(ctx context.Context, address string) error {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return errors.New("TCP listener not reachable from this host")
	}
	return conn.Close()
}
