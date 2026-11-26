# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Exercise the export producer and verifier through the real fixture pipeline."""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
import shutil
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from unittest import TestCase
from unittest.mock import patch

FIXTURE_FILE = "test/conformance/adapter/dispatch.test.js"
FIXTURE_CASE = "recording executable receives sync and async fixture launches"
COPIED_FILES = (
    "hack/conformance-owner.py",
    "hack/conformance-export",
    "hack/conformance",
    "hack/conformance-job",
    "hack/conformance-selection",
    "hack/conformance-supervise",
    "hack/conformance-transport.py",
    "hack/venue-check",
    "hack/pinned-source",
    "hack/baseline-node-reporter.mjs",
    "hack/baseline-extract",
    "test/conformance/adapter/run.cjs",
    "test/conformance/adapter/evidence.cjs",
    "test/conformance/adapter/preload.cjs",
    "test/conformance/adapter/dispatch.test.js",
)


def _git_env(work: Path) -> dict[str, str]:
    """Return a clean Git environment isolated from user and system config."""
    return {
        "PATH": os.environ.get("PATH", ""),
        "HOME": str(work),
        "LC_ALL": "C",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_ATTR_NOSYSTEM": "1",
        "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_TERMINAL_PROMPT": "0",
    }


def _git_commit_all(repo: Path, env: dict[str, str], message: str) -> str:
    """Stage all files in repo, commit, and return HEAD."""
    subprocess.check_call(["git", "-C", str(repo), "add", "-A"], env=env)
    subprocess.check_call(
        [
            "git",
            "-C",
            str(repo),
            "-c",
            "user.name=test",
            "-c",
            "user.email=test@example.invalid",
            "commit",
            "-q",
            "-m",
            message,
        ],
        env=env,
    )
    return subprocess.check_output(
        ["git", "-C", str(repo), "rev-parse", "HEAD"], text=True, env=env
    ).strip()


def _build_fixture_repo(root: Path, work: Path) -> tuple[Path, str]:
    """Create a clean Git checkout containing production scripts and a 1-case inventory."""
    fixture_root = work / "fixture-repo"
    fixture_root.mkdir()
    for rel in COPIED_FILES:
        dst = fixture_root / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / rel, dst)

    (fixture_root / ".gitignore").write_text(".cache/\n", encoding="utf-8")
    (fixture_root / "test/conformance/classification.tsv").write_text(
        "path\tline\tcolumn\tname\tclass\tphase\tpackage\tgate\tnote\n"
        f"{FIXTURE_FILE}\t26\t1\t{FIXTURE_CASE}\tconformance\tphase-2\tinternal/adapter\ttask\tFixture dispatch case.\n",
        encoding="utf-8",
    )
    baseline_dir = fixture_root / "test/conformance/baseline"
    baseline_dir.mkdir(parents=True, exist_ok=True)
    census_text = (
        "path\tline\tcolumn\tdepth\tname\tstatus\tdetail\n"
        f"{FIXTURE_FILE}\t26\t1\t0\t{FIXTURE_CASE}\tpass\t-\n"
    )
    census_path = baseline_dir / "census.tsv"
    census_path.write_text(census_text, encoding="utf-8")
    census_sha = hashlib.sha256(census_path.read_bytes()).hexdigest()
    (baseline_dir / "run.json").write_text(
        json.dumps({"census_sha256": census_sha}) + "\n", encoding="utf-8"
    )
    (fixture_root / "test/conformance/launch-sites.tsv").write_text(
        "path\tline\tcolumn\tkind\tvia\tcase\targv\n"
        f"{FIXTURE_FILE}\t28\t16\tcli\tspawnGatewayProcessSync\t{FIXTURE_CASE}\t[CLI, 'status']\n"
        f"{FIXTURE_FILE}\t31\t17\tcli\tspawnGatewayProcess\t{FIXTURE_CASE}\t[CLI, 'status']\n",
        encoding="utf-8",
    )
    (fixture_root / "test/conformance/adapter/repeated-sites.tsv").write_text(
        "site\tcount\n", encoding="utf-8"
    )

    git_env = _git_env(work)
    subprocess.check_call(["git", "-C", str(fixture_root), "init", "-q"], env=git_env)
    head = _git_commit_all(fixture_root, git_env, "fixture inventory")

    # Seed the pinned source tree under .cache (ignored by git) without node_modules
    # so pinned-source verifies it offline on each fixture run.
    cached_source = root / ".cache/model-gateway-js"
    assert cached_source.is_dir(), f"missing cached pinned source: {cached_source}"
    shutil.copytree(
        cached_source,
        fixture_root / ".cache/model-gateway-js",
        ignore=shutil.ignore_patterns("node_modules"),
    )
    return fixture_root, head


