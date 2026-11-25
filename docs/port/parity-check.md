# Parity checks

Run `hack/parity-check` (or `hack/parity-check structure`) to reconcile the
source-case census, classification, static launches, matrix lock and phase
ledger, and assertion pairs. Use `--root DIR` before the mode to check another
checkout. An exempt case must have one entry of the same class in
`test/conformance/exemptions-approved.tsv`. `assertion-pairs.tsv` records all
cases and must reflect the reviewed `add` and `remove` edges. The editable
`assertion-sites.tsv` must match exactly the generated pairs for non-exempt
cases. After the seven key fields its columns are `target`, `review`, `due`;
`due` is never empty, but structure mode does not require targets. A due case
has at least one pair unless a reviewed removal explains its absence.
Regenerate pairs independently in the venue before reviewing updates; comparing
the files alone cannot detect a deleted edge when another pair covers its case.

`hack/parity-check mapping phase-2 internal/worker` checks pairs due in that
phase with the specified owning package and `gate=task`. Omit the package to
include the whole phase, including phase-gated pairs and pending matrix tests.
A Go target uses the package path below this module (the full import path is
also accepted), a dot, and the exact test name with any slash-delimited subtests.
A difference case maps each pair to the applicable replacement test function
named for its slug in the plan; it may name a subtest of that function. An
exempt assertion needs a permitted `not-ported:` reason and a reviewer named
in `review`. When one Go target covers different sites, record the reviewed,
unconditional check
in each pair's `review` field. The tool checks presence, not whether a reviewer
approved a claim or the test body actually exercises each predicate.

`hack/parity-check results <phase>` also checks evidence. Pass paths to the
following files as needed; relative paths resolve from the caller's directory:

| Option | Evidence |
|---|---|
| `--go-json PATH` | Unfiltered `go test -json -count=1` output from the venue. Repeat for separate suites. The exact package and test must have `run` then `pass`, and the package must pass without cached output. |
| `--census PATH` | The source suite run against Go, with the baseline census columns: `path`, `line`, `column`, `depth`, `name`, `status`, `detail`. A due case must have `status=pass`. |
| `--launches PATH` | TSV with `path`, `line`, `column`, `name`, `binary`. Each row attributes one launch of the built Go binary (its absolute path) to the exact case key. Multiple launches per case are allowed. |

TSVs use a header and literal tabs with the same field escaping as the baseline.
Each gate checks its pending matrix targets and mapped replacements. The final
phase also checks all earlier ledgers and every replacement test named in the
plan, including unreferenced tests. The fixtures exercise the evidence formats;
the conformance census and attributed launch producers are not yet implemented.
The tool checks the records, not their provenance: use evidence from the gated
checkout and trusted venue, not a hand-authored record or an earlier run. An
uncached result alone cannot prove when the evidence was collected.
