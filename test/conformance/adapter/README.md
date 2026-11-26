# Running the launch adapter

From the worktree root, run `VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run conformance`. Without selectors the default `TARGET=identity` compares the entire pinned suite to the baseline, by file, registration line and column, full name, and outcome. It never edits the source tests or harness.

Append these make arguments to the same venue invocation:

| Argument | Effect |
| --- | --- |
| `MODE=probes` | Launch-only checks plus the complete pinned orderly-SIGTERM test. |
| `MODE=self-test` | Run identity with a native recording binary at each observed gateway dispatch. It checks the translated argv and fixture environment, then the real Node gateway handles the test so subsequent launches remain reachable. Raw environment values never enter the launch log. |
| `TARGET=binary:/absolute/venue/path` | Run the executable itself in place of each gateway launch. The path is on the venue, not the sending machine. Without selectors this runs all conformance cases. |
| `SITE=test/file.test.js:line` | Limit an identity diagnostic (including `MODE=self-test`) to one pinned registration site; cannot be used as binary conformance evidence. |
| `TIMEOUT_SECONDS=…` | Shorten the 200-second job limit. On expiry the supervisor lists active test files, terminates the job's process group and rejects the run. |
| `CLASS=conformance` | Run all conformance rows against the chosen target. |
| `PHASE=… PACKAGE=… GATE=…` | Select conformance rows by their exact inventory values. Omit any column to leave it unrestricted. |

A selected run rejects an empty selection, a missing or nonpassing case, or a conformance case with no attributed gateway process. A nested launch counts for its parent only if the inventory explicitly lists that child under the parent.

The venue returns TAP, one `# launch ` comment per JSON record, and a final summary. The `build` field is the binary's SHA-256 (`null` in identity mode). On an attested runner, `LAUNCH_LOG` may name a new absolute JSONL file outside the fixtures. The remote transport returns the same records as TAP comments because a successful remote workspace is removed. For example, `perl -ne 'print $1 if /^# launch (.*\n)$/' run.tap` extracts the JSONL stream from a captured run.

On an attested runner, export a successful binary suite's JSONL for `hack/parity-check results --launches` with `./hack/conformance-launches "$LAUNCH_LOG" "$RUN_ID" "$COMMIT" > launches.tsv`. Supply the same run ID and full gated checkout commit as for the other evidence. The exporter rejects identity records and mixed binaries, and attributes a nested launch to a parent only for an explicit inventory edge. Probes and the stand-in self-test are tooling checks, not product evidence.
