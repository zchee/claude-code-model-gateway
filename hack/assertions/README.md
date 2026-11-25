# Assertion inventory

The evidence is a static **may-reach** relation, not runtime coverage. A forbidden callback containing `assert.fail` is an obligation even when the suite passes without invoking it. Repeated visits to the same source position in one case count once.

Collect the checksummed frame in the attested venue, then extract its evidence on the developer machine:

```sh
VENUE_HOST=debian-13-trixie.gaudiy-platform ./hack/remote run assertion-sites > assertion-frame.log
```

```sh
./hack/assertion-extract assertion-frame.log
```

The venue installs the integrity-pinned TypeScript parser and Node declarations with `npm ci --ignore-scripts`. The collector checks the pinned export against the baseline, runs a synthetic analyzer self-test, and parses source. It **does not execute** plugin modules, tests, projected programs, or fixtures. `--audit <new-file.json>` on extraction preserves the unresolved case/call edges, other cases whose targets at those same sites were resolved automatically, counts of automatic resolutions, embedded-program anchors, and unmatched sites. Keep this diagnostic output outside Git. A damaged/stale frame, unowned assertion, or missing/stale call disposition prevents publication. `--initialize-mapping` creates, but never overwrites, the editable mapping. `--classification <file>` selects a classification for initialization (the checkout copy is the default).

## The four tables

All tables use the same literal-tab format as the baseline. Source coordinates are plugin-relative, one-based JavaScript lines and UTF-16 columns.

- `test/conformance/assertion-pairs.tsv` is generated from the export, expanded-case census and attribution overrides, not the classification or editable mapping. The case key (`path`, `line`, `column`, `name`) and assertion key (`site_path`, `site_line`, `site_column`) are followed by the callee `call` and provenance `via`.
- `assertion-overrides.tsv` has those seven key fields, `action` (`add`/`remove`) and `reason`. Additions must point to a parsed assertion absent from the specified case; removals must identify an existing pair. Duplicate and stale overrides fail. A successful addition has `via=override`. A removal is appropriate only when the source excludes that case from the branch.
- `assertion-sites.tsv` has the seven key fields plus `target`, `review`, `due`. It contains only pairs of `conformance`, `go-port` or `difference:*` cases. Exempt pairs remain visible in the generated evidence. The initial `due` is the case's phase; `target` and `review` start empty.
- `assertion-unresolved.tsv` records each call the analyzer could not resolve in at least one case: `path`, `line`, `column`, `callee`, `cases`, `disposition`, `reason`. The numeric count includes **all** distinct reaching cases, even when some bindings at that location were resolved. Nested call expressions may share a start position; the original callee spelling distinguishes them. Tabs, CR, LF and backslashes in that spelling are escaped as in the census. The extractor compares the independently generated call keys and case counts in both directions. `no-assertion-reachable` concerns the remaining opaque target, not the whole case or other, already resolved bindings at the same call. `resolved-by-override` requires a reason containing a whitespace-delimited `override-site:<site_path>:<site_line>:<site_column>` reference to a real `add` belonging to a reaching case.

## Reachability and limits

The checker traces source declarations, local/module calls, passed function values, option members, callback bindings and return values. Function arguments of external APIs are still visited: e.g. error predicates, promise executors and continuations, event handlers and timers. A visited set cuts cycles per case. A case owns its locally registered teardown. File-wide setup and teardown callbacks belong to every case in that file. A registering parent also owns the child checks it schedules; siblings do not own one another’s checks. There are no scoped `beforeEach`/`afterEach` calls in parent callbacks in this pin. Expanded registrations share a body unless an override records a disjoint scenario branch.

Eval arguments and written scripts are projected into virtual source files alongside the host, so relative helper imports resolve. Original offsets survive escapes, concatenation, joins and template substitution. The projection follows script parameters to caller inputs and preserves literal module paths; other substitutions are inert identifiers, never evaluated. Reachable Node entry files in the pinned test tree are followed from a resolved spawn argument. In particular, the native-budget client is a separate script: its assertion locations are linked through the real launch at `native-budget-probe.test.js:25`. The mode/prompt table and callers determine the explicit removals for unselected branches. The nested suite written by `gateway-process-isolation.test.js:1146` contains throws, not assertions or an extra census case.

Automatic boundary resolutions require source or type evidence. They cover resolved Node and ECMAScript declarations, statically determined package-lock dependencies, JSON data, product module paths, local function values, and case-specific callback bindings. Before using a product boundary the tool scans JavaScript under `lib/`, `bin/` and `hooks/` for assertion API imports or spellings; it rejects an assertion-bearing product tree. An opaque receiver's `.map` or `.end` is not a builtin just because of the name. A known `require` function does not prove what module an unknown path loads. Remaining dynamic targets, including likely builtins whose receiver the tool cannot establish, require a row with a source-checkable rationale. The frame's `rules` counts are per distinct `(path,line,column,callee)` with an automatically resolved binding; a partially resolved position can also require review for another case.

A separate scan checks the physical source tokens `assert.method(`, `assert(` and inline `require('node:assert/strict').method(` against parsed assertion sites, including text in child programs. It catches a dropped site, not a missing owner of a site attributed elsewhere. Static typing cannot prove arbitrary JavaScript dispatch; review call dispositions, callback escapes, spawn attribution and scenario-specific overrides rather than inferring completeness from counts alone.
