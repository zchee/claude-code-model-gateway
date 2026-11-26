# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Own a venue launch from outside its scope; never reap an orphan."""

import errno
import hashlib
import json
import os
import pwd
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from collections import Counter
from collections.abc import Mapping
from datetime import UTC, datetime
from pathlib import Path

# Sorted escaped TSV: registration and depth; registration and credited count.
# Both profiles come from the reviewed identity run, not the binary under test.
CASE_PROFILE = "907f889cd952126193724e3da5b219642a29a0437f8573061de76057fcfe227a"
COUNT_PROFILE = "0b2dad4d4ec33d0201aa1ca6a3de9afe5be4b581587be3d327ffa673f1cddda2"
SOURCE_PIN = "4a10b3bb214b5ecc779c18fa1ba16ff0943168ec00ed881d358449a147b0ca06"
CENSUS = "path\tline\tcolumn\tdepth\tname\tstatus\tdetail"
LAUNCHES = "path\tline\tcolumn\tname\tbinary\tbinary_sha256"
HOSTED_RUN_ID_RE = re.compile(r"gha-[1-9][0-9]*-[1-9][0-9]*")
VENUE_UNIT_RE = re.compile(r"mgw-run-[0-9]+-[0-9]+\.scope")
VENUE_RUN_ID_RE = re.compile(r"venue-mgw-run-[0-9]+-[0-9]+")
COMMIT_RE = re.compile(r"[0-9a-f]{40}")
DIGEST_RE = re.compile(r"[0-9a-f]{64}")
POSITIVE_INT_RE = re.compile(r"[1-9][0-9]*")


def profile(rows: list[str]) -> str:
    """Fingerprint every row, including multiplicity, independent of scheduling."""
    return hashlib.sha256("".join(s + "\n" for s in sorted(rows)).encode()).hexdigest()


def venue_run_id(unit: str) -> str:
    """Derive the dedicated-venue run identifier from its systemd scope unit."""
    if not VENUE_UNIT_RE.fullmatch(unit):
        raise ValueError(f"invalid venue scope unit: {unit}")
    return f"venue-{unit.removesuffix('.scope')}"


def check_producer_run_id(producer: str, run_id: str) -> None:
    """Require the run identifier shape to match the declared producer."""
    if producer == "hosted":
        if not HOSTED_RUN_ID_RE.fullmatch(run_id):
            raise ValueError("invalid run identity for hosted producer")
    elif producer == "venue":
        if not VENUE_RUN_ID_RE.fullmatch(run_id):
            raise ValueError("invalid run identity for venue producer")
    else:
        raise ValueError(f"unknown producer: {producer}")


def check_producer_env(
    producer: str, run_id: str, commit: str, env: Mapping[str, str]
) -> None:
    """Refuse a manifest whose identity disagrees with the active producer environment."""
    if (
        env.get("GITHUB_ACTIONS") == "true"
        or env.get("RUNNER_ENVIRONMENT") == "github-hosted"
    ):
        if producer != "hosted":
            raise ValueError("producer does not match hosted environment")
        gh_run = env.get("GITHUB_RUN_ID", "")
        gh_attempt = env.get("GITHUB_RUN_ATTEMPT", "")
        if not (
            POSITIVE_INT_RE.fullmatch(gh_run) and POSITIVE_INT_RE.fullmatch(gh_attempt)
        ):
            raise ValueError("missing hosted run identity in environment")
        if run_id != f"gha-{gh_run}-{gh_attempt}":
            raise ValueError("run identity does not match hosted environment")
        if env.get("GITHUB_SHA", "") != commit:
            raise ValueError("commit does not match hosted environment")
    elif env.get("CONFORMANCE_VENUE_UNIT"):
        if producer != "venue":
            raise ValueError("producer does not match venue environment")
        if run_id != venue_run_id(env["CONFORMANCE_VENUE_UNIT"]):
            raise ValueError("run identity does not match venue scope")
        venue_commit = env.get("CONFORMANCE_VENUE_COMMIT", "")
        if venue_commit and commit != venue_commit:
            raise ValueError("commit does not match venue archive")


