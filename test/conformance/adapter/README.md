# Running the launch adapter

## What a verdict means

**Attribution.** In a passing binary run of the pinned source tests, each selected product launch at an inventoried site ran the selected executable, and each site ran the expected number of times. Missing executions, recognized but unattributed launches, and source-runtime records in a binary run fail the comparison. This proof does not cover a shell starting the runtime, a worker thread, a copied runtime binary, or process-image replacement. The reviewed test-source fingerprint must match before the suite starts; re-pinning requires checking the launch forms and counts again. A copied runtime used as a fixture proxy is not a product launch.

**Venue hygiene.** Before allowing another run on the dedicated venue, the owner must verify that the prior run has no live processes (and therefore no listeners). A reservation on disk prevents a new run even if the kernel lock is free. If the outside owner dies or its watchdog expires, the reservation stays. The refusal names the reservation. Recovery is an explicit `hack/remote provision` or `deprovision` that verifies termination before clearing it.

**Verdicts through `hack/remote run conformance`.** `0` means pass, `1` means comparison or test failure, `2` means invalid request, and `124` means timeout. Other statuses are not verdicts: `70` means an infrastructure error, `97` a venue refusal, `255` a transport failure or watchdog kill of the remote command, and `128+signal` an interruption. Failure to establish cleanup is not evidence of a product mismatch.

**Non-goals.** The adapter does not defend against tests deliberately evading interception or automatically recover a dead outside owner. Standalone containment is best effort: a process group cannot contain detached children, and `SIGTERM` sent only to the shell wrapper is not forwarded to the running supervisor. Dispose of the runner after use; an in-group timeout control does not establish detached-child cleanup.

## Invocation

From the worktree root, run `VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run conformance`. Without selectors the default `TARGET=identity` compares the entire pinned suite to the baseline, by file, registration line and column, full name, and outcome. It never edits the source tests or harness.

Append these make arguments to the same venue invocation:

