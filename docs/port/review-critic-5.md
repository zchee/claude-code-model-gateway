# Fifth critic check of plan v5 (last delta check: phases 0 and 1)

- Recorded: 2026-11-25 23:07:59 JST (from `date`). Reviewer lane: plan-critic-5, model line `claude-gpt-6.2-astra-fast[1m]`.
- Snapshot reviewed: `.omc/drafts/model-gateway-go-port.v5.md` (sha256 prefix db775d60f79dfa19), against pin `a8501f251755`.
- Condensed record of the lane's single team message. Read-only lane; nothing executed.

## Verdict

REVISE for phases 0 and 1. T0.0 and T0.1 can start after the user's approval. T0.4's acceptance must change before its
off-venue check is run, and the parity inventory has a completion cycle later in the phase.

## Findings of the fourth check

| Item | State | Note |
|---|---|---|
| A | partly | the local target is gone, but the mandatory local `go test` of the guard package still starts a test binary before the guard is proven |
| B | resolved | the whole-tree export has the guide and both sibling modules; the lockfile and `npm ci` supply Ajv |
| C | resolved | version tests compile at bootstrap and first run in the venue task |
| D | partly | T0.9 and T1.0 are explicit, but T0.7b requires the mappings they produce, and the phase 1 Needs cells omit T1.0 |
| E | partly | the lock catches a deleted matrix row; the site schema cannot represent shared helper assertions, and a passing test name does not prove that each mapped assertion runs |
| minor | resolved | recorded in INV-07 §2 |
| earlier 1 | partly | persist the chosen generation in the start intent and recognize a matching live supervisor before the result is journaled |
| earlier 7 | partly | "a refusal exactly when armed-high" still rejects a valid exempt compaction (INV-02 §5.3; `request-worker.js:1362`); qualify it as an eligible non-compaction request |

## Remaining defects

1. MAJOR, T0.7b / T0.9 / T1.0 / AC-8: "the checker passes on T0.7's data" and "fails when a site has no target" cannot both hold before the handoffs. Split structural validation, phase-scoped mapping validation at the handoff, and passing-test evidence at the phase gate; test each mode.
2. MAJOR, §3.8 / T0.4 / AC-2: `go test ./internal/venue/...` on the gateway Mac contradicts §3.8 and §9; `TestMain` runs after package initialization, and a broken new guard would run tests there. Keep only the shell preflight on the Mac; run the test-binary negative control in a disposable non-venue and check its exact diagnostic and an unexecuted test sentinel. Check that the guard is the first statement under the default and the `process` tag.
3. MAJOR, T0.7b: "file, line, enclosing case" has no enclosing case for `test/tool-schema-compat.test.js:46-50`; `refusal()` serves distinct cases at :199, :311, :321 and :344. Record a site-to-case relation, map each case and site to an individually verified Go assertion or subtest, and add a shared-helper deletion fixture.
4. MAJOR, §4 / §9 phase 1: no row has T1.0 as a prerequisite (T1.13 needs only T0.1). Add the handoff to each implementing row.
5. MINOR, T0.7b / AC-8: `not-ported:<reason>` is accepted but has no closed reason list and no per-site approval.

## Citation sample

6 of 6 new citations of v5 held at `a8501f251755`.