def resolve_hosted_identity(
    root: Path, env: Mapping[str, str], home: Path | None = None
) -> tuple[str, str, str]:
    """Derive hosted run identity and commit from CI environment, attestation and Git."""
    if env.get("CONFORMANCE_VENUE_SCOPED") == "1":
        raise ValueError("hosted export cannot run inside a dedicated venue scope")
    if (
        env.get("GITHUB_ACTIONS") != "true"
        or env.get("RUNNER_ENVIRONMENT") != "github-hosted"
    ):
        raise ValueError("hosted export requires a GitHub-hosted runner environment")
    gh_run = env.get("GITHUB_RUN_ID", "")
    gh_attempt = env.get("GITHUB_RUN_ATTEMPT", "")
    sha = env.get("GITHUB_SHA", "")
    if not (
        POSITIVE_INT_RE.fullmatch(gh_run)
        and POSITIVE_INT_RE.fullmatch(gh_attempt)
        and COMMIT_RE.fullmatch(sha)
    ):
        raise ValueError("invalid hosted run identity or GITHUB_SHA")
    account_home = home if home is not None else Path(pwd.getpwuid(os.getuid()).pw_dir)
    attest = account_home / ".model-gateway-test-venue"
    if attest.is_symlink() or not attest.is_file():
        raise ValueError("hosted venue attestation is missing")
    lines = attest.read_text(encoding="utf-8").splitlines()
    if (
        len(lines) != 1
        or len(lines[0].split("\t")) != 3
        or lines[0].split("\t")[2] != "ci"
    ):
        raise ValueError("venue attestation is not a hosted ci attestation")
    git_env = {
        k: v
        for k, v in env.items()
        if k not in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR")
    }
    head = subprocess.check_output(
        ["git", "-C", str(root), "rev-parse", "--verify", "HEAD^{commit}"],
        text=True,
        env=git_env,
        timeout=15,
    ).strip()
    if head != sha:
        raise ValueError(f"checkout HEAD {head} does not match GITHUB_SHA {sha}")
    dirty = subprocess.check_output(
        [
            "git",
            "--no-optional-locks",
            "-C",
            str(root),
            "status",
            "--porcelain=v1",
            "--untracked-files=all",
            "--ignore-submodules=none",
        ],
        text=True,
        env=git_env,
        timeout=15,
    )
    if dirty:
        raise ValueError("checkout worktree is not clean")
    return "hosted", f"gha-{gh_run}-{gh_attempt}", sha


