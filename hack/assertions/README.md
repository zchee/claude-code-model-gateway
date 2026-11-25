# Assertion inventory

The evidence is a static **may-reach** relation, not runtime coverage. A forbidden callback containing `assert.fail` is an obligation even when the suite passes without invoking it. Repeated visits to the same source position in one case count once.

Collect the checksummed frame in the attested venue, then extract its evidence on the developer machine:

```sh
VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run assertion-sites > assertion-frame.log
```

```sh
./hack/assertion-extract assertion-frame.log
```

The venue installs the integrity-pinned TypeScript parser and Node declarations with `npm ci --ignore-scripts`. The collector checks the pinned export against the baseline, runs a synthetic analyzer self-test, and parses source. It **does not execute** plugin modules, tests, projected programs, or fixtures. `--audit <new-file.json>` on extraction preserves the unresolved case/call edges, other cases whose targets at those same sites were resolved automatically, counts of automatic resolutions, embedded-program anchors, and unmatched sites. Keep this diagnostic output outside Git. A damaged frame, a different collector or dependency lock, an unowned assertion, or a missing/stale call disposition prevents publication. The fingerprint covers the venue entry script, every adjacent `.cjs` module and JSON package manifest/lockfile. Ordinary extraction reads the census, not the classification. `--initialize-mapping` creates, but never overwrites, the editable mapping. `--classification <file>` selects its classification (the checkout copy is the default). To regenerate an untouched initial mapping after obligations change, verify **every** existing target and review is empty, move the existing mapping to a backup outside the checkout, reinitialize, and inspect the diff before discarding the backup. Preserve and reconcile any nonempty handoff data instead of reinitializing it. Run `hack/assertions/test-extraction <venue-log>` after extraction to exercise the census-only, stale-generator and mixed-case witness controls.

## The three conformance tables

All tables use the same literal-tab format as the baseline. Source coordinates are plugin-relative, one-based JavaScript lines and UTF-16 columns.

- `test/conformance/assertion-pairs.tsv` is generated from the export, expanded-case census and attribution overrides, not the classification or editable mapping. The case key (`path`, `line`, `column`, `name`) and assertion key (`site_path`, `site_line`, `site_column`) are followed by the callee `call` and provenance `via`.
- `assertion-overrides.tsv` has those seven key fields, `action` (`add`/`remove`) and `reason`. Additions must point to a parsed assertion absent from the specified case; removals must identify an existing pair. Duplicate and stale overrides fail. A successful addition has `via=override`. A removal is appropriate only when the source excludes that case from the branch.
- `assertion-sites.tsv` has the seven key fields plus `target`, `review`, `due`. It contains only pairs of `conformance`, `go-port` or `difference:*` cases. Exempt pairs remain visible in the generated evidence. The initial `due` is the case's phase; `target` and `review` start empty.

## Reviewing indirect calls

`hack/assertions/call-dispositions.tsv` is an extractor input, **not a parity mapping**. Its first line pins the plugin tree. Subsequent rows group positions with the same source file, callee spelling, case scope, disposition, expected assertion references and reason. `scope` is `*` for all reaching cases, or a JSON array of exact four-field census keys for mixed bindings. Every `(call site, case)` must occur exactly once. Callback or child assertion evidence must be present for **each individual case** in the row, never just for another caller at that location. Expand the comma-separated one-based `line:column` list to recover each call. Two nested calls can share a start position; the callee disambiguates them. Backslashes, tabs and line breaks in a callee use census escaping. The extractor rejects unknown disposition values, duplicate positions for one callee, missing or stale entries, and a different tree pin.

Sample an individual position in the pinned export, not just a callee name. Trace the receiver's construction or the function argument at the caller. `unresolved` in `--audit` gives affected case keys; `resolvedAtOpenSites` shows cases where the same target was determined mechanically. For a callback, check its registration and body against the pairs **for that same case**. Function arguments are visited even when the receiving API is external. A forbidden callback must retain its `assert.fail`. A genuine extra pair belongs in `assertion-overrides.tsv`, never in a no-assertion rationale.

The disposition vocabulary is closed:

