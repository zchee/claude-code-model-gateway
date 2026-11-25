# Source test inventory

Both TSV files use UTF-8, literal tabs and one record per line, without CSV quoting. Backslash is written as `\\`, as in the census; tab, carriage return and newline within a field are represented by `\t`, `\r` and `\n`. Paths are relative to `plugins/model-gateway` at the pin in `hack/pinned-source`. Locations are one-based; sort paths and names as text and positions as numbers.

## Case obligations

`classification.tsv` has nine columns: `path`, `line`, `column`, `name`, `class`, `phase`, `package`, `gate`, `note`. Read columns by header name, not by position. The first four fields identify each case. Locations identify registrations, not assertions. A table can register multiple names at one position. A parent is itself a case; its children have names of the form `parent > child`. Join these four columns to `baseline/census.tsv` to distinguish what was registered from what passed, failed or skipped.

| `class` | Obligation |
|---|---|
| `conformance` | Run the source check against the launched Go binary through the launch adapter. Using a source helper to construct fixture input or an expected value does not by itself make the gateway under test JavaScript. |
| `go-port` | Check the behavior in Go. Use this for direct module calls, injected dependencies, and cases where the original observation is Node-specific but the behavior is not. |
| `difference:<slug>` | Test the replacement behavior in the [port plan’s difference table](../../docs/port/model-gateway-go-port.md#310-differences-from-the-source). Use this class only when that difference changes **every** assertion. Mixed cases are `go-port`: the note identifies the changed assertion lines and difference slug, and the remaining assertions stay due. |
| `js-only:<reason>` | No Go case. A JS implementation detail is a case-level exemption only when no assertion protects observable gateway behavior. Reasons: `node-harness-self-test`, `js-source-lint`, `out-of-root-dependency`, `developer-probe`. |
| `unsupported:<reason>` | Outside the port milestone. Reasons: `windows-only`, `dormant-code`. |

`phase` (`phase-0` through `phase-5`) is the implementation phase; `package` owns the behavior under test. `gate` specifies when all runtime assertions of the case must pass; assertions on text or configuration the port ships are due with packaging in phase 5. `task` means the owning task, its transitive prerequisites and earlier phases supply every required runtime behavior. A black-box case is due no earlier than the gate at which the launch adapter is proven. `phase` means another independent task in that phase is also needed; for example, checking that `doctor` leaves a file unchanged before `ensure` repairs it requires both commands. Use the [test ownership table](../../docs/port/test-ownership.md) to resolve a `task` row by `(phase, package)` or a `phase` row by its phase gate. Exempt rows leave all three fields empty.

`note` explains a replacement expectation, a cross-task dependency or the concrete reason for an exemption. A runtime case can also check documentation: assertions about a shipped skill or manifest are due with packaging (phase-5), without moving the runtime case. Wording in unshipped source documentation is a candidate for a separately reviewed assertion exemption. A skipped source case is not automatically exempt and is not passing evidence.

The 57 test files have 693 text lines beginning with optional indentation and `test(`. One, `test/gateway-process-isolation.test.js:1150`, is inside a string written to an intentionally failing child suite by the case at line 1143. It is not a registration in the outer suite. The inventory therefore covers 692 outer registration sites and five nested `t.test` sites. No test is added for the string.

## Launch adapter inventory

`launch-sites.tsv` records the source `path`, `line`, `column` of a launch or helper invocation, its `kind`, the `via` API or wrapper, the full owning `case`, and literal or described `argv`. A `-` case identifies a shared helper's launch site. Look up its callers by case; a call inside a table-driven test may occur once for each expanded name. Repeated execution at one site within one case is one row, not a runtime invocation count. When a child starts a process, a second row records its parent's name, the same launch coordinates and arguments, and `via` = `subtest: <child name>`. This attributes the launch to the parent without counting another process or attributing it to siblings.

`kind` is one of `cli` (gateway command), `worker` (worker entry), `supervisor` (`serve-shim`), `shim` (generated stable command launcher), `hook` (hook entry), or `helper` (generic launch seam or explicitly identified fixture). Keep test-only fixture CLIs on Node: a filename or `serve-shim` argument alone does not identify a product executable. Ordinary Node `-e` scripts that only import a library, foreign tools, fake upstreams, and test runners are not gateway launches.

## Approved exemptions

`exemptions-approved.tsv` records the approval of every case that is exempt from the port (`js-only:` and `unsupported:` classes): `path`, `line`, `column`, `name`, `class`, `approved`. An exemption is valid only while a row with the same key and the identical class exists here. Adding an exemption or changing the class of an exempt case therefore needs a new independent review, which adds or replaces the row; the structure check compares both files in both directions.

`deferrals-approved.tsv` records approval to move an assertion past its case’s phase. It has the same seven pair-key columns as `assertion-sites.tsv`, followed by `due` and `approval`. Every deferred pair must have exactly one approval for the same pair and phase; a mapping review alone cannot move the date. The original case still has its own gate.

## Assertions per case

`assertion-pairs.tsv` is generated evidence: one row per pair of a case (`path`, `line`, `column`, `name`) and an assertion it can reach (`site_path`, `site_line`, `site_column`), with the callee or handed-off assertion function as written (`call`) and how the case reaches it (`via`: `direct`, `helper:`, `callback:`, `embedded:`). It covers all cases, is produced in a venue by `make assertion-sites` and `hack/assertion-extract`, and is never edited by hand. It records what a case may reach, not what ran.

`assertion-overrides.tsv` holds reviewed corrections to that relation (the seven key columns, `action`, `reason`); the generator applies them. `hack/assertions/call-dispositions.tsv` records a disposition and reason for calls the generator could not follow. Missing, unknown or stale dispositions prevent extraction.

`assertion-sites.tsv` is the editable mapping: the pairs of due cases only, with `target` (the Go test that takes the assertion's place, or an approved `not-ported:` reason), `review` and `due` (a phase, never earlier than the case's phase). `hack/parity-check structure` requires its pairs to equal the generated pairs of due cases in both directions. See `hack/assertions/README.md` for the generator.