def verify_export(
    directory: Path,
    manifest_file: str = "manifest.json",
    *,
    expected_producer: str | None = None,
    expected_run_id: str | None = None,
    expected_commit: str | None = None,
    env: Mapping[str, str] | None = None,
) -> None:
    """Check the completed bundle without a checkout, target binary or launch log."""
    if directory.is_symlink() or not directory.is_dir():
        raise ValueError("export path is not a regular directory")
    for filename in (manifest_file, "census.tsv", "launches.tsv"):
        entry = directory / filename
        if entry.is_symlink() or not entry.is_file():
            raise ValueError(f"missing or invalid {filename}")
    manifest = json.loads((directory / manifest_file).read_text(encoding="utf-8"))
    if not isinstance(manifest, dict):
        raise TypeError("manifest must be a JSON object")
    producer = manifest["producer"]
    run_id = manifest["run_id"]
    commit = manifest["commit"]
    if not (
        isinstance(producer, str)
        and isinstance(run_id, str)
        and isinstance(commit, str)
    ):
        raise TypeError("invalid producer, run identity, or commit")
    check_producer_run_id(producer, run_id)
    if not COMMIT_RE.fullmatch(commit):
        raise ValueError("invalid commit")
    if (
        type(manifest.get("clean_tree")) is not bool
        or manifest["clean_tree"] is not True
    ):
        raise ValueError("export does not record a clean checkout")
    if expected_producer is not None and producer != expected_producer:
        raise ValueError("manifest producer does not match expected producer")
    if expected_run_id is not None and run_id != expected_run_id:
        raise ValueError("manifest run identity does not match expected run identity")
    if expected_commit is not None and commit != expected_commit:
        raise ValueError("manifest commit does not match expected commit")
    if env is not None:
        check_producer_env(producer, run_id, commit, env)
    meta = {"run_id": run_id, "commit": commit}
    binary, digest = manifest["binary"], manifest["binary_sha256"]
    if (
        not isinstance(binary, str)
        or not re.fullmatch(r"/(?!/)[A-Za-z0-9_./-]+", binary)
        or os.path.normpath(binary) != binary
        or os.path.dirname(binary) == binary
    ):
        raise ValueError("invalid binary path")
    if not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
        raise ValueError("invalid binary digest")
    if (
        manifest["test_source_sha256"] != SOURCE_PIN
        or manifest["selection"] != "all-conformance"
    ):
        raise ValueError("unreviewed test source or partial selection")
    if (
        manifest["verdict"] != "pass"
        or type(manifest["status"]) is not int
        or manifest["status"] != 0
    ):
        raise ValueError("not a passing run")
    start, end = (datetime.fromisoformat(manifest[k]) for k in ("started", "ended"))
    if start.tzinfo is None or end.tzinfo is None or end < start:
        raise ValueError("invalid run interval")

    def rows(name: str, columns: str) -> list[list[str]]:
        data = (directory / name).read_bytes().decode("utf-8")
        if not data.endswith("\n") or "\r" in data:
            raise ValueError(f"malformed {name}")
        header, fields, *lines = data[:-1].split("\n")
        if json.loads(header) != meta:
            raise ValueError(f"{name} run identity or commit mismatch")
        result = [line.split("\t") for line in lines]
        if fields != columns or any(
            len(row) != len(columns.split("\t")) for row in result
        ):
            raise ValueError(f"malformed {name} columns")
        return result

    cases: list[str] = []
    counts: Counter[str] = Counter()
    for path, line, col, depth, name, status, detail in rows("census.tsv", CENSUS):
        if status != "pass" or detail != "-":
            raise ValueError("non-passing census entry")
        case_key = f"{path}\t{line}\t{col}\t{name}"
        if case_key in counts:
            raise ValueError("duplicate census entry")
        cases.append(f"{path}\t{line}\t{col}\t{depth}\t{name}")
        counts[case_key] = 0
    if profile(cases) != CASE_PROFILE:
        raise ValueError("census does not match full selection")
    for path, line, col, name, image, sha in rows("launches.tsv", LAUNCHES):
        case = f"{path}\t{line}\t{col}\t{name}"
        if case not in counts:
            raise ValueError("launch outside census")
        if (image, sha) != (binary, digest):
            raise ValueError("mixed binary path or digest")
        counts[case] += 1
    if profile([f"{case}\t{count}" for case, count in counts.items()]) != COUNT_PROFILE:
        raise ValueError("unexpected per-case launch count")


def prepare_export(
    directory: Path,
    manifest: dict[str, object],
    candidate: dict[str, str],
    *,
    expected_producer: str,
    expected_run_id: str,
    expected_commit: str,
    env: Mapping[str, str] | None = None,
) -> None:
    """Prepare a verified bundle in an unpublished directory after cleanup."""
    header = json.dumps({k: manifest[k] for k in ("run_id", "commit")}) + "\n"
    (directory / "manifest.pending").write_text(
        json.dumps(manifest) + "\n", encoding="utf-8"
    )
    for name in ("census", "launches"):
        (directory / f"{name}.tsv").write_text(
            header + candidate[name], encoding="utf-8"
        )
    verify_export(
        directory,
        "manifest.pending",
        expected_producer=expected_producer,
        expected_run_id=expected_run_id,
        expected_commit=expected_commit,
        env=env,
    )


