# Parity checks

Run `hack/parity-check` (or `hack/parity-check structure`) to reconcile the
source-case census, classification, static launches, matrix lock and phase
ledger, and assertion pairs. Use `--root DIR` before the mode to check another
checkout. An exempt case must have one entry of the same class in
`test/conformance/exemptions-approved.tsv`. `assertion-pairs.tsv` records all
cases and must reflect the reviewed `add` and `remove` edges. The editable
`assertion-sites.tsv` must match exactly the generated pairs for non-exempt
cases. After the seven key fields its columns are `target`, `review`, `due`.
Structure does not require targets, but each due case must retain a pair even
when removals are approved. Each pair is due in the case's phase unless
`test/conformance/deferrals-approved.tsv` has an approval for the same pair
and later phase. The baseline census must match
its recorded SHA-256, and the ownership table's package and phase-gate counts
must match classification. Static launch rows can include `case_path`,
`case_line`, and `case_column` to name the registration exactly; otherwise,
the launch path and full case name must identify a unique census case.

Review coordinated edits to the matrix and its lock/ledger, to exemptions or
deferrals and their approvals, and to the generated pairs and editable mapping.
Regenerate the pairs independently in the venue to catch edges deleted from
both pair files.

The difference table starts at its header and continues to the end of its
section. Blank lines and prose do not hide later table-shaped rows. Matrix
rows belong to contiguous tables with the matrix header; an unrelated table
outside those blocks is not part of the matrix.

`hack/parity-check mapping phase-2 internal/worker` checks pairs due in that
phase with the specified owning package and `gate=task`. Omit the package to
include the whole phase, including phase-gated pairs and pending matrix tests.
A Go target uses the package path below this module (the full import path is
also accepted), a dot, and the exact test name with any slash-delimited subtests.
Use the full module path to name a test in the module root.
Each Go target for a `go-port` or deferred pair needs mapping approval in
`review`; that field never approves a deferral. If a test is shared across
sites, the review must describe each unconditional check. The later phase's
mapping and results gates require a deferred pair's reviewed Go target or
approved assertion exemption; its case's original gate does not move.
A difference case must map at least one pair to an approved replacement test
at the case's gate. A mapped subtest may match a replacement function, but a
named replacement subtest must match exactly. Any exempt assertion needs a
permitted `not-ported:` reason and a reviewer named in `review`. The tool
checks for review text, not the reviewer or the test body's behavior.

`hack/parity-check results <phase>` also checks evidence. Pass `--commit SHA`
with the full commit ID of the gated checkout. Every supplied evidence file
starts with a JSON metadata line `{"run_id":"...","commit":"..."}`; all files
must have the same run identifier and match the expected commit. Relative
paths resolve from the caller's directory. Supply each file its gate requires:

| Option | Evidence |
|---|---|
| `--go-json PATH` | Unfiltered `go test -json -count=1` output from the venue. Repeat for separate suites. The exact package and test must have ordered start/run/pass events and a successful uncached package summary. |
| `--census PATH` | The source suite run against Go; TSV columns `path`, `line`, `column`, `depth`, `name`, `status`, `detail`. Each conformance case assigned to the phase must pass, even if its individual pairs are due later. |
| `--launches PATH` | TSV with `path`, `line`, `column`, `name`, `binary`, `binary_sha256`. Each row attributes one launch to the exact case key. The absolute binary path and lowercase SHA-256 must be identical throughout the run. |

After the metadata line, TSVs use a header and literal tabs with the same
field escaping as the baseline. Each results gate requires its pending matrix
and mapped tests plus every replacement scheduled for that phase in the plan's
`Replacement tests (phase)` column. Entries are separated by `<br>`; each is a
backquoted Go test target followed by its phase in parentheses. The preceding
prose column is not evidence. The final phase includes the earlier ledgers and
all plan replacements.

Future producers must assign a fresh identifier to each run, derive its commit
from the gated checkout, collect unfiltered Go events without caching, and
record the executable path and measured digest from the actual Go launch.
The checker matches those claims but cannot authenticate the producers, prove
when a run occurred, or inspect an executable in another venue.
