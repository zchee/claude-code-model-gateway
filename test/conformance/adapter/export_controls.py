# Copyright 2026 The claude-code-model-gateway Authors.
# SPDX-License-Identifier: Apache-2.0
"""Exercise the export producer and verifier with the real dispatch fixture."""

import hashlib
import importlib.util
import json
import os
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path
from unittest import TestCase


def main(root: Path, work: Path, full: bool) -> None:
    """Check the complete identity profile and mutate one stand-in input at a time."""
    spec = importlib.util.spec_from_file_location(
        "owner", root / "hack/conformance-owner.py"
    )
    assert spec is not None and spec.loader is not None
    owner = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(owner)
    if full:
        assert json.loads((work / "identity-profile.json").read_text()) == [
            owner.CASE_PROFILE,
            owner.COUNT_PROFILE,
        ]
        print(
            "# full identity census and credited launch counts match offline verifier pins"
        )

    image = str(work / "standin")
    args = ["conformance", f"TARGET=binary:{image}"]
    commit = "0" * 40
    checks = TestCase()
    assert owner.export_target(args, 0, commit) == image
    with checks.assertRaisesRegex(ValueError, "not a pass"):
        owner.export_target(args, 1, commit)
    print("# export control failed-run: pass")
    for options, revision, reason in [
        (["conformance"], commit, "source-runtime"),
        (args + ["MODE=probes"], commit, "partial selection"),
        (args + ["PHASE=phase-2"], commit, "partial selection"),
        (args, "dirty", "clean commit"),
    ]:
        with checks.assertRaisesRegex(ValueError, reason):
            owner.export_target(options, 0, revision)

    events = [
        json.loads(line)
        for line in (work / "standin-events.jsonl").read_text().splitlines()
    ]
    (event,) = [e for e in events if e["type"] == "test:pass"]
    assert events[-1]["type"] == "test:summary" and events[-1]["success"]
    file = "test/conformance/adapter/dispatch.test.js"
    assert event["file"].endswith(file)
    loc = "\t".join((file, str(event["line"]), str(event["column"])))
    case = f"{loc}\t{event['nesting']}\t{event['name']}"
    key = f"{loc}\t{event['name']}"
    digest = hashlib.sha256(Path(image).read_bytes()).hexdigest()
    candidate = {
        "census": f"{owner.CENSUS}\n{case}\tpass\t-\n",
        "launches": (work / "standin-table").read_text(),
    }
    stamp = datetime.now(UTC).isoformat()
    venue_unit = "mgw-run-1-1.scope"
    venue_id = owner.venue_run_id(venue_unit)
    manifest = {
        "producer": "venue",
        "run_id": venue_id,
        "commit": commit,
        "clean_tree": True,
        "binary": image,
        "binary_sha256": digest,
        "test_source_sha256": owner.SOURCE_PIN,
        "selection": "all-conformance",
        "started": stamp,
        "ended": stamp,
        "verdict": "pass",
        "status": 0,
    }
    # Only this test process knows the dispatch fixture's profile. The public verifier must reject it.
    vars(owner).update(
        CASE_PROFILE=owner.profile([case]), COUNT_PROFILE=owner.profile([key + "\t2"])
    )
    bundle = work / "dispatch-bundle"
    bundle.mkdir()
    owner.prepare_export(
        bundle,
        manifest,
        candidate,
        expected_producer="venue",
        expected_run_id=venue_id,
        expected_commit=commit,
        env={"CONFORMANCE_VENUE_UNIT": venue_unit, "CONFORMANCE_VENUE_COMMIT": commit},
    )
    (bundle / "manifest.pending").rename(bundle / "manifest.json")
    owner.verify_export(bundle, env={})
    clean_env = {"PATH": os.environ.get("PATH", "")}
    check = subprocess.run(
        [root / "hack/conformance-export", "verify", bundle],
        capture_output=True,
        text=True,
        env=clean_env,
        check=False,
    )
    assert check.returncode == 70 and "full selection" in check.stderr, check
    print(
        "# dispatch fixture: one passing registration, two measured binary launches; public verifier refused fixture"
    )

    for name, filename, reason in [
        ("missing-launch", "launches.tsv", "per-case launch count"),
        ("changed-commit", "manifest.json", "commit mismatch"),
        ("missing-case", "census.tsv", "full selection"),
        ("mixed-digest", "launches.tsv", "mixed binary"),
    ]:
        path = bundle / filename
        original = path.read_text()
        if name == "changed-commit":
            altered = json.dumps({**manifest, "commit": "1" * 40})
        elif name == "mixed-digest":
            altered = original.replace(digest, "f" * 64, 1)
        else:
            altered = "".join(original.splitlines(keepends=True)[:-1])
        path.write_text(altered)
        with checks.assertRaisesRegex(ValueError, reason):
            owner.verify_export(bundle, env={})
        path.write_text(original)
        print(f"# export control {name}: pass")

    # Control: manifest identifier must match the active producer and environment.
    hosted_id = "gha-100-1"
    hosted_manifest = {
        **manifest,
        "producer": "hosted",
        "run_id": hosted_id,
    }
    hosted_env = {
        "PATH": os.environ.get("PATH", ""),
        "GITHUB_ACTIONS": "true",
        "RUNNER_ENVIRONMENT": "github-hosted",
        "GITHUB_RUN_ID": "100",
        "GITHUB_RUN_ATTEMPT": "1",
        "GITHUB_SHA": commit,
    }
    hosted_bundle = work / "hosted-dispatch-bundle"
    hosted_bundle.mkdir()
    owner.prepare_export(
        hosted_bundle,
        hosted_manifest,
        candidate,
        expected_producer="hosted",
        expected_run_id=hosted_id,
        expected_commit=commit,
        env=hosted_env,
    )
    (hosted_bundle / "manifest.pending").rename(hosted_bundle / "manifest.json")
    owner.verify_export(hosted_bundle, env=hosted_env)

    # Mismatched hosted run id refused in-process and through the CLI wrapper.
    wrong_hosted_env = {**hosted_env, "GITHUB_RUN_ID": "999"}
    with checks.assertRaisesRegex(ValueError, "hosted environment"):
        owner.verify_export(hosted_bundle, env=wrong_hosted_env)
    cli_mismatch = subprocess.run(
        [root / "hack/conformance-export", "verify", hosted_bundle],
        capture_output=True,
        text=True,
        env=wrong_hosted_env,
        check=False,
    )
    assert (
        cli_mismatch.returncode == 70 and "hosted environment" in cli_mismatch.stderr
    ), cli_mismatch

    # Cross-producer mismatches and wrong venue unit refused.
    with checks.assertRaisesRegex(ValueError, "hosted environment"):
        owner.verify_export(bundle, env=hosted_env)
    with checks.assertRaisesRegex(ValueError, "venue scope"):
        owner.verify_export(
            bundle, env={"CONFORMANCE_VENUE_UNIT": "mgw-run-999-1.scope"}
        )
    with checks.assertRaisesRegex(ValueError, "venue environment"):
        owner.verify_export(hosted_bundle, env={"CONFORMANCE_VENUE_UNIT": venue_unit})
    for bad_producer, bad_id in [("hosted", venue_id), ("venue", hosted_id)]:
        with checks.assertRaisesRegex(ValueError, "invalid run identity"):
            owner.check_producer_run_id(bad_producer, bad_id)

    # Hosted identity resolver enforces ci attestation, HEAD == GITHUB_SHA, and clean tree.
    fake_repo = work / "fake-hosted-repo"
    fake_repo.mkdir()
    git_env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": str(work),
        "GIT_CONFIG_NOSYSTEM": "1",
    }
    subprocess.check_call(["git", "-C", str(fake_repo), "init", "-q"], env=git_env)
    (fake_repo / "file.txt").write_text("ok\n", encoding="utf-8")
    subprocess.check_call(["git", "-C", str(fake_repo), "add", "file.txt"], env=git_env)
    subprocess.check_call(
        [
            "git",
            "-C",
            str(fake_repo),
            "-c",
            "user.name=test",
            "-c",
            "user.email=test@example.invalid",
            "commit",
            "-q",
            "-m",
            "init",
        ],
        env=git_env,
    )
    head = subprocess.check_output(
        ["git", "-C", str(fake_repo), "rev-parse", "HEAD"], text=True, env=git_env
    ).strip()
    fake_home = work / "fake-hosted-home"
    fake_home.mkdir()
    attest = fake_home / ".model-gateway-test-venue"
    attest.write_text("2026-11-26T00:00:00Z\trunner\tci\n", encoding="utf-8")
    real_hosted_env = {**hosted_env, "GITHUB_SHA": head}
    assert owner.resolve_hosted_identity(
        fake_repo, real_hosted_env, home=fake_home
    ) == (
        "hosted",
        hosted_id,
        head,
    )
    with checks.assertRaisesRegex(ValueError, "GITHUB_SHA"):
        owner.resolve_hosted_identity(
            fake_repo, {**real_hosted_env, "GITHUB_SHA": "1" * 40}, home=fake_home
        )
    attest.write_text("2026-11-26T00:00:00Z\tvenue\tprovision\n", encoding="utf-8")
    with checks.assertRaisesRegex(ValueError, "hosted ci attestation"):
        owner.resolve_hosted_identity(fake_repo, real_hosted_env, home=fake_home)
    attest.write_text("2026-11-26T00:00:00Z\trunner\tci\n", encoding="utf-8")
    (fake_repo / "dirty.txt").write_text("dirty\n", encoding="utf-8")
    with checks.assertRaisesRegex(ValueError, "not clean"):
        owner.resolve_hosted_identity(fake_repo, real_hosted_env, home=fake_home)
    print("# export control producer-id-mismatch: pass")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3] == "full")