def hosted_export(root: Path, env: Mapping[str, str]) -> int:
    """Run the full binary conformance suite on a hosted runner and write its export."""
    target = env.get("TARGET", "identity")
    mode = env.get("MODE", "suite") or "suite"
    classification = env.get("CLASS", "")
    args = ["conformance", f"TARGET={target}", f"MODE={mode}"]
    if classification:
        args.append(f"CLASS={classification}")
    for key in ("PHASE", "PACKAGE", "GATE", "SITE"):
        if env.get(key):
            args.append(f"{key}={env[key]}")
    try:
        producer, run_id, commit = resolve_hosted_identity(root, env)
        binary = export_target(args, 0, commit)
        if classification and classification != "conformance":
            raise ValueError("invalid CLASS selector")
        binary_path = Path(binary)
        if (
            not binary_path.is_file()
            or not os.access(binary, os.X_OK)
            or os.path.realpath(binary) != binary
        ):
            raise ValueError(f"invalid executable: {binary}")
        out_raw = env.get("EXPORT_DIR", "")
        if not out_raw and env.get("RUNNER_TEMP"):
            out_raw = str(Path(env["RUNNER_TEMP"]) / "conformance-export")
        if (
            not out_raw
            or not os.path.isabs(out_raw)
            or os.path.normpath(out_raw) != out_raw
            or os.path.dirname(out_raw) == out_raw
        ):
            raise ValueError("EXPORT_DIR must be a clean absolute path")
        output = Path(out_raw)
        if output.exists() or output.is_symlink():
            raise ValueError(f"EXPORT_DIR already exists: {output}")
        if not output.parent.is_dir() or output.parent.is_symlink():
            raise ValueError(f"EXPORT_DIR parent is not a directory: {output.parent}")
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
        print(f"conformance export refused: {exc}", file=sys.stderr)
        return 70

    venue_rc = subprocess.run([str(root / "hack/venue-check")], check=False).returncode
    if venue_rc != 0:
        return 97

    started = datetime.now(UTC).isoformat()
    capture_dir = Path(tempfile.mkdtemp(prefix="mgw-capture-"))
    candidate_path = capture_dir / "candidate.json"
    stage: Path | None = None
    try:
        run_env = dict(env)
        run_env["TARGET"] = f"binary:{binary}"
        run_env["MODE"] = "suite"
        run_env["CONFORMANCE_CAPTURE"] = binary
        run_env["CONFORMANCE_CANDIDATE"] = str(candidate_path)
        proc = subprocess.run(
            [str(root / "hack/conformance")], env=run_env, check=False
        )
        if proc.returncode != 0:
            print(
                f"conformance export refused: comparison status {proc.returncode} is not a pass",
                file=sys.stderr,
            )
            return 70
        post_identity = resolve_hosted_identity(root, env)
        if post_identity != (producer, run_id, commit):
            raise ValueError("hosted run identity or checkout changed during run")
        candidate = json.loads(candidate_path.read_text(encoding="utf-8"))
        if not isinstance(candidate, dict) or candidate.get("binary") != binary:
            raise ValueError("candidate does not match the requested executable")
        stage = Path(tempfile.mkdtemp(prefix=".pending-", dir=output.parent))
        prepare_export(
            stage,
            {
                "producer": producer,
                "run_id": run_id,
                "commit": commit,
                "clean_tree": True,
                "binary": binary,
                "binary_sha256": candidate["binary_sha256"],
                "test_source_sha256": candidate["test_source_sha256"],
                "selection": "all-conformance",
                "started": started,
                "ended": datetime.now(UTC).isoformat(),
                "verdict": "pass",
                "status": 0,
            },
            candidate,
            expected_producer=producer,
            expected_run_id=run_id,
            expected_commit=commit,
            env=env,
        )
        stage.rename(output)
        stage = None
        (output / "manifest.pending").rename(output / "manifest.json")
        print(f"conformance export: {output}", flush=True)
        return 0
    except (
        OSError,
        KeyError,
        ValueError,
        TypeError,
        subprocess.SubprocessError,
    ) as exc:
        print(f"conformance export refused: {exc}", file=sys.stderr)
        return 70
    finally:
        shutil.rmtree(capture_dir, ignore_errors=True)
        if stage is not None:
            shutil.rmtree(stage, ignore_errors=True)


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


def export_target(args: list[str], status: int, commit: str) -> str:
    """Return the image only for a successful, isolated, full binary comparison."""
    if status != 0:
        raise ValueError(f"comparison status {status} is not a pass")
    options = dict(arg.split("=", 1) for arg in args if "=" in arg)
    target = options.get("TARGET", "identity")
    if not target.startswith("binary:"):
        raise ValueError("source-runtime target")
    if [arg for arg in args if "=" not in arg] != ["conformance"]:
        raise ValueError("expected one conformance goal")
    if options.get("MODE", "suite") != "suite" or any(
        k in options for k in ("PHASE", "PACKAGE", "GATE", "SITE")
    ):
        raise ValueError("partial selection")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("checkout was not a clean commit archive")
    return target.removeprefix("binary:")


