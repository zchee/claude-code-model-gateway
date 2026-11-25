# Third critic check of plan v3 (targeted: phases 0 and 1)

- Recorded: 2026-11-25 22:37:45 JST (from `date`). Reviewer lane: plan-critic-3, model line `claude-gpt-6.2-astra-fast[1m]`.
- Snapshot reviewed: `.omc/drafts/model-gateway-go-port.v3.md` (sha256 prefix 79e7b6c64d56a8e3).
- Condensed record of the lane's single team message. Read-only lane; nothing executed.

## Verdicts

- Phases 0-1: REVISE. T0.0 can be authorized separately with its export-path handoff corrected.
- Whole plan: REVISE. Offline migration, rollback and soak acceptance still have contradictions.

## Disposition of the second-round findings

| # | State | Remaining point |
|---|---|---|
| 1 | partly | rollback uses the stop marker after the mandatory Node `stop`, which always writes it (`commands.js:2951-2953`); capture the pre-stop intent separately. Step 0 refuses the in-progress journal that a rerun must resume |
| 2 | partly | T2.0 does not own the fixture-shutdown IPC; T2.0b does not say what the binary does on `fixture-shutdown` (INV-05 §3.3) |
| 3 | partly | test-file import checks and a caller-set venue flag do not enforce host isolation |
| 4 | partly | AC-8 accepts `unsupported:<reason>` without an exhaustive restriction; the matrix is not required to cover every in-scope assertion |
| 5 | partly | T0.0 writes `.cache/model-gateway-js/`, section 4 points executors at `.omc/research/source-4ffb2f6/`. Pin tree, 33 files and 13,056 lines verified |
| 6 | partly | step 1 omits the binary that step 2 replaces; section 10 still prescribes rename-aside; AC-15 injects a failure between two renames. One replacement algorithm with a recoverable prior binary |
| 7 | partly | AC-10 needs a stop reason and ordered worker identities joined to lifecycle events (INV-02 §5.3; INV-03 §7) |
| 8 | resolved | for the named omissions (D2, D4, D7, D10, D11, D13) |
| 9 | partly | phase 5 requires AC-15 before the T6.2 real-host checklist can exist; split simulated install from real-host acceptance |
| 10 | partly | AC-6 accepts `port-owned` or `owner-unknown` for an unresolved proxy pid; INV-03 §4.6 rules 8-9 distinguish a timeout from a completed lookup without a pid |

## Remaining phase 0-1 defects

1. CRITICAL, §3.8 / T0.4 / AC-2: the Tier 1 rule checks what test files import, not what product code and helpers reached from those tests do (the lifecycle lock reaches process inspection, `lifecycle-lock.js:35-40,75-78`). Run all new test binaries in the isolated venue by default; allow local execution only under proven confinement; test a helper-mediated prohibited operation inside the venue.
2. MAJOR, §9 / T0.0-T0.5: T0.1 is scheduled before T0.7 assigns independent tests; T0.3's Node and fuzz work runs before T0.4's venue enforcement; T0.5's atomic writer needs T0.3, which is missing from its Needs.
3. MAJOR, T0.7 / AC-8 / AC-11: 693 static sites are not the expanded identities (three sites generate six cases in `relay-stream-abort`). Require agreement by file, site and full case name, a non-empty assertion ledger per phase, and negative checker fixtures (missing expansion, omitted row, unauthorized exemption). Collecting names under Node needs T0.4 and the sibling test dependencies of INV-05 §3.3.
4. MAJOR, T0.0 / §4: one verified export path; show that a clean checkout can read a cited file there.
5. MAJOR, T0.6: a fake CLI only returns its programmed exit status; INV-04 §13 item 4 asks what the real Claude CLI does on the probe's 404. Keep the question open or get separately authorized real-CLI evidence.
6. MAJOR, phase 1 heading / T1.5 / T1.11 / §7: "Tier 1 only" contradicts the Tier 2 Node/Ajv corpus and real multi-process lock contention. Split into named Tier 2 gates; local tests consume committed goldens.
7. MAJOR, phase 0 gate / OD-8: AC-1 requires `xcode-27` CI even when no push or Darwin venue is approved. Define the deferred Darwin check, or require the venue at phase 0.
8. MAJOR, §9 branches: `port/phase-N` from main loses the phase 0 commits when the merge to main has not happened. Branch from the verified tip of the previous phase, or make the merge a prerequisite.
9. MINOR, T1.13: interior line breaks are joined with LF but the frame's original delimiter is appended (`request-worker.js:1701-1714`); require byte-exact CRLF-frame and unterminated-tail cases.

## Citation sample

8 of 10 fully supported. Partial: T1.13 / `request-worker.js:1675-1702`. Unsupported: T0.6 / INV-04 §13 item 4 as a basis for the fake CLI.
