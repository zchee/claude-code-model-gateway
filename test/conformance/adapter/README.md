# Running the launch adapter

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
| `PHASE=… PACKAGE=… GATE=…` | Select conformance rows by their exact inventory values. Omit any column to leave it unrestricted. |

## Exit statuses

Both `hack/conformance` and `hack/remote` distinguish runtime/parity failures, usage errors, and infrastructure timeouts:

| Exit status | Meaning |
| --- | --- |
| `0` | Success. |
| `1` | Parity, test, target, or runtime failure (including detached survivors, unattributed launches, or unverified teardown). |
| `2` | Usage, argument, selector, or timeout-configuration error. |
| `124` | Infrastructure timeout (either `TIMEOUT_SECONDS` in `hack/conformance-supervise` or `RuntimeMaxSec` on the venue scope). |

(`97` is reserved for `hack/venue-check` attestation/environment refusals; `255` indicates an SSH transport failure.)

## Launch attribution contract

`test/conformance/launch-sites.tsv` records launch sites (`case`, `site`, `role`), not loop iteration counts. A selected run rejects an empty selection, a missing or nonpassing case, missing/inconsistent/out-of-order file or suite summaries, or any due `(case, site, role)` row that lacks at least one valid attributed launch for the selected target and measured digest. A nested launch counts for its parent only if the inventory explicitly lists that child under the parent.

Per-execution enforcement is provided by fail-closed interception in `preload.cjs` and record-level validation in `evidence.cjs`: every execution of a repeated site (such as the three iterations at `test/wiring-mode.test.js:136–139`) emits its own launch record and is validated against `target`, measured SHA-256 `build`, positive `pid`, and `!unattributed`. Product entry paths are classified independently of Node executable spelling (`process.execPath`, symlinks, `node`, `env node`) and leading Node options; any product invocation through `child_process` that cannot be attributed or uses an unsupported option layout fails closed with an `unattributed` record rather than falling back to Node, while known decoys stay on Node.

## Containment and recovery

On the shared Linux venue, `hack/remote` is the single owner of OS containment: it runs **every** `run` invocation (regardless of Make goals or their order) inside one user systemd scope (`RuntimeMaxSec` = `TIMEOUT_SECONDS + 90` when `conformance` is among the goals, `600` seconds otherwise; `KillMode=control-group`) without installing a subreaper, so detached children remain in the scope cgroup while keeping normal `ppid=1` orphan parentage. Before releasing the launcher into `systemd-run`, `hack/remote` persists the scope unit name together with the launcher's PID and kernel start tick to `active-scope` while holding the venue `flock` outside the scope. It clears `active-scope` and releases the lock only after outside verification confirms both that the launcher is dead and that the scope is `inactive` with an empty `cgroup.procs`. If verification fails or the outer lock owner dies abruptly, `hack/remote status` reports the venue unavailable instead of free until `hack/remote provision` stops and verifies the recorded launcher and scope (before running the clean-state and port checks) and recovers it.

Outside `hack/remote` (disposable hosted runners, macOS, or Linux hosts without a user systemd instance), `hack/conformance` runs with non-reaping (`waitid(WNOWAIT)`) process-group containment without requiring a user systemd bus, and scope-only controls in `hack/conformance-containment-check` report `not applicable on this host` by name.

## Evidence output

The venue returns TAP, one `# launch ` comment per JSON record, and a final summary. The `build` field is the binary's SHA-256 (`null` in identity mode). On an attested runner, `LAUNCH_LOG` may name a new absolute JSONL file outside the fixtures. The remote transport returns the same records as TAP comments because a successful remote workspace is removed. For example, `perl -ne 'print $1 if /^# launch (.*\n)$/' run.tap` extracts the JSONL stream from a captured run.

On an attested runner, export a successful binary suite's JSONL for `hack/parity-check results --launches` with `./hack/conformance-launches "$LAUNCH_LOG" "$RUN_ID" "$COMMIT" > launches.tsv`. Supply the same run ID and full gated checkout commit as for the other evidence. The exporter rejects identity records, unattributed records, and mixed binaries, and attributes a nested launch to a parent only for an explicit inventory edge. Probes and the stand-in self-test are tooling checks, not product evidence.
