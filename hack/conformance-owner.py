# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Own a venue launch from outside its scope; never reap an orphan."""

import errno
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path


def write_atomic(target: Path, content: str) -> None:
    """Write and fsync a temp sibling, then atomically replace target."""
    tmp = target.with_suffix(".new")
    with tmp.open("w", encoding="utf-8") as fh:
        fh.write(content)
        fh.flush()
        os.fsync(fh.fileno())
    tmp.replace(target)


def identity(pid: int) -> tuple[bytes, int] | None:
    """Return state and start tick, or None only on positive evidence of exit."""
    try:
        fields = Path(f"/proc/{pid}/stat").read_bytes().rsplit(b")", 1)[1].split()
    except (FileNotFoundError, ProcessLookupError):
        return None
    return fields[0], int(fields[19])


def launcher_state(pid: int, tick: int) -> tuple[str, str]:
    """Classify a recorded launcher as gone, alive, or cannot-tell."""
    try:
        value = identity(pid)
    except (OSError, IndexError, ValueError) as exc:
        return "cannot-tell", str(exc)
    if value is None or value[0] == b"Z" or value[1] != tick:
        return "gone", ""
    return "alive", ""


def stop_launcher(pid: int, tick: int) -> None:
    """Stop only the matching process, without waiting for or reaping a nonchild."""
    if sys.platform != "linux":
        raise RuntimeError("launcher verification requires Linux pidfds")
    state, detail = launcher_state(pid, tick)
    if state == "cannot-tell":
        raise RuntimeError(f"cannot verify launcher {pid}: {detail}")
    if state == "gone":
        return
    try:
        fd = os.pidfd_open(pid)
    except ProcessLookupError:
        return
    try:
        state, detail = launcher_state(pid, tick)
        if state == "cannot-tell":
            raise RuntimeError(f"cannot verify launcher {pid}: {detail}")
        if state == "alive":
            signal.pidfd_send_signal(fd, signal.SIGKILL)
        deadline = time.monotonic() + 5
        while True:
            state, detail = launcher_state(pid, tick)
            if state == "gone":
                return
            if state == "cannot-tell":
                raise RuntimeError(f"cannot verify launcher {pid}: {detail}")
            if time.monotonic() >= deadline:
                raise TimeoutError(f"launcher {pid} did not stop")
            time.sleep(0.05)
    except ProcessLookupError:
        return
    finally:
        os.close(fd)


def manager(*args: str) -> str:
    """Run one bounded user-manager operation, raising on uncertainty."""
    return subprocess.check_output(
        ["systemctl", "--user", *args], text=True, timeout=10
    ).strip()


def prop(unit: str, name: str) -> str:
    """Read an individual systemd property."""
    return manager("show", "--value", "-p", name, "--", unit)


def members(unit: str) -> list[int]:
    """Read actual cgroup membership; pseudo-file size is not membership."""
    cg = prop(unit, "ControlGroup")
    if not cg:
        uid = os.getuid()
        cg = f"/user.slice/user-{uid}.slice/user@{uid}.service/app.slice/{unit}"
    group = Path("/sys/fs/cgroup") / cg.lstrip("/")
    try:
        events = dict(
            line.split() for line in (group / "cgroup.events").read_text().splitlines()
        )
        pids = [int(s) for s in (group / "cgroup.procs").read_text().split()]
    except FileNotFoundError:
        if group.exists():
            raise
        return []
    # populated includes descendants even if the top-level cgroup is empty.
    return pids or ([-1] if events["populated"] != "0" else [])


def drain(unit: str) -> None:
    """Stop all members of the owned unit and require verified inactivity."""
    state = prop(unit, "ActiveState")
    if state not in ("inactive", "failed") or members(unit):
        manager("stop", "--", unit)
    if prop(unit, "ActiveState") == "failed":
        manager("reset-failed", "--", unit)
    if prop(unit, "ActiveState") != "inactive" or members(unit):
        raise RuntimeError(f"{unit}: cannot verify empty inactive scope")


