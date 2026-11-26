# Running the launch adapter

From the worktree root, run `VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run conformance`. Without selectors the default `TARGET=identity` compares the entire pinned suite to the baseline, by file, registration line and column, full name, and outcome. It never edits the source tests or harness.

Append these make arguments to the same venue invocation:

| Argument | Effect |
| --- | --- |
| `MODE=probes` | Run the four required launch probes plus the complete pinned orderly-SIGTERM test. All four named probes and the pinned case must pass with attributed launches of the expected role. |
| `MODE=self-test` | Run identity with a native recording binary at each observed gateway dispatch. It checks the translated argv and fixture environment, then the real Node gateway handles the test so subsequent launches remain reachable. Raw environment values never enter the launch log. Also runs the adapter and containment negative controls. |
| `TARGET=binary:/absolute/venue/path` | Run the executable itself in place of each gateway launch. The path is on the venue, not the sending machine. Without selectors this runs all conformance cases. |
| `SITE=test/file.test.js:line` | Limit an identity diagnostic (including `MODE=self-test`) to one pinned registration site; cannot be used as binary conformance evidence. |
| `TIMEOUT_SECONDS=…` | Set the job wall-clock limit in seconds (default 200, up to `MAX_TIMEOUT_SECONDS`). On expiry the supervisor lists active test files, terminates the job's scope (including detached children without changing orphan parentage), verifies termination, and exits 124 (infrastructure timeout). |
| `MAX_TIMEOUT_SECONDS=…` | Explicitly raise the job timeout ceiling above the 200-second default (up to the venue's 1800-second hard bound) for slower targets or cold runners without editing scripts. |
| `CLASS=conformance` | Run all conformance rows against the chosen target. |
| `PHASE=… PACKAGE=… GATE=…` | Select conformance rows by their exact inventory values. Omit any column to leave it unrestricted. |

A selected run rejects an empty selection, a missing or nonpassing case, missing or inconsistent file/suite summaries, any actual product invocation that cannot be attributed or redirected (while leaving known decoys on Node), or any inventoried launch obligation (by site and role) that lacks a valid attributed launch for the selected target and measured digest. A nested launch counts for its parent only if the inventory explicitly lists that child under the parent.

On Linux venues, `hack/remote` runs conformance inside an outer user systemd scope with an OS-enforced deadline (`RuntimeMaxSec`), and `hack/conformance` runs the job inside a bound job scope (`BindsTo` + its own `RuntimeMaxSec`). Detached children remain inside the job cgroup while keeping normal `ppid=1` orphan parentage. Termination of the job scope is verified before the venue lock is released; if the inner supervisor is killed abruptly, outer teardown stops and verifies the scope, and if termination cannot be verified (or the outer lock owner dies without clearing the active scope), `hack/remote status` reports the venue unavailable instead of free until `hack/remote provision` verifies all scopes are empty and recovers it.

The venue returns TAP, one `# launch ` comment per JSON record, and a final summary. The `build` field is the binary's SHA-256 (`null` in identity mode). On an attested runner, `LAUNCH_LOG` may name a new absolute JSONL file outside the fixtures. The remote transport returns the same records as TAP comments because a successful remote workspace is removed. For example, `perl -ne 'print $1 if /^# launch (.*\n)$/' run.tap` extracts the JSONL stream from a captured run.

On an attested runner, export a successful binary suite's JSONL for `hack/parity-check results --launches` with `./hack/conformance-launches "$LAUNCH_LOG" "$RUN_ID" "$COMMIT" > launches.tsv`. Supply the same run ID and full gated checkout commit as for the other evidence. The exporter rejects identity records, unattributed records, and mixed binaries, and attributes a nested launch to a parent only for an explicit inventory edge. Probes and the stand-in self-test are tooling checks, not product evidence.
