# Pinned Node test census

`census.tsv` contains one ordered result per case reported by the **outer** Node test runner. Paths are relative to the plugin; line and column are one-based registration coordinates, and depth starts at zero. Names include all parents, separated by ` > `. Backslash, tab, CR and LF in a field are written as `\\`, `\t`, `\r` and `\n`. A pass has `-` for `detail`. For a boolean skip with no runner message, `detail` quotes the pinned declaration's predicate.

`run.json` records the source and tools, the package command, exit status, UTC start and end, per-file completion and totals. A failed assertion is an observed outcome, not a reason to retry or omit the case. Both files are generated evidence; do not edit them by hand.

Run from the repository root. The remote script checks the venue, installs locked dependencies and runs `npm test` once. Its output includes one checksummed, compressed frame of runner events; no extra SSH retrieval or change to `hack/remote` is needed.

```sh
VENUE_HOST=debian-13-trixie.gaudiy-platform hack/remote status
```

```sh
VENUE_HOST=debian-13-trixie.gaudiy-platform hack/remote run baseline-node >| baseline-node.log 2>&1
```

```sh
VENUE_HOST=debian-13-trixie.gaudiy-platform hack/remote status
```

```sh
hack/baseline-extract baseline-node.log
```

Extraction checks the pin, file summaries, runner totals and parent/child identities. It checks that every registration site reported at least one case and that each case maps to a site. A separate structure check compares expanded cases against the independently derived classification; the extractor does not use classification data. Until that check exists, adopt a replacement census only when the sorted `(path, line, column, name)` rows from both `census.tsv` and `../classification.tsv` are identical, including duplicate counts. Cancelled or aborted runs cannot replace the baseline. `hack/baseline-extract --verify` checks the census SHA-256 against `run.json` and detects a mismatched pair, including an interruption between file renames. For diagnosis, `hack/baseline-extract --dump "$TMPDIR/baseline-frame.json" baseline-node.log` writes a new file outside the checkout instead of replacing evidence. Run extraction even if `npm test` returned nonzero: the exit code alone does not distinguish an individual failure from an incomplete run. The text match at `test/gateway-process-isolation.test.js:1150` belongs to a temporary program executed by the test at line 1143. Its intentional TAP failure is asserted by that outer case; it is not a second outer case.

## Discarded collections

The first collector mistook a socket-activated session bus for a leftover and withheld its run record. The second collector distinguished processes in the login session from services under the systemd user manager. Its runner output and the first run's retained events had identical test identities and outcomes: 816 passed, 9 failed and 7 skipped. Neither run had `lsof`, which the pinned code uses on Linux to find the owner of a listening port. The second census (SHA-256 `c17e7b5894c07ab8b06118ddfcec52e66629f772eccf3fb2a1a7e2e583b99764`) is not the baseline. After the distribution's `lsof` was installed, the whole suite was run again, not just the failed cases. All nine passed on that run; all 832 identities and all seven skips were unchanged. A fourth full run checked interruption, process and evidence integrity; its census was byte-identical to the third. The fifth collection used exact session-bus executable identity and bounded process-group cleanup, and reproduced the same census.

The nine failures from the second run, with the first error line as reported:

| Registration | Name | First error line |
|---|---|---|
| `test/discovery-cache.test.js:284` | ensure writes the discovery cache with missing ChatGPT auth without refusing the router | `The input did not match the regular expression /ChatGPT sign-in is required/. Input:` |
| `test/discovery-cache.test.js:345` | healthy quiet ensure emits only the discovery-cache context without a readiness nudge | `healthy Codex must not emit a readiness nudge` |
| `test/gateway-process-isolation.test.js:1326` | parallel healthy ensures reap a real owned stale-version orphan while the current worker and stream survive | `probe child survived its supervisor` |
| `test/gateway-process-isolation.test.js:1388` | sibling ensure retires dead records without deleting replacement worker and proxy records | `replacement pid record was not written: /tmp/model-gateway-venue/runs/run.Ff7xX3JQ/t.hJNeEX/model-gateway-sibling-record-replacement-OcjFsp/.claude/model-gateway/guardian.pid` |
| `test/gateway-process-isolation.test.js:1577` | foreign configured-port supervisor is preserved and reported | `The input did not match the regular expression /shim supervisor conflict: PID 80522 owns :38457 from a different install root/. Input:` |
| `test/gateway-process-isolation.test.js:1614` | cache-junction gateway process is preserved as a foreign port owner | `The input did not match the regular expression /belongs to a different install root/. Input:` |
| `test/gateway-process-isolation.test.js:1793` | setup restart path refuses a foreign shim before it can restart its worker | `The input did not match the regular expression /refusing to stop PID 80692 on :39373; it belongs to a different install root/. Input:` |
| `test/rc-compat-mode.test.js:560` | remote-control CLI adopts an unmarked mapping from its serving supervisor | `Enable Remote Control compatibility by adding only this block:` |
| `test/recovery-lifecycle.test.js:109` | installed gates recover stale boot state once, preserve streaming, recover death and honor persistent stop | `model-gateway: could not confirm the owner of :39515 (last observed PID unknown); the owning process could not be identified as this model-gateway install; left the listener untouched.` |