def reservation(path: Path) -> tuple[list[str], tuple[int, int] | None]:
    """Accept only complete current or known legacy records."""
    pairs = [line.split("=", 1) for line in path.read_text().splitlines()]
    fields = dict(pairs)
    if not fields or len(fields) != len(pairs):
        raise ValueError("empty or duplicate fields in reservation")
    keys = set(fields)
    launcher = None
    if keys == {"unit", "launcher_pid", "launcher_start"}:
        launcher = int(fields.pop("launcher_pid")), int(fields.pop("launcher_start"))
        if min(launcher) < 1:
            raise ValueError("invalid launcher identity")
    elif keys not in ({"pending"}, {"unit"}, {"outer", "job"}, {"outer"}):
        raise ValueError("incomplete or unknown reservation format")
    units = list(fields.values())
    if not all(re.fullmatch(r"mgw-[A-Za-z0-9_-]+\.scope", unit) for unit in units):
        raise ValueError("invalid scope name in reservation")
    return units, launcher


def inspect_record(path: Path, recover: bool) -> None:
    """Describe or verify the reservation without ever clearing it."""
    units, launcher = reservation(path)
    if recover:
        if launcher:
            stop_launcher(*launcher)
        for unit in units:
            drain(unit)
        return
    parts: list[str] = []
    if launcher:
        state, detail = launcher_state(*launcher)
        suffix = f" ({detail})" if detail else ""
        parts.append(f"launcher={launcher[0]}:{state}{suffix}")
    for unit in units:
        try:
            parts.append(
                f"unit={unit} state={prop(unit, 'ActiveState')} pids={members(unit)}"
            )
        except (OSError, subprocess.SubprocessError, ValueError, KeyError) as exc:
            parts.append(f"unit={unit} state=cannot-tell ({exc})")
    print("; ".join(parts))