| Argument | Effect |
| --- | --- |
| `MODE=probes` | Run the four required launch probes plus the complete pinned orderly-SIGTERM test. All four named probes and the pinned case must pass with attributed launches of the expected role. |
| `MODE=self-test` | Run identity with a native recording binary at each observed gateway dispatch. It checks the translated argv and fixture environment, then the real Node gateway handles the test so subsequent launches remain reachable. Raw environment values never enter the launch log. Also runs the adapter and containment negative controls. |
| `TARGET=binary:/absolute/venue/path` | Run the executable itself in place of each gateway launch. The path is on the venue, not the sending machine. Without selectors this runs all conformance cases. |
| `SITE=test/file.test.js:line` | Limit an identity diagnostic (including `MODE=self-test`) to one pinned registration site; cannot be used as binary conformance evidence. |
| `TIMEOUT_SECONDS=…` | Set the job wall-clock limit in seconds (default 200, up to `MAX_TIMEOUT_SECONDS`). On expiry the supervisor lists active test files, stops the job's process group (retaining the leader unreaped via `waitid(WNOWAIT)`), and exits `124` (infrastructure timeout). |
| `MAX_TIMEOUT_SECONDS=…` | Explicitly raise the job timeout ceiling above the 200-second default (up to the venue's 1800-second hard bound) for slower targets or cold runners without editing scripts. |
| `CLASS=conformance` | Run all conformance rows against the chosen target. |
| `PHASE=… PACKAGE=… GATE=…` | Select conformance rows by inventory values. `PHASE=phase-N` selects conformance rows assigned to phases up to and including `N`; `PACKAGE` and `GATE` match exact values. Omit any column to leave it unrestricted. |

## Attribution evidence

The launch inventory defaults to one execution per `(case, site, role)` row. `adapter/repeated-sites.tsv` lists the exceptions. Nested launches credit their parent only through an explicit inventory edge. Passing tests must also have complete, consistent file and suite summaries. A record must match the selected target and digest and identify a successful process launch. Before running the suite, `checkPinnedLaunchForms` scans all JS/CJS/MJS files under the pinned plugin's `test/` for out-of-scope forms (`worker_threads`, `cluster`, `shell:` options, uninventoried `child_process` APIs, and unreviewed `process.execPath` copies) and checks the full tree SHA-256 digest; changing a fixture or re-pinning fails until reviewed.

## Containment and recovery

On the shared Linux venue, `hack/remote` is the single owner of OS containment: it runs **every** `run` invocation (regardless of Make goals or their order) inside one user systemd scope (`RuntimeMaxSec` = `TIMEOUT_SECONDS + 90` when `conformance` is among the goals, `600` seconds otherwise; `KillMode=control-group`) without installing a subreaper, so detached children remain in the scope cgroup while keeping normal `ppid=1` orphan parentage. Before releasing the launcher into `systemd-run`, `hack/remote` persists the scope unit name together with the launcher's PID and kernel start tick to `active-scope` while holding the venue `flock` outside the scope. It clears `active-scope` and releases the lock only after outside verification confirms both that the launcher is dead and that the scope is `inactive` with an empty `cgroup.procs`. If verification fails or the outer lock owner dies abruptly, `hack/remote status` reports the venue unavailable instead of free until `hack/remote provision` stops and verifies the recorded launcher and scope (before running the clean-state and port checks) and recovers it.

The independent outside watchdog covers startup, verification, and pruning, including a stopped owner. It kills the owner's process group, not the reserved scope; the latter has its own systemd deadline. After a watchdog expiry or an unverifiable launcher identity, the next run refuses and names `active-scope`. Recover with `VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote provision`; after a deliberate control, append the printed unit name to require an exact match under the lock. Never remove the marker manually. A passing recovery checks the recorded launcher, scope membership and service state before reuse.

The self-test runs its named controls in the production scope; it creates no sibling units. `hack/remote run check-scope-leftover` leaves a bounded child after a successful payload: expect `70`, not a pass. Two lifecycle controls are separate invocations because they intentionally end their own run: `hack/remote check-cancellation` requires the detached listener's port to stay bound after launcher exit and become free after scope drain (expect `143`, then a free venue); `hack/remote check-owner-deadline` freezes its own outside owner (expect no verdict, then refusal until recovery). Neither is part of a product comparison. On standalone hosts only the process-group check applies.

The owner publishes the final class and status only after verified drain, staging any eligible export, and a best-effort pruning attempt. Among completed comparisons, timeout takes precedence over failure, and failure over pass. A prune error is reported without changing that result; unverified teardown, a failed outcome write, or inability to release `active-scope` returns status `97` with the reservation retained until explicit recovery. `70` denotes an infrastructure error (including a candidate capture or export failure after a passing suite), never a comparison failure.

## Evidence output

The venue returns TAP, one `# launch ` comment per JSON record, and a final summary. The `build` field is the binary's SHA-256 (`null` in identity mode). On an attested runner, `LAUNCH_LOG` may name a new absolute JSONL file outside the fixtures. The remote transport returns the same records as TAP comments because a successful remote workspace is removed. For example, `perl -ne 'print $1 if /^# launch (.*\n)$/' run.tap` extracts the JSONL stream from a captured run.

Raw logs are diagnostics, not results-check inputs. Checker-facing `census.tsv` and `launches.tsv` files are written only as part of a completed, passing, gate-phase (`PHASE=phase-N`) binary conformance export directory alongside `manifest.json` (`producer`, `run_id`, `commit`, `clean_tree`, `phase`, `binary`, `binary_sha256`, `test_source_sha256`, `selection`, `started`, `ended`, `verdict`, `status`). An export serves one gate: `PHASE=phase-N` selects all conformance cases assigned to phases up to and including `N` in the reviewed inventory (`selection: "conformance<=phase-N"`). Narrower selections (`PACKAGE`, `GATE`, `SITE`, non-suite `MODE`) and runs without `PHASE` are refused. Run identifiers and commits always come from the producer environment, never from a caller argument.

**Atomic publication and commit point.** Both producers stage `manifest.json`, `census.tsv`, and `launches.tsv` under their final names inside an unpublished sibling directory, fsync and verify them there, and publish with one same-filesystem directory rename. A failed or interrupted publication before that rename leaves no directory under the final name and does not block a retry. On the dedicated venue, the exact order is: (1) verified scope drain; (2) stage and verify the export; (3) best-effort workspace prune; (4) write the typed outcome (`last-outcome`); (5) rename the export directory into place; (6) release the reservation (`active-scope`). The directory rename is the single commit point. If the owner is killed before the rename, or if writing `last-outcome` fails, no export appears under the final name. If the owner is killed or releasing `active-scope` fails after the rename, the export is valid — because the run passed, was drained, and was verified — and the owner returns status `97` while leaving `active-scope` in place so the next run is refused until explicit recovery (`hack/remote provision` or `deprovision`).

Two producers are supported:

- **Hosted Linux runner (`producer: "hosted"`):** run `TARGET="binary:<abs-path>" PHASE="phase-N" EXPORT_DIR="<abs-dir>" ./hack/conformance-export run` (or `make conformance-export TARGET=... PHASE=... EXPORT_DIR=...`) under standalone process-group containment on a disposable GitHub-hosted runner. Invoke `./hack/conformance-export` directly in workflows and scripts whenever exact exit statuses (`70`, `97`) matter, because GNU Make collapses non-zero recipe failures to status `2`. It derives `run_id = gha-<GITHUB_RUN_ID>-<GITHUB_RUN_ATTEMPT>` and `commit = GITHUB_SHA`, and requires `GITHUB_ACTIONS=true`, `RUNNER_ENVIRONMENT=github-hosted`, the `ci` attestation created by `./hack/venue-attest-ci`, and a clean Git checkout whose `HEAD` equals `GITHUB_SHA` both before and after the suite. Because its run ID matches the Go test JSON from the same workflow attempt, its files can be passed directly to `hack/parity-check results phase-N` with that run's Go test JSON.
- **Dedicated venue (`producer: "venue"`):** from a clean checkout, run `hack/remote run build-conformance-binary` and then `hack/remote run conformance TARGET=binary:/tmp/model-gateway-venue/bin/conformance-model-gateway PHASE=phase-N` with `VENUE_HOST` set. `hack/remote` and the outside owner script (`hack/conformance-owner.py`) run from the working checkout rather than from inside the archived payload; `hack/remote` establishes clean state and `HEAD` before reading any file from the checkout, rejects exports when the commit's tree has gitlinks or active `export-ignore`/`export-subst` attributes (while still allowing diagnostic runs), streams `git archive` of that commit, and re-checks both `HEAD` and clean state after producing the archive (refusing export if either changed). After the gate-phase binary suite passes and scope cleanup is verified, the outside owner derives `run_id = venue-<scope-unit-without-.scope>` (never `gha-...`), stages and verifies `/tmp/model-gateway-venue/exports/<run_id>`, writes `last-outcome`, publishes with one rename, and prints the path. A dedicated-venue export is valid only with Go test JSON acquired under the same venue run ID; it cannot be combined with hosted `gha-...` Go test JSON.

Verify any export directory from a clean Git checkout at the manifest's commit with `./hack/conformance-export verify DIR` (or `make conformance-export-verify EXPORT_DIR=DIR`). The verifier checks that the checkout is clean and at `HEAD == manifest.commit`, derives the expected cases, depths, launch counts, and test-source digest for `manifest.phase` directly from the checkout's reviewed inventory (`test/conformance/classification.tsv`, `test/conformance/launch-sites.tsv`, `test/conformance/adapter/repeated-sites.tsv`, `test/conformance/baseline/census.tsv` verified against `test/conformance/baseline/run.json`, and `test/conformance/adapter/evidence.cjs`), rejects duplicate JSON member names in `manifest.json` and the TSV metadata lines, checks `producer` against the `run_id` format (and active producer environment when run inside CI or a venue scope), checks `run_id` and `commit` equality across all three files, verifies one canonical absolute binary path and SHA-256 throughout, and verifies every case passed and ran the expected number of launches. When the reviewed inventory legitimately changes in a new commit, there are no profile hashes to regenerate: verifying from that commit's checkout automatically derives the updated expectations.

Failed, dirty, partial, probe, self-test, source-runtime, interrupted, and mixed-binary runs refuse to produce an export. Explicit export invocations (`./hack/conformance-export run` and `verify`) return infrastructure status `70` on refusal (or `97` on venue refusal). Diagnostic `hack/remote run conformance` preserves the comparison's own verdict (`0`, `1`, `124`) when an ineligible run is refused; a failure to create, serialize, or write the candidate after the suite passes, or any post-pass staging/publication error, returns infrastructure status `70` (never comparison failure `1`) while leaving the comparison's own passing summary separate and visible.

**Trust boundary and guarantees.** The suite, the binary under test, the producer account, and the storage/artifact location from which the files are taken are trusted; run identity comes from the producer's environment, and this mechanism is not authentication against someone who controls that account. Within that boundary, an accepted export ties one passing, complete gate-phase binary run's census and launch tables to one run identifier, clean commit, executable path, and measured executable SHA-256, and detects accidental mismatch, omission, or metadata tampering across the three files. It does **not** prove the binary was built from the recorded commit (that is the build's job) and does not combine independent runs into one.

Workflow steps to add to the `ubuntu-26.04` job in `.github/workflows/test.yaml` once a Go binary passes the target gate phase (after `./hack/venue-attest-ci`; set `PHASE` to the gated phase, calling `./hack/conformance-export` directly so exact non-verdict statuses are preserved):

```yaml
      - name: Set up pinned Node for conformance
        if: ${{ matrix.runner == 'ubuntu-26.04' }}
        uses: actions/setup-node@v7
        with:
          node-version: '24.11.1'
      - name: Run binary conformance export
        if: ${{ matrix.runner == 'ubuntu-26.04' }}
        env:
          npm_config_cache: ${{ runner.temp }}/npm-cache
        run: |
          if ! command -v lsof >/dev/null; then
            sudo apt-get update && sudo apt-get install -y lsof
          fi
          bin="$RUNNER_TEMP/conformance-model-gateway"
          export_dir="$RUNNER_TEMP/conformance-export"
          go build -o "$bin" .
          TARGET="binary:$bin" PHASE="phase-2" EXPORT_DIR="$export_dir" ./hack/conformance-export run
          ./hack/conformance-export verify "$export_dir"
```

Include `${{ runner.temp }}/conformance-export` in the `ubuntu-26.04` artifact upload alongside `go-test-plain.jsonl` and `go-test-process.jsonl`.
