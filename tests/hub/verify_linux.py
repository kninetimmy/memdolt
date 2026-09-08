"""Isolated Linux gate: generated units/config plus real dual-stack nft ingress.

Run only on a disposable Linux CI runner, as root. All nft operations execute in
new named network namespaces; no host firewall, account or service is changed.
The caller must verify the native Dolt release archive before invoking this rig.
"""

import argparse
import json
import os
from pathlib import Path
import pwd
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
import uuid


def run(args, *, ok=True, env=None):
    result = subprocess.run([str(a) for a in args], capture_output=True, text=True,
                            timeout=30, env=env)
    if ok and result.returncode:
        raise AssertionError(f"command failed ({result.returncode}): {args}; output withheld")
    return result


def require(condition, detail):
    if not condition:
        raise AssertionError(detail)


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)


LISTEN = """
import selectors, socket, sys
s = selectors.DefaultSelector()
for port in sys.argv[1:]:
    listener = socket.socket(socket.AF_INET6)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
    listener.bind(('::', int(port)))
    listener.listen(64)
    s.register(listener, selectors.EVENT_READ)
while True:
    for key, _ in s.select():
        conn, _ = key.fileobj.accept()
        conn.close()
"""

CONNECT = """
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), 0.5).close()
except OSError:
    sys.exit(1)
"""


