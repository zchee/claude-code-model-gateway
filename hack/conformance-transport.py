# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Checks used by the SSH transport before the checkout is available."""

import ipaddress
import json
import shutil
import socket
import subprocess
import sys
from pathlib import Path


def address(value: str) -> ipaddress.IPv4Address | ipaddress.IPv6Address:
    """Normalize interface addresses, including IPv4-mapped IPv6 addresses."""
    ip = ipaddress.ip_address(value.split("/")[0].split("%")[0])
    return ip.ipv4_mapped or ip if isinstance(ip, ipaddress.IPv6Address) else ip


def reject_local(names: list[str]) -> None:
    """Reject local SSH destinations before creating an archive or connecting."""
    darwin = sys.platform == "darwin"
    cmd = ["/sbin/ifconfig", "-a"] if darwin else ["ip", "-o", "address"]
    family = 0 if darwin else 2
    ours = {
        address(fields[family + 1])
        for line in subprocess.check_output(cmd, text=True).splitlines()
        if len(fields := line.split()) > family + 1
        and fields[family] in ("inet", "inet6")
    }
    if not ours:
        raise RuntimeError("cannot determine local interface addresses")
    local = {socket.gethostname().lower(), socket.getfqdn().lower()}
    local.update([n.split(".")[0] for n in local])
    for name in names:
        name = name.strip("[]").rstrip(".").lower()
        if name in local or name == "localhost" or name.endswith(".localhost"):
            raise ValueError("refusing local destination")
    for info in socket.getaddrinfo(
        names[-1].strip("[]"), None, type=socket.SOCK_STREAM
    ):
        ip = address(str(info[4][0]))
        if ip.is_loopback or ip.is_unspecified or ip in ours:
            raise ValueError("refusing local destination")


def main(args: list[str]) -> None:
    """Check the destination, verify an archive pin, or prune negative-control runs."""
    if not args or len(args) != {"destination": 3, "checksum": 3, "prune": 1}.get(
        args[0]
    ):
        print("remote: invalid transport operation or arguments", file=sys.stderr)
        sys.exit(2)
    op, *values = args
    if op == "destination":
        try:
            reject_local(values)
        except (ValueError, socket.gaierror) as exc:
            print(f"remote: {exc}", file=sys.stderr)
            sys.exit(2)
    elif op == "checksum":
        with Path(values[0]).open() as f:
            versions = json.load(f)
        print(
            next(
                a["sha256"]
                for v in versions
                for a in v["files"]
                if a["filename"] == values[1]
            )
        )
    elif op == "prune":
        runs = sorted(
            Path.cwd().glob("run.*"), key=lambda p: p.stat().st_mtime_ns, reverse=True
        )
        for old in runs[5:]:
            shutil.rmtree(old)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (
        OSError,
        subprocess.SubprocessError,
        RuntimeError,
        ValueError,
        LookupError,
        TypeError,
        StopIteration,
    ) as exc:
        # No local tool or archive error is a comparison verdict.
        print(f"remote: transport setup failed: {exc}", file=sys.stderr)
        sys.exit(70)