def _reset_fixture_modules(fixture_root: Path) -> None:
    """Remove installed node_modules so pinned-source verifies the clean tree offline."""
    modules = (
        fixture_root / ".cache/model-gateway-js/plugins/model-gateway/node_modules"
    )
    if modules.exists():
        shutil.rmtree(modules)


def _check_archive_controls(
    root: Path, work: Path, fixture_root: Path, head: str
) -> None:
    """Verify check-archive accepts clean commits and refuses gitlinks and export attrs."""
    transport = root / "hack/conformance-transport.py"
    clean_env = _git_env(work)
    ok = subprocess.run(
        [
            sys.executable,
            "-I",
            str(transport),
            "check-archive",
            str(fixture_root),
            head,
        ],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert ok.returncode == 0, ok

    attr_repo = work / "archive-attr-repo"
    attr_repo.mkdir()
    subprocess.check_call(["git", "-C", str(attr_repo), "init", "-q"], env=clean_env)
    (attr_repo / "a.txt").write_text("hello\n", encoding="utf-8")
    (attr_repo / ".gitattributes").write_text("*.txt export-ignore\n", encoding="utf-8")
    commit_ignore = _git_commit_all(attr_repo, clean_env, "export-ignore")
    res_ignore = subprocess.run(
        [
            sys.executable,
            "-I",
            str(transport),
            "check-archive",
            str(attr_repo),
            commit_ignore,
        ],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert res_ignore.returncode == 3 and "export-ignore" in res_ignore.stderr, (
        res_ignore
    )

    (attr_repo / ".gitattributes").write_text("*.txt export-subst\n", encoding="utf-8")
    commit_subst = _git_commit_all(attr_repo, clean_env, "export-subst")
    res_subst = subprocess.run(
        [
            sys.executable,
            "-I",
            str(transport),
            "check-archive",
            str(attr_repo),
            commit_subst,
        ],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert res_subst.returncode == 3 and "export-subst" in res_subst.stderr, res_subst

    subprocess.check_call(
        ["git", "-C", str(attr_repo), "rm", "-q", ".gitattributes"], env=clean_env
    )
    subprocess.check_call(
        [
            "git",
            "-C",
            str(attr_repo),
            "update-index",
            "--add",
            "--cacheinfo",
            f"160000,{commit_ignore},submod",
        ],
        env=clean_env,
    )
    subprocess.check_call(
        [
            "git",
            "-C",
            str(attr_repo),
            "-c",
            "user.name=test",
            "-c",
            "user.email=test@example.invalid",
            "commit",
            "-q",
            "-m",
            "gitlink",
        ],
        env=clean_env,
    )
    commit_gitlink = subprocess.check_output(
        ["git", "-C", str(attr_repo), "rev-parse", "HEAD"], text=True, env=clean_env
    ).strip()
    res_gitlink = subprocess.run(
        [
            sys.executable,
            "-I",
            str(transport),
            "check-archive",
            str(attr_repo),
            commit_gitlink,
        ],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert res_gitlink.returncode == 3 and "gitlink" in res_gitlink.stderr, res_gitlink
    print(
        "# archive safety: clean commit accepted; export-ignore, export-subst and gitlink refused"
    )


def main(root: Path, work: Path, full: bool) -> None:
    """Check inventory derivation and run all export controls through the real producer."""
    spec = importlib.util.spec_from_file_location(
        "owner", root / "hack/conformance-owner.py"
    )
    assert spec is not None and spec.loader is not None
    owner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(owner)
    checks = TestCase()

    if full:
        obs = json.loads(
            (work / "identity-observations.json").read_text(encoding="utf-8")
        )
        class_rows = owner._read_raw_tsv(
            root / "test/conformance/classification.tsv",
            ("path", "line", "column", "name", "class", "phase"),
        )
        summary_parts: list[str] = []
        for phase in ("phase-2", "phase-3", "phase-4", "phase-5"):
            target_num = owner._phase_number(phase)
            exp_cases, exp_counts, source_pin = owner.derive_phase_expectations(
                root, phase
            )
            assert source_pin == obs["source_digest"]
            phase_keys = {
                f"{r['path']}\t{r['line']}\t{r['column']}\t{r['name']}"
                for r in class_rows
                if r["class"] == "conformance"
                and owner._phase_number(r["phase"]) <= target_num
            }
            live_cases = {k: obs["cases"][k] for k in phase_keys}
            live_counts = {k: obs["counts"][k] for k in phase_keys}
            assert exp_cases == live_cases, f"case mismatch for {phase}"
            assert exp_counts == live_counts, f"count mismatch for {phase}"
            summary_parts.append(f"{phase}={len(exp_cases)}/{sum(exp_counts.values())}")
        print(
            "# full identity observations match inventory derivation: "
            + " ".join(summary_parts)
        )

    fixture_root, head = _build_fixture_repo(root, work)
    _check_archive_controls(root, work, fixture_root, head)

    standin = str(work / "standin")
    digest = hashlib.sha256(Path(standin).read_bytes()).hexdigest()
    fake_home = work / "fake-hosted-home"
    fake_home.mkdir()
    attest = fake_home / ".model-gateway-test-venue"
    attest.write_text("2026-11-26T00:00:00Z\trunner\tci\n", encoding="utf-8")

    hosted_id = "gha-100-1"
    bundle = work / "fixture-export"
    base_hosted_env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": str(work),
        "TMPDIR": os.environ.get("TMPDIR", "/tmp"),
        "npm_config_cache": os.environ.get("npm_config_cache", str(work / "npm-cache")),
        "GITHUB_ACTIONS": "true",
        "RUNNER_ENVIRONMENT": "github-hosted",
        "GITHUB_RUN_ID": "100",
        "GITHUB_RUN_ATTEMPT": "1",
        "GITHUB_SHA": head,
        "TARGET": f"binary:{standin}",
        "PHASE": "phase-2",
        "EXPORT_DIR": str(bundle),
    }

    # Positive producer run: goes through real hosted_export -> conformance -> run.cjs ->
    # real candidate writer -> prepare_export (final filenames) -> verify_export -> publish_export.
    _reset_fixture_modules(fixture_root)
    assert owner.hosted_export(fixture_root, base_hosted_env, home=fake_home) == 0
    assert {p.name for p in bundle.iterdir()} == {
        "manifest.json",
        "census.tsv",
        "launches.tsv",
    }
    manifest = json.loads((bundle / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["phase"] == "phase-2"
    assert manifest["selection"] == "conformance<=phase-2"
    assert manifest["binary"] == standin
    assert manifest["binary_sha256"] == digest

    clean_env = {"PATH": os.environ.get("PATH", ""), "HOME": str(work)}
    verify_ok = subprocess.run(
        [str(fixture_root / "hack/conformance-export"), "verify", str(bundle)],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert verify_ok.returncode == 0, verify_ok
    verify_hosted_ok = subprocess.run(
        [str(fixture_root / "hack/conformance-export"), "verify", str(bundle)],
        capture_output=True,
        text=True,
        env=base_hosted_env,
        check=False,
    )
    assert verify_hosted_ok.returncode == 0, verify_hosted_ok

    # Real repo inventory (177 phase-2 cases) must refuse the 1-case fixture bundle.
    with checks.assertRaisesRegex(
        ValueError, r"missing 177 case\(s\).*classification\.tsv"
    ):
        owner.verify_export(bundle, root=root, require_git_checkout=False, env={})

    # Dirty verification checkout must be refused by the CLI verifier.
    dirty_marker = fixture_root / "untracked.txt"
    dirty_marker.write_text("dirty\n", encoding="utf-8")
    verify_dirty = subprocess.run(
        [str(fixture_root / "hack/conformance-export"), "verify", str(bundle)],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    dirty_marker.unlink()
    assert verify_dirty.returncode == 70 and "not clean" in verify_dirty.stderr, (
        verify_dirty
    )
    print(
        "# dispatch fixture: real producer exported, verified against fixture checkout, and refused against full inventory"
    )

    # Control 1: failed-run (through real hosted_export with a failing binary and
    # venue refusal, plus export_target status classification).
    fail_bin = work / "fail-bin"
    fail_bin.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
    fail_bin.chmod(0o700)
    failed_out = work / "failed-export"
    _reset_fixture_modules(fixture_root)
    failed_err = io.StringIO()
    with contextlib.redirect_stderr(failed_err):
        rc_failed = owner.hosted_export(
            fixture_root,
            {
                **base_hosted_env,
                "TARGET": f"binary:{fail_bin}",
                "EXPORT_DIR": str(failed_out),
            },
            home=fake_home,
        )
    assert rc_failed == 70
    assert (
        "conformance export refused: comparison status 1 is not a pass"
        in failed_err.getvalue()
    ), failed_err.getvalue()
    assert not failed_out.exists()

    claude_dir = work / "claude-path"
    claude_dir.mkdir()
    fake_claude = claude_dir / "claude"
    fake_claude.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    fake_claude.chmod(0o700)
    venue_refused_out = work / "venue-refused-export"
    venue_err = io.StringIO()
    with contextlib.redirect_stderr(venue_err):
        rc_venue = owner.hosted_export(
            fixture_root,
            {
                **base_hosted_env,
                "PATH": f"{claude_dir}:{base_hosted_env['PATH']}",
                "EXPORT_DIR": str(venue_refused_out),
            },
            home=fake_home,
        )
    assert rc_venue == 70
    assert (
        "conformance export refused: venue refusal status 97 is not a pass"
        in venue_err.getvalue()
    ), venue_err.getvalue()
    assert not venue_refused_out.exists()

    # Preflight executable-path consistency: canonical path with space refused before suite.
    space_dir = work / "dir with space"
    space_dir.mkdir()
    space_bin = space_dir / "standin"
    shutil.copy2(standin, space_bin)
    space_out = work / "space-export"
    assert (
        owner.hosted_export(
            fixture_root,
            {
                **base_hosted_env,
                "TARGET": f"binary:{space_bin}",
                "EXPORT_DIR": str(space_out),
            },
            home=fake_home,
        )
        == 70
    )
    assert not space_out.exists()

    venue_args = ["conformance", f"TARGET=binary:{standin}", "PHASE=phase-2"]
    assert owner.export_target(venue_args, 0, head) == (standin, "phase-2")
    for status_code, label in [
        (1, "comparison"),
        (70, "infrastructure"),
        (97, "venue refusal"),
        (124, "timeout"),
    ]:
        with checks.assertRaisesRegex(
            ValueError, f"{label} status {status_code} is not a pass"
        ):
            owner.export_target(venue_args, status_code, head)
    for options, revision, reason in [
        (["conformance", "PHASE=phase-2"], head, "source-runtime"),
        (["conformance", f"TARGET=binary:{standin}"], head, "missing gate PHASE"),
        (venue_args + ["MODE=probes"], head, "partial selection"),
        (venue_args + ["PACKAGE=internal/adapter"], head, "partial selection"),
        (venue_args + ["GATE=task"], head, "partial selection"),
        (venue_args, "dirty", "clean commit"),
        (
            ["conformance", f"TARGET=binary:{space_bin}", "PHASE=phase-2"],
            head,
            "invalid binary path",
        ),
    ]:
        with checks.assertRaisesRegex(ValueError, reason):
            owner.export_target(options, 0, revision)
    print("# export control failed-run: pass")

    def run_cli_verify(
        target_dir: Path, env: dict[str, str] | None = None
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(fixture_root / "hack/conformance-export"), "verify", str(target_dir)],
            capture_output=True,
            text=True,
            env=clean_env if env is None else env,
            check=False,
        )

    # Control 2: missing-launch.
    launches_path = bundle / "launches.tsv"
    orig_launches = launches_path.read_text(encoding="utf-8")
    launches_path.write_text(
        "".join(orig_launches.splitlines(keepends=True)[:-1]), encoding="utf-8"
    )
    res = run_cli_verify(bundle)
    launches_path.write_text(orig_launches, encoding="utf-8")
    assert (
        res.returncode == 70
        and "expected 2" in res.stderr
        and "observed 1" in res.stderr
        and "test/conformance/launch-sites.tsv" in res.stderr
    ), res
    print("# export control missing-launch: pass")

    # Control 3: changed-commit (both CLI HEAD mismatch and TSV header mismatch).
    manifest_path = bundle / "manifest.json"
    orig_manifest = manifest_path.read_text(encoding="utf-8")
    manifest_path.write_text(
        json.dumps({**manifest, "commit": "1" * 40}) + "\n", encoding="utf-8"
    )
    res = run_cli_verify(bundle)
    assert res.returncode == 70 and "does not match expected commit" in res.stderr, res
    with checks.assertRaisesRegex(ValueError, "commit mismatch"):
        owner.verify_export(
            bundle, root=fixture_root, require_git_checkout=False, env={}
        )
    manifest_path.write_text(orig_manifest, encoding="utf-8")
    print("# export control changed-commit: pass")

    # Control 4: missing-case (remove case from both census and launches so no
    # launch-outside-census check masks the missing-case check).
    census_path = bundle / "census.tsv"
    orig_census = census_path.read_text(encoding="utf-8")
    census_path.write_text(
        "".join(orig_census.splitlines(keepends=True)[:-1]), encoding="utf-8"
    )
    launches_path.write_text(
        "".join(orig_launches.splitlines(keepends=True)[:2]), encoding="utf-8"
    )
    res = run_cli_verify(bundle)
    census_path.write_text(orig_census, encoding="utf-8")
    launches_path.write_text(orig_launches, encoding="utf-8")
    assert (
        res.returncode == 70
        and "missing 1 case(s)" in res.stderr
        and "test/conformance/classification.tsv" in res.stderr
    ), res
    print("# export control missing-case: pass")

    # Control 5: mixed-digest.
    launches_path.write_text(
        orig_launches.replace(digest, "f" * 64, 1), encoding="utf-8"
    )
    res = run_cli_verify(bundle)
    launches_path.write_text(orig_launches, encoding="utf-8")
    assert res.returncode == 70 and "mixed binary" in res.stderr, res
    print("# export control mixed-digest: pass")

    # Control 6: producer-id-mismatch (hosted & venue env, cross-producer, identity resolver).
    wrong_hosted_env = {**base_hosted_env, "GITHUB_RUN_ID": "999"}
    cli_mismatch = run_cli_verify(bundle, env=wrong_hosted_env)
    assert (
        cli_mismatch.returncode == 70 and "hosted environment" in cli_mismatch.stderr
    ), cli_mismatch

    venue_unit = "mgw-run-1-1.scope"
    venue_id = owner.venue_run_id(venue_unit)
    stamp = datetime.now(UTC).isoformat()
    venue_stage = work / ".pending-venue-control"
    venue_stage.mkdir()
    venue_out = work / "venue-control-bundle"
    owner.prepare_export(
        venue_stage,
        {
            "producer": "venue",
            "run_id": venue_id,
            "commit": head,
            "clean_tree": True,
            "phase": "phase-2",
            "binary": standin,
            "binary_sha256": digest,
            "test_source_sha256": manifest["test_source_sha256"],
            "selection": "conformance<=phase-2",
            "started": stamp,
            "ended": stamp,
            "verdict": "pass",
            "status": 0,
        },
        {
            "census": "".join(orig_census.splitlines(keepends=True)[1:]),
            "launches": "".join(orig_launches.splitlines(keepends=True)[1:]),
        },
        root=fixture_root,
        require_git_checkout=True,
        expected_producer="venue",
        expected_run_id=venue_id,
        expected_commit=head,
        expected_phase="phase-2",
        env={"CONFORMANCE_VENUE_UNIT": venue_unit, "CONFORMANCE_VENUE_COMMIT": head},
    )
    owner.publish_export(venue_stage, venue_out)
    venue_ok = run_cli_verify(
        venue_out,
        env={
            **clean_env,
            "CONFORMANCE_VENUE_UNIT": venue_unit,
            "CONFORMANCE_VENUE_COMMIT": head,
        },
    )
    assert venue_ok.returncode == 0, venue_ok
    venue_bad = run_cli_verify(
        venue_out,
        env={**clean_env, "CONFORMANCE_VENUE_UNIT": "mgw-run-999-1.scope"},
    )
    assert venue_bad.returncode == 70 and "venue scope" in venue_bad.stderr, venue_bad
    cross_hosted = run_cli_verify(venue_out, env=base_hosted_env)
    assert (
        cross_hosted.returncode == 70 and "hosted environment" in cross_hosted.stderr
    ), cross_hosted
    cross_venue = run_cli_verify(
        bundle, env={**clean_env, "CONFORMANCE_VENUE_UNIT": venue_unit}
    )
    assert cross_venue.returncode == 70 and "venue environment" in cross_venue.stderr, (
        cross_venue
    )
    for bad_producer, bad_id in [("hosted", venue_id), ("venue", hosted_id)]:
        with checks.assertRaisesRegex(ValueError, "invalid run identity"):
            owner.check_producer_run_id(bad_producer, bad_id)

    assert owner.resolve_hosted_identity(
        fixture_root, base_hosted_env, home=fake_home
    ) == ("hosted", hosted_id, head)
    with checks.assertRaisesRegex(ValueError, "GITHUB_SHA"):
        owner.resolve_hosted_identity(
            fixture_root, {**base_hosted_env, "GITHUB_SHA": "1" * 40}, home=fake_home
        )
    attest.write_text("2026-11-26T00:00:00Z\tvenue\tprovision\n", encoding="utf-8")
    with checks.assertRaisesRegex(ValueError, "hosted ci attestation"):
        owner.resolve_hosted_identity(fixture_root, base_hosted_env, home=fake_home)
    attest.write_text("2026-11-26T00:00:00Z\trunner\tci\n", encoding="utf-8")
    dirty_marker.write_text("dirty\n", encoding="utf-8")
    with checks.assertRaisesRegex(ValueError, "not clean"):
        owner.resolve_hosted_identity(fixture_root, base_hosted_env, home=fake_home)
    dirty_marker.unlink()
    print("# export control producer-id-mismatch: pass")

    # Control 7: publication-failure triggers a real rename failure inside hosted_export
    # after the staging directory is created and verified, removes the staging directory,
    # leaves nothing under the final name, and allows retry into the same path.
    pub_parent = work / "pub-export-parent"
    pub_parent.mkdir(mode=0o700)
    pub_target = pub_parent / "published"
    orig_publish = owner.publish_export
    seen_stages: list[Path] = []

    def fail_rename(stage_dir: Path, dest: Path) -> None:
        seen_stages.append(stage_dir)
        assert stage_dir.is_dir() and stage_dir.parent == pub_parent
        assert (stage_dir / "manifest.json").is_file()
        pub_parent.chmod(0o500)
        try:
            orig_publish(stage_dir, dest)
        finally:
            pub_parent.chmod(0o700)

    pub_err = io.StringIO()
    try:
        _reset_fixture_modules(fixture_root)
        with (
            patch.object(owner, "publish_export", fail_rename),
            contextlib.redirect_stderr(pub_err),
        ):
            rc_pub = owner.hosted_export(
                fixture_root,
                {**base_hosted_env, "EXPORT_DIR": str(pub_target)},
                home=fake_home,
            )
    finally:
        pub_parent.chmod(0o700)
    assert rc_pub == 70, pub_err.getvalue()
    assert "conformance export refused:" in pub_err.getvalue(), pub_err.getvalue()
    assert len(seen_stages) == 1 and not seen_stages[0].exists()
    assert not pub_target.exists() and not pub_target.is_symlink()
    assert list(pub_parent.iterdir()) == []

    # Retry into the exact same final path succeeds once publish_export is restored.
    _reset_fixture_modules(fixture_root)
    assert (
        owner.hosted_export(
            fixture_root,
            {**base_hosted_env, "EXPORT_DIR": str(pub_target)},
            home=fake_home,
        )
        == 0
    )
    retry_ok = run_cli_verify(pub_target)
    assert retry_ok.returncode == 0, retry_ok
    print("# export control publication-failure: pass")

    # Control 8: capture-write-failure yields infrastructure status 70 while keeping
    # the comparison pass result separate and visible.
    blocked_candidate = work / "blocked-candidate.json"
    blocked_candidate.write_text("keep\n", encoding="utf-8")
    _reset_fixture_modules(fixture_root)
    capture_env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": str(work),
        "TMPDIR": os.environ.get("TMPDIR", "/tmp"),
        "npm_config_cache": os.environ.get("npm_config_cache", str(work / "npm-cache")),
        "TARGET": f"binary:{standin}",
        "MODE": "suite",
        "PHASE": "phase-2",
        "CONFORMANCE_CAPTURE": standin,
        "CONFORMANCE_CANDIDATE": str(blocked_candidate),
    }
    cap_proc = subprocess.run(
        [str(fixture_root / "hack/conformance")],
        capture_output=True,
        text=True,
        env=capture_env,
        check=False,
    )
    assert cap_proc.returncode == 70, cap_proc
    combined_cap = cap_proc.stdout + cap_proc.stderr
    assert '"ok":true' in combined_cap, cap_proc
    assert "candidate capture failed" in combined_cap, cap_proc
    assert blocked_candidate.read_text(encoding="utf-8") == "keep\n"
    print("# export control capture-write-failure: pass")

    # Control 9: duplicate-metadata in census.tsv, launches.tsv, and manifest.json refused.
    dup_header = (
        f'{{"run_id":"{hosted_id}","run_id":"{hosted_id}","commit":"{head}"}}\n'
    )
    census_path.write_text(
        dup_header + "".join(orig_census.splitlines(keepends=True)[1:]),
        encoding="utf-8",
    )
    res_dup_census = run_cli_verify(bundle)
    census_path.write_text(orig_census, encoding="utf-8")
    assert (
        res_dup_census.returncode == 70
        and "duplicate JSON member name" in res_dup_census.stderr
    ), res_dup_census

    launches_path.write_text(
        dup_header + "".join(orig_launches.splitlines(keepends=True)[1:]),
        encoding="utf-8",
    )
    res_dup_launches = run_cli_verify(bundle)
    launches_path.write_text(orig_launches, encoding="utf-8")
    assert (
        res_dup_launches.returncode == 70
        and "duplicate JSON member name" in res_dup_launches.stderr
    ), res_dup_launches

    dup_manifest = orig_manifest.replace(
        '"producer": "hosted"', '"producer": "hosted", "producer": "hosted"', 1
    )
    assert dup_manifest != orig_manifest
    manifest_path.write_text(dup_manifest, encoding="utf-8")
    res_dup_manifest = run_cli_verify(bundle)
    manifest_path.write_text(orig_manifest, encoding="utf-8")
    assert (
        res_dup_manifest.returncode == 70
        and "duplicate JSON member name" in res_dup_manifest.stderr
    ), res_dup_manifest
    print("# export control duplicate-metadata: pass")

    # Control 10: subdirectory-root refuses an ignored subdirectory of a clean checkout
    # even when it contains matching inventory files.
    spoof_root = fixture_root / ".cache/spoof-root"
    shutil.copytree(fixture_root / "test", spoof_root / "test")
    try:
        res_subdir = subprocess.run(
            [
                sys.executable,
                "-I",
                str(fixture_root / "hack/conformance-owner.py"),
                "verify",
                str(spoof_root),
                str(bundle),
            ],
            capture_output=True,
            text=True,
            env=clean_env,
            check=False,
        )
        assert (
            res_subdir.returncode == 70
            and "not the Git repository top level" in res_subdir.stderr
        ), res_subdir
        with checks.assertRaisesRegex(ValueError, "not the Git repository top level"):
            owner.resolve_hosted_identity(spoof_root, base_hosted_env, home=fake_home)
    finally:
        shutil.rmtree(spoof_root, ignore_errors=True)
    print("# export control subdirectory-root: pass")

    # Control 11: git-config-override refuses an untracked file even when inherited
    # Git config or XDG excludes attempt to hide it, and ignores external attributes.
    hide_all = work / "hide-all.ignore"
    hide_all.write_text("*\n", encoding="utf-8")
    fake_gitconfig = work / "fake.gitconfig"
    fake_gitconfig.write_text(
        f"[core]\n\texcludesFile = {hide_all}\n", encoding="utf-8"
    )
    xdg_dir = work / "xdg-config"
    (xdg_dir / "git").mkdir(parents=True)
    (xdg_dir / "git/ignore").write_text("*\n", encoding="utf-8")
    hide_attrs = work / "hide-attrs"
    hide_attrs.write_text("* export-ignore\n", encoding="utf-8")

    overrides = [
        {
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.excludesFile",
            "GIT_CONFIG_VALUE_0": str(hide_all),
        },
        {"GIT_CONFIG_GLOBAL": str(fake_gitconfig)},
        {"GIT_CONFIG_PARAMETERS": f"'core.excludesFile={hide_all}'"},
        {"XDG_CONFIG_HOME": str(xdg_dir)},
    ]
    dirty_marker.write_text("dirty\n", encoding="utf-8")
    try:
        for override in overrides:
            res_override = run_cli_verify(bundle, env={**clean_env, **override})
            assert (
                res_override.returncode == 70 and "not clean" in res_override.stderr
            ), (override, res_override)
            res_remote_git = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1" && clean_git "$2" status --porcelain=v1 --untracked-files=all --ignore-submodules=none',
                    "bash",
                    str(root / "hack/remote"),
                    str(fixture_root),
                ],
                capture_output=True,
                text=True,
                env={**clean_env, **override},
                check=False,
            )
            assert (
                res_remote_git.returncode == 0
                and "untracked.txt" in res_remote_git.stdout
            ), (override, res_remote_git)
        with checks.assertRaisesRegex(ValueError, "not clean"):
            owner.resolve_hosted_identity(
                fixture_root, {**base_hosted_env, **overrides[0]}, home=fake_home
            )
    finally:
        dirty_marker.unlink(missing_ok=True)

    res_attr_override = subprocess.run(
        [
            sys.executable,
            "-I",
            str(root / "hack/conformance-transport.py"),
            "check-archive",
            str(fixture_root),
            head,
        ],
        capture_output=True,
        text=True,
        env={
            **clean_env,
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.attributesFile",
            "GIT_CONFIG_VALUE_0": str(hide_attrs),
        },
        check=False,
    )
    assert res_attr_override.returncode == 0, res_attr_override
    print("# export control git-config-override: pass")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3] == "full")
