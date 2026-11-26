# Running the launch adapter

## What a verdict means

**Attribution.** In a passing binary run of the pinned source tests, each selected product launch at an inventoried site ran the selected executable, and each site ran the expected number of times. Missing executions, recognized but unattributed launches, and source-runtime records in a binary run fail the comparison. This proof does not cover a shell starting the runtime, a worker thread, a copied runtime binary, or process-image replacement. The reviewed test-source fingerprint must match before the suite starts; re-pinning requires checking the launch forms and counts again. A copied runtime used as a fixture proxy is not a product launch.

**Venue hygiene.** Before allowing another run on the dedicated venue, the owner must verify that the prior run has no live processes (and therefore no listeners). A reservation on disk prevents a new run even if the kernel lock is free. If the outside owner dies or its watchdog expires, the reservation stays. The refusal names the reservation. Recovery is an explicit `hack/remote provision` or `deprovision` that verifies termination before clearing it.

**Verdicts through `hack/remote run conformance`.** `0` means pass, `1` means comparison or test failure, `2` means invalid request, and `124` means timeout. Other statuses are not verdicts: `97` is a venue refusal, `255` is a transport failure, and `128+signal` means interruption. Failure to establish cleanup is not evidence of a product mismatch.

**Non-goals.** The adapter does not defend against tests deliberately evading interception or automatically recover a dead outside owner. Standalone containment is best effort: a process group cannot contain detached children. Dispose of the runner after use; an in-group timeout control does not establish detached-child cleanup.

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
| `PHASE=… PACKAGE=… GATE=…` | Select conformance rows by their exact inventory values. Omit any column to leave it unrestricted. |

## Attribution evidence

The launch inventory defaults to one execution per `(case, site, role)` row. `adapter/repeated-sites.tsv` lists the exceptions. Nested launches credit their parent only through an explicit inventory edge. Passing tests must also have complete, consistent file and suite summaries. A record must match the selected target and digest and identify a successful process launch. Before running the suite, `checkPinnedLaunchForms` scans all JS/CJS/MJS files under the pinned plugin's `test/` for out-of-scope forms (`worker_threads`, `cluster`, `shell:` options, uninventoried `child_process` APIs, and unreviewed `process.execPath` copies) and checks the full tree SHA-256 digest; changing a fixture or re-pinning fails until reviewed.

## Containment and recovery

On the shared Linux venue, `hack/remote` is the single owner of OS containment: it runs **every** `run` invocation (regardless of Make goals or their order) inside one user systemd scope (`RuntimeMaxSec` = `TIMEOUT_SECONDS + 90` when `conformance` is among the goals, `600` seconds otherwise; `KillMode=control-group`) without installing a subreaper, so detached children remain in the scope cgroup while keeping normal `ppid=1` orphan parentage. Before releasing the launcher into `systemd-run`, `hack/remote` persists the scope unit name together with the launcher's PID and kernel start tick to `active-scope` while holding the venue `flock` outside the scope. It clears `active-scope` and releases the lock only after outside verification confirms both that the launcher is dead and that the scope is `inactive` with an empty `cgroup.procs`. If verification fails or the outer lock owner dies abruptly, `hack/remote status` reports the venue unavailable instead of free until `hack/remote provision` stops and verifies the recorded launcher and scope (before running the clean-state and port checks) and recovers it.

The independent outside watchdog covers startup, verification, and pruning, including a stopped owner. It kills the owner's process group, not the reserved scope; the latter has its own systemd deadline. After a watchdog expiry or an unverifiable launcher identity, the next run refuses and names `active-scope`. Recover with `VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote provision`; after a deliberate control, append the printed unit name to require an exact match under the lock. Never remove the marker manually. A passing recovery checks the recorded launcher, scope membership and service state before reuse.

The self-test runs its three named controls in the production scope; it creates no sibling units. Two lifecycle controls are separate invocations because they intentionally end their own run: `hack/remote check-cancellation` cancels while its fixture listener is live (expect `143`, then a free venue); `hack/remote check-owner-deadline` freezes its own outside owner (expect no verdict, then refusal until recovery). Neither is part of a product comparison. On standalone hosts only the process-group check applies.

The owner publishes the final class and status only after verified drain and a pruning attempt. Among completed comparisons, timeout takes precedence over failure, and failure over pass. A prune error is reported without changing that result; unverified teardown or a failed outcome write means **no verdict**, not a failed comparison. `70` denotes an infrastructure error.

## Evidence output

The venue returns TAP, one `# launch ` comment per JSON record, and a final summary. The `build` field is the binary's SHA-256 (`null` in identity mode). On an attested runner, `LAUNCH_LOG` may name a new absolute JSONL file outside the fixtures. The remote transport returns the same records as TAP comments because a successful remote workspace is removed. For example, `perl -ne 'print $1 if /^# launch (.*\n)$/' run.tap` extracts the JSONL stream from a captured run.

On an attested runner, export a successful binary suite's JSONL for `hack/parity-check results --launches` with `./hack/conformance-launches "$LAUNCH_LOG" "$RUN_ID" "$COMMIT" > launches.tsv`. Supply the same run ID and full gated checkout commit as for the other evidence. The exporter rejects identity records, unattributed records, and mixed binaries, and attributes a nested launch to a parent only for an explicit inventory edge. Probes and the stand-in self-test are tooling checks, not product evidence.