| Value | What to check at each listed position |
|---|---|
| `data-method` | Follow the receiver to fixture JSON, array, string, buffer or collection data and verify its method is intrinsic, not a project method; any predicate or iteration callback is traced at its argument. |
| `node-io` | Confirm the receiver was created as a Node server, socket, response, child, stream or emitter; these native operations contain no suite assertions, and handlers are visited where supplied. |
| `staged-module` | Resolve the staged path, filename or mode in the caller and confirm the selected product or historical module has no assertion import; explicit child continuations remain in the projected program. |
| `fixture-operation` | Follow the concrete fixture operation, table row or default named in the reason and check its body for assertions or further calls; do not infer this disposition from the function name. |
| `product-result` | Trace the object back to the named product factory and examine the returned operation: its implementation is in the assertion-free product tree, while callbacks passed by the case remain independently traced. |
| `historical-adapter` | Check the adapter loader, snapshot digest and selected schema conversion method; the historical implementation has no assertion API, and the loader's own digest assertion remains a pair. |
| `callback-attributed` | This call **can** run assertion-bearing callbacks: check the registrations and require every case in scope to own its witnessed callback assertions. |
| `child-entry-reviewed` | The entry, preload, shell command or argv cannot be resolved completely. Trace the executable, flags and fixture writer for these cases and verify the child has no additional assertion obligation. |
| `child-attributed` | The child has a known assertion despite an opaque runtime launch path. The `assertions` field lists its exact `[site_path,site_line,site_column]` triples; every listed case must own each one. |

`callbackAssertions` in the diagnostic frame identifies each concrete callback's directly written assertion by case, call site and assertion coordinates. A callback-attributed row with no such witness or a missing same-case pair is rejected. Conversely, a witnessed row cannot use an assertion-free disposition.

## Reachability and limits

The checker traces source declarations, local/module calls, passed function values, option members, callback bindings and return values. Function arguments of external APIs are still visited: e.g. error predicates, promise executors and continuations, event handlers and timers. A visited set cuts cycles per case. A case owns its locally registered teardown. File-wide setup and teardown callbacks belong to every case in that file. A registering parent also owns the child checks it schedules; siblings do not own one another’s checks. There are no scoped `beforeEach`/`afterEach` calls in parent callbacks in this pin. Expanded registrations share a body unless an override records a disjoint scenario branch.

Eval arguments and written scripts are projected into virtual source files alongside the host, so relative helper imports resolve. Original offsets survive escapes, concatenation, joins and template substitution. The projection follows script parameters to caller inputs and preserves literal module paths; other substitutions are inert identifiers, never evaluated. Reachable Node entry files in the pinned test tree are followed from a resolved spawn argument. Launch options, environments and fork `execArgv` follow variable initializers, simple wrapper returns and case-bound parameters. `NODE_OPTIONS` preloads are read after applying object-spread precedence; a later explicit value replaces earlier spreads, while an unreadable surviving value makes the launch unresolved. A launch is rechecked after all calls in the case have supplied their bindings, so a second invocation of a shared wrapper cannot hide a different preload. Opaque options, environment or flag arrays require a disposition even when the executable and main script are known. In particular, the native-budget client is a separate script: its assertion locations are linked through the real launch at `native-budget-probe.test.js:25`. The mode/prompt table and callers determine the explicit removals for unselected branches. The nested suite written by `gateway-process-isolation.test.js:1146` contains throws, not assertions or an extra census case.

Automatic boundary resolutions require source or type evidence. They cover resolved Node and ECMAScript declarations, statically determined package-lock dependencies, JSON data, product module paths, local function values, and case-specific callback bindings. Before using a product boundary the tool scans JavaScript under `lib/`, `bin/` and `hooks/` for assertion API imports or spellings; it rejects an assertion-bearing product tree. An opaque receiver's `.map` or `.end` is not a builtin just because of the name. A known `require` function does not prove what module an unknown path loads. Remaining dynamic targets, including likely builtins whose receiver the tool cannot establish, require a row with a source-checkable rationale. The frame's `rules` counts are per distinct `(path,line,column,callee)` with an automatically resolved binding; a partially resolved position can also require review for another case.

The site is the call expression or the function reference handed to another operation. For example, `{ kill: assert.fail }` records `assert.fail` at the reference with `via=callback:kill`; the guard should prevent that callback from running. A separate scan checks literal `assert.method` and inline `require('node:assert/strict').method` spellings with or without a following parenthesis, and bare `assert(`, against parsed assertion sites, including text in child programs. It catches a dropped site, not a missing owner of a site attributed elsewhere. Static typing cannot prove arbitrary JavaScript dispatch; review call dispositions, callback escapes, spawn attribution and scenario-specific overrides rather than inferring completeness from counts alone.
