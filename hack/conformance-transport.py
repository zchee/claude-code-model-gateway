# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Checks used by the SSH transport before the checkout is available."""

import ipaddress
import json
import os
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


def check_archive(root: str, commit: str) -> None:
    """Refuse export archives whose tree contains gitlinks or export attributes."""
    git_env = {
        k: v
        for k, v in os.environ.items()
        if k not in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR")
    }
    tree_out = subprocess.check_output(
        ["git", "--no-optional-locks", "-C", root, "ls-tree", "-rtz", commit],
        env=git_env,
        timeout=30,
    )
    paths: list[str] = []
    for raw in tree_out.split(b"\0"):
        if not raw:
            continue
        meta_b, path_b = raw.split(b"\t", 1)
        mode, obj_type, _ = meta_b.decode("ascii").split(" ", 2)
        rel = path_b.decode("utf-8", errors="surrogateescape")
        if mode == "160000" or obj_type == "commit":
            print(
                f"remote: commit {commit} contains gitlink {rel}; refusing conformance export",
                file=sys.stderr,
            )
            sys.exit(3)
        paths.append(rel)
        if obj_type == "tree":
            paths.append(f"{rel}/")
    if not paths:
        return
    stdin_bytes = (
        b"\0".join(p.encode("utf-8", errors="surrogateescape") for p in paths) + b"\0"
    )
    attr_out = subprocess.check_output(
        [
            "git",
            "--no-optional-locks",
            "-C",
            root,
            "check-attr",
            f"--source={commit}",
            "--stdin",
            "-z",
            "export-ignore",
            "export-subst",
        ],
        input=stdin_bytes,
        env=git_env,
        timeout=30,
    )
    tokens = attr_out.split(b"\0")
    if tokens and tokens[-1] == b"":
        tokens.pop()
    if len(tokens) % 3 != 0:
        raise ValueError("malformed git check-attr output")
    for idx in range(0, len(tokens), 3):
        rel = tokens[idx].decode("utf-8", errors="surrogateescape")
        attr = tokens[idx + 1].decode("ascii")
        value = tokens[idx + 2].decode("utf-8", errors="surrogateescape")
        if value not in ("unspecified", "unset"):
            print(
                f"remote: commit {commit} has {attr}={value} on {rel}; refusing conformance export",
                file=sys.stderr,
            )
            sys.exit(3)


def main(args: list[str]) -> None:
    """Check the destination, archive safety, archive pin, or prune runs."""
    if not args or len(args) != {
        "destination": 3,
        "checksum": 3,
        "prune": 1,
        "check-archive": 3,
    }.get(args[0]):
        print("remote: invalid transport operation or arguments", file=sys.stderr)
        sys.exit(2)
    op, *values = args
    if op == "destination":
        try:
            reject_local(values)
        except (ValueError, socket.gaierror) as exc:
            print(f"remote: {exc}", file=sys.stderr)
            sys.exit(2)
    elif op == "check-archive":
        check_archive(values[0], values[1])
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