def run(path: Path, unit: str, limit: int, args: list[str]) -> int:
    """Release the launcher only after publishing its identity under the flock."""
    control, work, *payload = args
    directory = Path(work)
    ready = directory / "control.ready"
    port: int | None = None
    if path.read_text() != f"pending={unit}\n":
        raise ValueError("not the current reservation")
    interrupted = 0

    def interrupt(sig: int, _frame: object) -> None:
        nonlocal interrupted
        interrupted = sig

    for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, interrupt)
    gate, release = os.pipe()
    stub = (
        "import os,sys; fd=int(sys.argv[1]); token=os.read(fd,1); os.close(fd); "
        "sys.exit(70) if token!=b'G' else os.execvp(sys.argv[2],sys.argv[2:])"
    )
    cmd = [
        sys.executable,
        "-I",
        "-c",
        stub,
        str(gate),
        "systemd-run",
        "--user",
        "--scope",
        f"--unit={unit}",
        f"-pRuntimeMaxSec={limit}",
        "-pTimeoutStopSec=5",
        "-pKillMode=control-group",
        "-pCollectMode=inactive",
        "--quiet",
        "--",
        "env",
        "-u",
        "XDG_RUNTIME_DIR",
        "-u",
        "DBUS_SESSION_BUS_ADDRESS",
        "CONFORMANCE_VENUE_SCOPED=1",
        *payload,
    ]
    # Keep the launcher in its own session so an outside watchdog killing the
    # owner's process group leaves the scope to its systemd deadline or recovery.
    child = subprocess.Popen(cmd, pass_fds=(gate,), start_new_session=True)
    os.close(gate)
    released = False
    try:
        proc = identity(child.pid)
        if proc is None:
            raise RuntimeError("launcher exited before registration")
        write_atomic(
            path,
            f"unit={unit}\nlauncher_pid={child.pid}\nlauncher_start={proc[1]}\n",
        )
        os.write(release, b"G")
        released = True
    finally:
        os.close(release)
        if not released:
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
    while child.poll() is None and not interrupted:
        if control != "run" and ready.is_file():
            port = int(ready.read_text())
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                pass
            print(f"remote: control listening on {port} in {unit}", flush=True)
            if control == "check-owner-deadline":
                # Stop only ourselves, already owned by the outer timeout. Record
                # the identity for diagnostics; cleanup never signals this numeric PID.
                owner = os.getpid()
                stamp = identity(owner)
                if stamp is None:
                    raise RuntimeError("cannot identify control owner")
                print(
                    f"remote: freezing owner pid={owner} start={stamp[1]} unit={unit}",
                    flush=True,
                )
                signal.raise_signal(signal.SIGSTOP)
            else:
                signal.raise_signal(signal.SIGTERM)
        time.sleep(0.05)
    if child.returncode is None:
        stop_launcher(child.pid, proc[1])
    status = child.wait(timeout=5)
    expired = prop(unit, "Result") == "timeout"
    leftover = members(unit)
    if control == "check-cancellation":
        if port is None or not leftover:
            raise RuntimeError("no live listener to drain after launcher exit")
        with socket.socket() as probe:
            try:
                probe.bind(("127.0.0.1", port))
            except OSError as exc:
                if exc.errno != errno.EADDRINUSE:
                    raise
            else:
                raise RuntimeError(f"control port {port} was free before scope drain")
        print(f"remote: launcher exited; port {port} still bound; members={leftover}")
    drain(unit)
    if port is not None:
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", port))
        print(f"remote: control listener {port} released, {unit} verified empty")
    # Only payload results in this private range are verdicts. A failing
    # systemd-run or shell setup can otherwise look exactly like a test failure.
    status = {80: 0, 81: 1, 82: 2, 83: 124}.get(
        status, status if status == 97 or status < 0 or status > 128 else 70
    )
    if expired or status == 124:
        status = 124
    elif interrupted:
        status = 128 + interrupted
    elif leftover:
        print(f"remote: cleaned up leftover scope members: {leftover}", file=sys.stderr)
        status = status or 70
    if status < 0:
        status = 128 - status
    try:
        if status == 0:
            shutil.rmtree(directory)
        else:
            print(f"failed run retained: {directory}", file=sys.stderr)
        runs = sorted(
            directory.parent.glob("run.*"),
            key=lambda p: p.stat().st_mtime_ns,
            reverse=True,
        )
        for old in runs[5:]:
            shutil.rmtree(old)
    except OSError as exc:
        print(f"remote: could not prune run directories: {exc}", file=sys.stderr)
    if interrupted and status != 124:
        status = 128 + interrupted
    label = {0: "pass", 1: "failure", 2: "usage", 124: "timeout"}.get(
        status, "no-verdict"
    )
    record = f"unit={unit} class={label} status={status}"
    write_atomic(path.with_name("last-outcome"), record + "\n")
    path.unlink()
    print(f"remote: {record}", flush=True)
    return status


def main(args: list[str]) -> int:
    """Dispatch only the operation requested by the outside venue owner."""
    os.environ["XDG_RUNTIME_DIR"] = f"/run/user/{os.getuid()}"
    os.environ.pop("DBUS_SESSION_BUS_ADDRESS", None)
    command, file, *rest = args
    try:
        if command == "listen" and os.environ.get("CONFORMANCE_VENUE_SCOPED") == "1":
            if os.fork():
                time.sleep(60)
                return 70
            os.setsid()
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                listener.listen(1)
                write_atomic(Path(file), str(listener.getsockname()[1]))
                time.sleep(60)
            return 70
        if command == "run":
            return run(Path(file), rest[0], int(rest[1]), rest[2:])
        if command not in ("status", "recover"):
            raise ValueError("invalid owner operation")
        inspect_record(Path(file), command == "recover")
        return 0
    except (
        OSError,
        ValueError,
        IndexError,
        KeyError,
        subprocess.SubprocessError,
        RuntimeError,
    ) as exc:
        print(
            f"remote: cannot verify {file}: {exc}; reservation retained",
            file=sys.stderr,
        )
        return 97


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