def verify(root, source_memdolt, source_dolt, uid):
    account = pwd.getpwuid(uid)
    require(uid != 0 and account.pw_name not in ("root", "nobody"), "need an existing unprivileged CI account")
    bin_dir = root / "bin"
    bin_dir.mkdir(mode=0o755)
    memdolt, dolt = bin_dir / "memdolt", bin_dir / "dolt"
    for source, target in ((source_memdolt, memdolt), (source_dolt, dolt)):
        require(source.is_file(), "missing verified build/release executable")
        shutil.copyfile(source, target)
        target.chmod(0o755)
    data, bundle = root / "data", root / "config"
    data.mkdir(mode=0o700)
    os.chown(data, uid, account.pw_gid)
    nft = Path(shutil.which("nft")).resolve(strict=True)
    run([memdolt, "hub", "init", "--output", bundle, "--config-dir", bundle,
         "--data-dir", data, "--dolt", dolt, "--memdolt", memdolt, "--nft", nft,
         "--user", account.pw_name, "--network-service", "memdolt-test-private.service",
         "--interface", "private0", "--ipv4", "100.90.0.1", "--ipv6", "fd7a:115c:a1e0::1",
         "--ready-seconds", "1", "--json"])
    config = bundle / "hub.json"
    # Verify the actual generated systemd grammar, preserving production unit
    # bytes. Only the disposable network dependency is a local fixture unit.
    dependency = root / "memdolt-test-private.service"
    dependency.write_text("[Service]\nType=oneshot\nExecStart=/usr/bin/true\nRemainAfterExit=yes\n")
    unit_env = dict(os.environ, SYSTEMD_UNIT_PATH=f"{bundle}:{root}:/usr/lib/systemd/system:/lib/systemd/system")
    run(["systemd-analyze", "verify", "--man=no", bundle / "memdolt-hub.service",
         bundle / "memdolt-hub-boundary.service"], env=unit_env)
    unit = (bundle / "memdolt-hub.service").read_text().splitlines()
    pre = [shlex.split(line.split("=", 1)[1].lstrip("+")) for line in unit if line.startswith("ExecStartPre=")]
    start = [shlex.split(line.split("=", 1)[1]) for line in unit if line.startswith("ExecStart=")]
    require(len(pre) == 2 and len(start) == 1 and "preflight" in pre[0] and "ready" in pre[1], "unit startup order")
    suffix = uuid.uuid4().hex[:10]
    namespaces, processes = [], []
    hub_ns, private_ns, denied_ns = [f"mdh-{suffix}-{role}" for role in ("hub", "private", "denied")]

    def ns(name, *args, **kwargs):
        return run(["ip", "netns", "exec", name, *args], **kwargs)

    def connect(name, host, port):
        return ns(name, sys.executable, "-c", CONNECT, host, str(port), ok=False).returncode == 0

    def spawn(args, env=None):
        process = subprocess.Popen(["ip", "netns", "exec", hub_ns, *map(str, args)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env, cwd=data)
        processes.append(process)
        return process

    def report(mode, success, *extra):
        result = ns(hub_ns, memdolt, "hub", mode, "--config", config, "--json", *extra, ok=False)
        document = json.loads(result.stdout)
        require(document["ok"] == success and (result.returncode == 0) == success,
                f"unexpected {mode} result: {document}")
        return document

    def as_user(command):
        return ["setpriv", "--reuid", str(uid), "--regid", str(account.pw_gid), "--clear-groups", *command]

    try:
        for name in (hub_ns, private_ns, denied_ns):
            run(["ip", "netns", "add", name])
            namespaces.append(name)
            ns(name, "ip", "link", "set", "lo", "up")
        for peer, iface, v4, v6 in ((private_ns, "private0", "100.90.0", "fd7a:115c:a1e0::"),
                                    (denied_ns, "public0", "192.0.2", "2001:db8::")):
            ns(hub_ns, "ip", "link", "add", iface, "type", "veth", "peer", "name", "peer0", "netns", peer)
            for space, name, number in ((hub_ns, iface, "1"), (peer, "peer0", "2")):
                ns(space, "ip", "address", "add", f"{v4}.{number}/24", "dev", name)
                ns(space, "ip", "-6", "address", "add", f"{v6}{number}/64", "dev", name, "nodad")
                ns(space, "ip", "link", "set", name, "up")
        ns(denied_ns, "ip", "route", "add", "100.90.0.1/32", "via", "192.0.2.1")
        ns(denied_ns, "ip", "-6", "route", "add", "fd7a:115c:a1e0::1/128", "via", "2001:db8::1")
        report("preflight", True, "--files-only")
        report("preflight", False)  # a file on disk is not applied protection
        require(ns(hub_ns, *pre[0], ok=False).returncode != 0, "unit would start without protection")

        ns(hub_ns, nft, "add", "table", "inet", "unrelated")
        sentinel = ns(hub_ns, nft, "--json", "list", "table", "inet", "unrelated").stdout
        ns(hub_ns, nft, "create", "table", "inet", "memdolt_hub")
        report("preflight", False)  # a named but unprotected table also fails
        require(ns(hub_ns, nft, "-f", bundle / "private.nft", ok=False).returncode != 0,
                "foreign table was replaced")
        ns(hub_ns, nft, "delete", "table", "inet", "memdolt_hub")
        ns(hub_ns, nft, "--check", "-f", bundle / "private.nft")
        ns(hub_ns, nft, "-f", bundle / "private.nft")
        # Print only nonsecret applied policy for useful CI diagnostic evidence.
        print(ns(hub_ns, nft, "--json", "--numeric", "list", "table", "inet", "memdolt_hub").stdout, flush=True)
        report("preflight", True)
        require(sentinel == ns(hub_ns, nft, "--json", "list", "table", "inet", "unrelated").stdout,
                "unrelated table changed")

        listeners = spawn([sys.executable, "-c", LISTEN, "3306", "50051", "39000"])
        for _ in range(40):
            if connect(private_ns, "100.90.0.1", 39000):
                break
            time.sleep(0.05)
        for port in (3306, 50051):
            for host in ("100.90.0.1", "fd7a:115c:a1e0::1"):
                require(connect(private_ns, host, port), f"permitted private traffic refused: {host}:{port}")
                require(not connect(denied_ns, host, port), f"denied interface reached private destination: {host}:{port}")
            for host in ("192.0.2.1", "2001:db8::1"):
                require(not connect(denied_ns, host, port), f"public traffic reached hub port: {host}:{port}")
        for host in ("192.0.2.1", "2001:db8::1"):
            require(connect(denied_ns, host, 39000), "unrelated traffic was blocked")
        require(connect(hub_ns, "127.0.0.1", 50051) and connect(hub_ns, "::1", 50051), "loopback refused")
        stop(listeners)
        print("PASS: permitted/denied SQL and remotes ingress on both IP families; unrelated rules and traffic preserved", flush=True)

        report("ready", False)  # privileges must be deliberately bootstrapped
        # Native first-start account bootstrap is isolated and loopback-only.
        # A random test credential exists only in this process environment.
        native_env = {"PATH": "/usr/bin:/bin", "HOME": str(data), "DOLT_ROOT_PASSWORD": uuid.uuid4().hex,
                      "DOLT_DISABLE_EVENT_FLUSH": "1"}
        bootstrap = spawn(as_user([dolt, "sql-server", "--host", "127.0.0.1", "--port", "13307",
                                  "--data-dir", data, "--doltcfg-dir", data / ".doltcfg",
                                  "--privilege-file", data / ".doltcfg/privileges.db", "--socket", data / "dolt.sock"]), native_env)
        for _ in range(100):
            if connect(hub_ns, "127.0.0.1", 13307):
                break
            require(bootstrap.poll() is None, "native bootstrap process failed (logs withheld)")
            time.sleep(0.05)
        require(connect(hub_ns, "127.0.0.1", 13307), "native bootstrap did not listen")
        stop(bootstrap)
        privilege_file = data / ".doltcfg/privileges.db"
        require(privilege_file.is_file() and not privilege_file.is_symlink(), "native privilege file absent")
        privilege_file.chmod(0o600)
        report("ready", True)
        # Replay the actual generated ExecStartPre/ExecStart arguments inside the
        # disposable namespace with the unit's privilege separation, not a second
        # hand-authored Dolt command. PID 1 installation/enablement is not claimed.
        ns(hub_ns, *pre[0])
        ns(hub_ns, *as_user(pre[1]))
        server = spawn(as_user(start[0]), {"PATH": "/usr/bin:/bin", "HOME": str(data), "DOLT_DISABLE_EVENT_FLUSH": "1"})
        for _ in range(100):
            if connect(private_ns, "100.90.0.1", 50051):
                break
            require(server.poll() is None, "generated native server failed (logs withheld)")
            time.sleep(0.05)
        native = report("status", True)
        require(native["observed_dolt_version"] == "1.88.1", "wrong native baseline")
        require(not connect(denied_ns, "192.0.2.1", 50051) and not connect(denied_ns, "2001:db8::1", 50051),
                "real wildcard remotes listener exposed")
        stop(server)
        report("status", False)  # files/version alone cannot claim listening

        ns(hub_ns, "ip", "-6", "address", "del", "fd7a:115c:a1e0::1/64", "dev", "private0")
        began = time.monotonic()
        report("ready", False)
        require(time.monotonic() - began < 10, "readiness failure was unbounded")
        ns(hub_ns, "ip", "-6", "address", "add", "fd7a:115c:a1e0::1/64", "dev", "private0", "nodad")
        # This fixture executable is newly created test code, not a download.
        original = bin_dir / "dolt-verified"
        dolt.rename(original)
        dolt.write_text("#!/bin/sh\nprintf 'dolt version 1.88.2\\n'\n")
        dolt.chmod(0o755)
        skew = report("ready", False)
        require(skew["observed_dolt_version"] == "1.88.2", "skew was not reported")
        dolt.unlink()
        original.rename(dolt)
        ns(hub_ns, nft, "add", "table", "inet", "memdolt_hub", "{ flags dormant; }")
        report("preflight", False)
        ns(hub_ns, nft, "delete", "table", "inet", "memdolt_hub")
        report("preflight", False)
        print("PASS: generated unit/config, real native 1.88.1 startup/status, missing/dormant protection, version and readiness guards", flush=True)
    finally:
        for process in reversed(processes):
            stop(process)
        for name in reversed(namespaces):
            run(["ip", "netns", "delete", name])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--memdolt", type=Path, required=True)
    parser.add_argument("--dolt", type=Path, required=True)
    parser.add_argument("--uid", type=int, required=True)
    args = parser.parse_args()
    require(sys.platform == "linux" and os.geteuid() == 0, "disposable Linux CI root required")
    # /run has a root-owned, non-world-writable ancestry. /tmp deliberately
    # cannot pass production's trusted deployment path check.
    with tempfile.TemporaryDirectory(prefix="memdolt-hub-test-", dir="/run") as directory:
        root = Path(directory)
        root.chmod(0o755)
        verify(root, args.memdolt.resolve(strict=True), args.dolt.resolve(strict=True), args.uid)


if __name__ == "__main__":
    main()
