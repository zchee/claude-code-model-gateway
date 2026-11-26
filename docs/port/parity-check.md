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

For `--census` and `--launches`, use both files from one [conformance export](../../test/conformance/adapter/README.md#evidence-output) produced for the gated `PHASE=phase-N` and accepted by `hack/conformance-export verify DIR` from a clean checkout at the manifest's commit: either from the same hosted `gha-<run>-<attempt>` workflow run as the Go test JSON, or from a dedicated venue run together with Go test JSON acquired under that same venue run ID. Never mix identifiers across producers.

After the metadata line, TSVs use a header and literal tabs with the same
field escaping as the baseline. Each results gate requires its pending matrix
and mapped tests plus every replacement scheduled for that phase in the plan's
`Replacement tests (phase)` column. Entries are separated by `<br>`; each is a
backquoted Go test target followed by its phase in parentheses. The preceding
prose column is not evidence. The final phase includes the earlier ledgers and
all plan replacements.

### Collect hosted Go evidence

Run either collection procedure from the main checkout of a standard clone; both
refuse linked worktrees, separate Git directories, and bare repositories.
At the gated commit, list
completed runs of `.github/workflows/test.yaml` (`Build, lint, and test`)
filtered by `--workflow test.yaml` — `--event push` (which also produces the
hosted conformance export when `.github/conformance-phase` names `$phase`) or
`--event workflow_dispatch` (triggered with `-f conformance_phase=$phase` on
the pushed branch). Never accept `pull_request` runs as gate evidence. Select a
successful run whose hosted jobs all ran in the same attempt. If rerunning, use
**Re-run all jobs**, not a single-job rerun. Keep the separate Linux
development-venue logs required by the verification plan; use the hosted
artifacts as the Go JSON (and, for phases with due conformance cases, the
conformance export) inputs below. No `retention-days` is set in
`.github/workflows/test.yaml`, so the repository's artifact retention policy
applies uniformly to all uploaded artifacts.

In both procedures below,
downloads are guarded in the same command by resolving
`.github/workflows/test.yaml` to its workflow ID with `gh api` and checking
`gh run view --attempt --json` so the command fails unless the selected run
attempt is `completed` with conclusion `success` and matches that
`workflowDatabaseId`, event (`push` or `workflow_dispatch`), commit, run ID, and
attempt. Downloads are staged in a directory resolved physically outside the
checkouts and checked by directory identity (`test -ef`) across every ancestor
(refusing if `TMPDIR` resolves to or inside a checkout, and exporting `TMPDIR`
as the physical staging root before any helper runs), verification runs from a
separate clean detached worktree of the gated commit created outside the
repository for that purpose, and verified files are copied into the main
checkout's `.omc/artifacts/verify/` only afterwards. A trap removes the
temporary worktree and staging directory on exit so repeating the procedure for
a later attempt or gate works without manual cleanup
(`hack/conformance-export verify` disables user-global ignore files and rejects
a checkout with untracked files, including `.omc/`).

#### Go-only phases (`phase-0`, `phase-1`)

```sh
phase=phase-0
event=push # or workflow_dispatch
commit=$(git rev-parse HEAD)
gh run list --workflow test.yaml --event "$event" --commit "$commit" --status completed \
  --json workflowName,name,databaseId,attempt,headSha,conclusion,url
```

Enter that run's `databaseId` and `attempt`, separated by a space, at `read`.
Inspect the run summary to confirm both `checks` legs are present and passed.

```sh
read -r run attempt
gh run view "$run" --attempt "$attempt" --exit-status
```

Guard the selected run attempt, download into a physical staging directory
outside the repository, verify all four Go headers and the results gate from a
separate clean worktree of `$commit`, and archive into the main checkout's
`.omc/artifacts/verify/` only if every check passes:

```sh
(
  set -eu
  within_any_checkout() {
    dir=$1
    shift
    while :; do
      for checkout in "$@"; do
        if [ -n "$checkout" ] && test "$dir" -ef "$checkout"; then
          return 0
        fi
      done
      case "$dir" in
        /|//|'') break ;;
      esac
      parent=${dir%/*}
      [ -n "$parent" ] || parent=/
      [ "$parent" != "$dir" ] || break
      dir=$parent
    done
    return 1
  }

  refuse_layout() {
    printf 'standard git layout required: run from the main checkout\n' >&2
    exit 1
  }
  [ -z "${GIT_DIR+x}${GIT_WORK_TREE+x}${GIT_COMMON_DIR+x}" ] || refuse_layout
  core_wt_rc=0
  git config --get-all core.worktree >/dev/null 2>&1 || core_wt_rc=$?
  [ "$core_wt_rc" -eq 1 ] || refuse_layout

  # Keep any trailing newline in the checkout name when capturing Git's path.
  top=$(git rev-parse --path-format=absolute --show-toplevel 2>/dev/null && printf .) || refuse_layout
  main_real=${top%??}
  [ -d "$main_real/.git" ] && [ ! -L "$main_real/.git" ] || refuse_layout
  git_dir=$(git rev-parse --path-format=absolute --git-dir 2>/dev/null) || refuse_layout
  common_dir=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || refuse_layout
  test "$git_dir" -ef "$common_dir" && test "$main_real/.git" -ef "$common_dir" || refuse_layout

  if [ ! -d "${TMPDIR:-/tmp}" ]; then
    printf 'TMPDIR must name an existing directory: %s\n' "${TMPDIR:-/tmp}" >&2
    exit 1
  fi
  tmp_base=$(CDPATH='' cd -P -- "${TMPDIR:-/tmp}" && pwd -P)
  if within_any_checkout "$tmp_base" "$main_real"; then
    printf 'TMPDIR must resolve outside the repository: %s\n' "$tmp_base" >&2
    exit 1
  fi

  workflow_meta=$(gh api 'repos/{owner}/{repo}/actions/workflows/test.yaml')
  if ! workflow_id=$(printf '%s\n' "$workflow_meta" | jq -esr \
    'if length == 1
        and (.[0] | type == "object")
        and (.[0].id | type == "number")
        and ((.[0].id | tostring) | test("^[1-9][0-9]*$"))
        and .[0].path == ".github/workflows/test.yaml"
     then .[0].id | tostring
     else empty
     end') || [ -z "$workflow_id" ]; then
    printf 'failed to resolve workflow id for .github/workflows/test.yaml\n' >&2
    exit 1
  fi

  run_meta=$(gh run view "$run" --attempt "$attempt" \
    --json databaseId,attempt,status,conclusion,event,headSha,workflowDatabaseId)
  if ! printf '%s\n' "$run_meta" | jq -es \
    --arg run "$run" \
    --arg attempt "$attempt" \
    --arg workflow_id "$workflow_id" \
    --arg event "$event" \
    --arg commit "$commit" \
    'length == 1
     and ($run | test("^[1-9][0-9]*$"))
     and ($attempt | test("^[1-9][0-9]*$"))
     and ($workflow_id | test("^[1-9][0-9]*$"))
     and ($event == "push" or $event == "workflow_dispatch")
     and (.[0].databaseId | type == "number")
     and ((.[0].databaseId | tostring) == $run)
     and (.[0].attempt | type == "number")
     and ((.[0].attempt | tostring) == $attempt)
     and (.[0].workflowDatabaseId | type == "number")
     and ((.[0].workflowDatabaseId | tostring) == $workflow_id)
     and .[0].status == "completed"
     and .[0].conclusion == "success"
     and .[0].event == $event
     and .[0].headSha == $commit' >/dev/null; then
    printf 'selected run attempt failed guard: run=%s attempt=%s workflow_id=%s event=%s commit=%s\n' \
      "$run" "$attempt" "$workflow_id" "$event" "$commit" >&2
    exit 1
  fi

  work_root=$(mktemp -d "${tmp_base%/}/parity-check-$phase-gha-$run-$attempt.XXXXXXXX")
  work_root=$(CDPATH='' cd -P -- "$work_root" && pwd -P)
  verify_wt=''
  trap '
    if [ -n "$verify_wt" ] && [ -e "$verify_wt" ]; then
      git -C "$main_real" worktree remove --force "$verify_wt" >/dev/null 2>&1 || true
    fi
    if [ -n "$work_root" ] && [ -d "$work_root" ]; then
      rm -rf -- "$work_root"
    fi
  ' EXIT

  if within_any_checkout "$work_root" "$main_real"; then
    printf 'staging root must resolve outside the repository: %s\n' "$work_root" >&2
    exit 1
  fi
  TMPDIR=$work_root
  export TMPDIR

  stage="$work_root/stage"
  mkdir -p -- "$stage"
  stage_real=$(CDPATH='' cd -P -- "$stage" && pwd -P)

  gh run download "$run" --name "go-test-ubuntu-26.04-$attempt" --dir "$stage_real/ubuntu-26.04"
  gh run download "$run" --name "go-test-xcode-27-$attempt" --dir "$stage_real/xcode-27"

  verify_wt="$work_root/verify-wt"
  git -C "$main_real" worktree add --detach "$verify_wt" "$commit"
  verify_wt_real=$(CDPATH='' cd -P -- "$verify_wt" && pwd -P)
  if within_any_checkout "$stage_real" "$verify_wt_real" "$main_real"; then
    printf 'staging directory must resolve outside the verifying checkout: %s\n' "$stage_real" >&2
    exit 1
  fi

  for runner in ubuntu-26.04 xcode-27; do
    for suite in plain process; do
      file="$stage_real/$runner/go-test-$suite.jsonl"
      if ! head -n 1 "$file" | jq -es --arg commit "$commit" --arg run_id "gha-$run-$attempt" \
        'length == 1 and .[0].commit == $commit and .[0].run_id == $run_id' >/dev/null; then
        printf 'wrong or missing evidence metadata: %s\n' "$file" >&2
        exit 1
      fi
    done
  done

  "$verify_wt_real/hack/parity-check" results "$phase" --commit "$commit" \
    --go-json "$stage_real/ubuntu-26.04/go-test-plain.jsonl" \
    --go-json "$stage_real/ubuntu-26.04/go-test-process.jsonl" \
    --go-json "$stage_real/xcode-27/go-test-plain.jsonl" \
    --go-json "$stage_real/xcode-27/go-test-process.jsonl"

  archive="$main_real/.omc/artifacts/verify/$phase"
  mkdir -p -- "$archive"
  out=$(mktemp -d "$archive/gha-$run-$attempt.XXXXXXXX")
  cp -R "$stage_real/." "$out/"
  printf 'archived: %s\n' "$out"
)
```

#### Gate phases with conformance (`phase-2` through `phase-5`)

Only the final `conformance-export-$phase-$attempt` artifact published by the
downstream `publish-conformance-export` job (after both matrix legs succeed in
the same attempt and both attempt-qualified Go artifacts exist) is gate
evidence. Never use `conformance-export-candidate-$phase-$attempt` as gate
evidence.

```sh
phase=phase-2
event=push # or workflow_dispatch
commit=$(git rev-parse HEAD)
gh run list --workflow test.yaml --event "$event" --commit "$commit" --status completed \
  --json workflowName,name,databaseId,attempt,headSha,conclusion,url
```

Enter that run's `databaseId` and `attempt`, separated by a space, at `read`,
and inspect the run summary to confirm both `checks` legs and
`publish-conformance-export` passed in that attempt.

```sh
read -r run attempt
gh run view "$run" --attempt "$attempt" --exit-status
```

Guard the selected run attempt, download the two Go artifacts and the final
conformance export into a physical staging directory outside the repository,
verify the export, manifest, all four Go headers, and the results gate from a
separate clean worktree of `$commit`, and archive into the main checkout's
`.omc/artifacts/verify/` only if every check passes:

```sh
(
  set -eu
  within_any_checkout() {
    dir=$1
    shift
    while :; do
      for checkout in "$@"; do
        if [ -n "$checkout" ] && test "$dir" -ef "$checkout"; then
          return 0
        fi
      done
      case "$dir" in
        /|//|'') break ;;
      esac
      parent=${dir%/*}
      [ -n "$parent" ] || parent=/
      [ "$parent" != "$dir" ] || break
      dir=$parent
    done
    return 1
  }

  refuse_layout() {
    printf 'standard git layout required: run from the main checkout\n' >&2
    exit 1
  }
  [ -z "${GIT_DIR+x}${GIT_WORK_TREE+x}${GIT_COMMON_DIR+x}" ] || refuse_layout
  core_wt_rc=0
  git config --get-all core.worktree >/dev/null 2>&1 || core_wt_rc=$?
  [ "$core_wt_rc" -eq 1 ] || refuse_layout

  # Keep any trailing newline in the checkout name when capturing Git's path.
  top=$(git rev-parse --path-format=absolute --show-toplevel 2>/dev/null && printf .) || refuse_layout
  main_real=${top%??}
  [ -d "$main_real/.git" ] && [ ! -L "$main_real/.git" ] || refuse_layout
  git_dir=$(git rev-parse --path-format=absolute --git-dir 2>/dev/null) || refuse_layout
  common_dir=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || refuse_layout
  test "$git_dir" -ef "$common_dir" && test "$main_real/.git" -ef "$common_dir" || refuse_layout

  if [ ! -d "${TMPDIR:-/tmp}" ]; then
    printf 'TMPDIR must name an existing directory: %s\n' "${TMPDIR:-/tmp}" >&2
    exit 1
  fi
  tmp_base=$(CDPATH='' cd -P -- "${TMPDIR:-/tmp}" && pwd -P)
  if within_any_checkout "$tmp_base" "$main_real"; then
    printf 'TMPDIR must resolve outside the repository: %s\n' "$tmp_base" >&2
    exit 1
  fi

  workflow_meta=$(gh api 'repos/{owner}/{repo}/actions/workflows/test.yaml')
  if ! workflow_id=$(printf '%s\n' "$workflow_meta" | jq -esr \
    'if length == 1
        and (.[0] | type == "object")
        and (.[0].id | type == "number")
        and ((.[0].id | tostring) | test("^[1-9][0-9]*$"))
        and .[0].path == ".github/workflows/test.yaml"
     then .[0].id | tostring
     else empty
     end') || [ -z "$workflow_id" ]; then
    printf 'failed to resolve workflow id for .github/workflows/test.yaml\n' >&2
    exit 1
  fi

  run_meta=$(gh run view "$run" --attempt "$attempt" \
    --json databaseId,attempt,status,conclusion,event,headSha,workflowDatabaseId)
  if ! printf '%s\n' "$run_meta" | jq -es \
    --arg run "$run" \
    --arg attempt "$attempt" \
    --arg workflow_id "$workflow_id" \
    --arg event "$event" \
    --arg commit "$commit" \
    'length == 1
     and ($run | test("^[1-9][0-9]*$"))
     and ($attempt | test("^[1-9][0-9]*$"))
     and ($workflow_id | test("^[1-9][0-9]*$"))
     and ($event == "push" or $event == "workflow_dispatch")
     and (.[0].databaseId | type == "number")
     and ((.[0].databaseId | tostring) == $run)
     and (.[0].attempt | type == "number")
     and ((.[0].attempt | tostring) == $attempt)
     and (.[0].workflowDatabaseId | type == "number")
     and ((.[0].workflowDatabaseId | tostring) == $workflow_id)
     and .[0].status == "completed"
     and .[0].conclusion == "success"
     and .[0].event == $event
     and .[0].headSha == $commit' >/dev/null; then
    printf 'selected run attempt failed guard: run=%s attempt=%s workflow_id=%s event=%s commit=%s\n' \
      "$run" "$attempt" "$workflow_id" "$event" "$commit" >&2
    exit 1
  fi

  work_root=$(mktemp -d "${tmp_base%/}/parity-check-$phase-gha-$run-$attempt.XXXXXXXX")
  work_root=$(CDPATH='' cd -P -- "$work_root" && pwd -P)
  verify_wt=''
  trap '
    if [ -n "$verify_wt" ] && [ -e "$verify_wt" ]; then
      git -C "$main_real" worktree remove --force "$verify_wt" >/dev/null 2>&1 || true
    fi
    if [ -n "$work_root" ] && [ -d "$work_root" ]; then
      rm -rf -- "$work_root"
    fi
  ' EXIT

  if within_any_checkout "$work_root" "$main_real"; then
    printf 'staging root must resolve outside the repository: %s\n' "$work_root" >&2
    exit 1
  fi
  TMPDIR=$work_root
  export TMPDIR

  stage="$work_root/stage"
  mkdir -p -- "$stage"
  stage_real=$(CDPATH='' cd -P -- "$stage" && pwd -P)

  gh run download "$run" --name "go-test-ubuntu-26.04-$attempt" --dir "$stage_real/ubuntu-26.04"
  gh run download "$run" --name "go-test-xcode-27-$attempt" --dir "$stage_real/xcode-27"
  gh run download "$run" --name "conformance-export-$phase-$attempt" --dir "$stage_real/conformance-export"

  verify_wt="$work_root/verify-wt"
  git -C "$main_real" worktree add --detach "$verify_wt" "$commit"
  verify_wt_real=$(CDPATH='' cd -P -- "$verify_wt" && pwd -P)
  if within_any_checkout "$stage_real" "$verify_wt_real" "$main_real"; then
    printf 'staging directory must resolve outside the verifying checkout: %s\n' "$stage_real" >&2
    exit 1
  fi

  "$verify_wt_real/hack/conformance-export" verify "$stage_real/conformance-export"

  if ! jq -es --arg commit "$commit" --arg run_id "gha-$run-$attempt" --arg phase "$phase" \
    'length == 1
     and .[0].producer == "hosted"
     and .[0].commit == $commit
     and .[0].run_id == $run_id
     and .[0].phase == $phase
     and .[0].clean_tree == true
     and .[0].verdict == "pass"
     and .[0].status == 0
     and .[0].selection == ("conformance<=" + $phase)' \
    "$stage_real/conformance-export/manifest.json" >/dev/null; then
    printf 'wrong or missing conformance manifest metadata: %s\n' "$stage_real/conformance-export/manifest.json" >&2
    exit 1
  fi

  for runner in ubuntu-26.04 xcode-27; do
    for suite in plain process; do
      file="$stage_real/$runner/go-test-$suite.jsonl"
      if ! head -n 1 "$file" | jq -es --arg commit "$commit" --arg run_id "gha-$run-$attempt" \
        'length == 1 and .[0].commit == $commit and .[0].run_id == $run_id' >/dev/null; then
        printf 'wrong or missing evidence metadata: %s\n' "$file" >&2
        exit 1
      fi
    done
  done

  "$verify_wt_real/hack/parity-check" results "$phase" --commit "$commit" \
    --go-json "$stage_real/ubuntu-26.04/go-test-plain.jsonl" \
    --go-json "$stage_real/ubuntu-26.04/go-test-process.jsonl" \
    --go-json "$stage_real/xcode-27/go-test-plain.jsonl" \
    --go-json "$stage_real/xcode-27/go-test-process.jsonl" \
    --census "$stage_real/conformance-export/census.tsv" \
    --launches "$stage_real/conformance-export/launches.tsv"

  archive="$main_real/.omc/artifacts/verify/$phase"
  mkdir -p -- "$archive"
  out=$(mktemp -d "$archive/gha-$run-$attempt.XXXXXXXX")
  cp -R "$stage_real/." "$out/"
  printf 'archived: %s\n' "$out"
)
```

For phases with due conformance cases, take `--census` and `--launches` together from the verified final export (`conformance-export-<phase>-<attempt>`),
not from individual diagnostics or a candidate artifact. The export contract in [the work plan, §6](model-gateway-go-port.md#6-verification-steps)
requires a producer manifest and a full passing binary run through that phase; matching metadata lines alone do not
establish either. For the hosted run above, reject
files for a different commit or a different run ID (`gha-<run>-<attempt>`),
a single-job rerun, a `pull_request` run, and concatenated or edited files. Suite failures may still
produce Go test artifacts (and a Linux-only pass while macOS fails may produce a
candidate artifact); they never produce the final `conformance-export-<phase>-<attempt>`
artifact and do not count as passing evidence.

Producers must collect unfiltered Go events without caching and record the
executable path and measured digest from the actual Go launch for conformance.
The checker matches those claims but cannot authenticate the producers, prove
when a run occurred, or inspect an executable in another venue.