def run(path: Path, unit: str, limit: int, args: list[str]) -> int:
    """Release the launcher only after publishing its identity under the flock."""
    split = args.index("--")
    control, work, commit, *goals = args[:split]
    payload = args[split + 1 :]
    run_id = venue_run_id(unit)
    started = datetime.now(UTC).isoformat()
    directory = Path(work)
    candidate_path = directory / "conformance-candidate.json"
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
    capture = ""
    if control == "run" and "conformance" in goals:
        try:
            capture = export_target(goals, 0, commit)
        except ValueError:
            # Diagnostics still run; report the refusal alongside their actual outcome.
            capture = ""
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
        f"CONFORMANCE_VENUE_UNIT={unit}",
        f"CONFORMANCE_CAPTURE={capture}",
        f"CONFORMANCE_CANDIDATE={candidate_path if capture else ''}",
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
    candidate: dict[str, str] | None = None
    binary = ""
    if control == "run" and "conformance" in goals:
        try:
            binary = export_target(goals, status, commit)
        except ValueError as exc:
            print(f"conformance export refused: {exc}", file=sys.stderr)
        else:
            try:
                candidate = json.loads(candidate_path.read_text(encoding="utf-8"))
                if not isinstance(candidate, dict) or candidate.get("binary") != binary:
                    raise ValueError(
                        "candidate does not match the requested executable"
                    )
            except (OSError, ValueError, KeyError, TypeError) as exc:
                candidate = None
                status = 70
                print(f"conformance export refused (status=70): {exc}", file=sys.stderr)
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
    stage: Path | None = None
    output = path.parent / "exports" / run_id
    if candidate is not None and status == 0 and not interrupted:
        try:
            output.parent.mkdir(mode=0o700, exist_ok=True)
            stage = Path(tempfile.mkdtemp(prefix=".pending-", dir=output.parent))
            prepare_export(
                stage,
                {
                    "producer": "venue",
                    "run_id": run_id,
                    "commit": commit,
                    "clean_tree": True,
                    "binary": binary,
                    "binary_sha256": candidate["binary_sha256"],
                    "test_source_sha256": candidate["test_source_sha256"],
                    "selection": "all-conformance",
                    "started": started,
                    "ended": datetime.now(UTC).isoformat(),
                    "verdict": "pass",
                    "status": status,
                },
                candidate,
                expected_producer="venue",
                expected_run_id=run_id,
                expected_commit=commit,
                env={
                    "CONFORMANCE_VENUE_UNIT": unit,
                    "CONFORMANCE_VENUE_COMMIT": commit,
                },
            )
        except (OSError, KeyError, ValueError, TypeError) as exc:
            print(f"conformance export refused (status=70): {exc}", file=sys.stderr)
            status = 70
    if interrupted and status != 124:
        status = 128 + interrupted
    exported: Path | None = None
    if stage is not None:
        if status == 0:
            try:
                stage.rename(output)
                (output / "manifest.pending").rename(output / "manifest.json")
                exported = output
            except OSError as exc:
                print(f"conformance export refused (status=70): {exc}", file=sys.stderr)
                status = 70
                shutil.rmtree(stage, ignore_errors=True)
        else:
            shutil.rmtree(stage, ignore_errors=True)
    label = {0: "pass", 1: "failure", 2: "usage", 124: "timeout"}.get(
        status, "no-verdict"
    )
    record = f"unit={unit} class={label} status={status}"
    write_atomic(path.with_name("last-outcome"), record + "\n")
    path.unlink()
    if exported is not None:
        print(f"remote: conformance export {exported}", flush=True)
    print(f"remote: {record}", flush=True)
    return status


def main(args: list[str]) -> int:
    """Dispatch only the operation requested by the outside venue owner."""
    if args[:1] == ["verify"]:
        try:
            if len(args) != 2:
                raise ValueError("usage: conformance-export verify DIR")
            verify_export(Path(args[1]), env=os.environ)
        except (OSError, KeyError, ValueError, TypeError) as exc:
            print(f"conformance export refused: {exc}", file=sys.stderr)
            return 70
        return 0
    if args[:1] == ["export"]:
        if len(args) != 2:
            print("usage: conformance-export run", file=sys.stderr)
            return 70
        return hosted_export(Path(args[1]), os.environ)
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
