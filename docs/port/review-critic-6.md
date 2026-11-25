# Sixth critic check: tasks T0.4, T0.7 and T0.7b of plan v6 against the fifth check

- Recorded: 2026-11-26 00:14:34 JST (from `date`). Reviewer lane: plan-critic-6, model line `claude-gpt-6.2-astra-fast[1m]`
  (agent type `gpt62-astra-fast-critic`; the line matches the pin).
- Reviewed: `.omc/plans/model-gateway-go-port.md` (v6, sha256 `737c0956001af57b…`, identical to
  `.omc/drafts/model-gateway-go-port.v6.md`), against `review-critic-5.md` and pin `a8501f251755`.
- Scope given by the user: check T0.4, T0.7 and T0.7b of v6 against the findings of the fifth check; proposals only,
  the plan is not edited in this run.
- The lane's report time: 2026-11-26 00:09:49 JST (its own `date` call). Read-only lane: no SSH, port probe, test,
  Node/npm, gateway command, credential read, file edit or process control.
- Sections "Verdict" to "Limits" are the lane's final message, unchanged apart from Markdown layout. The last section
  is the lead's spot check.

## Verdict

**REVISE.** None of the three tasks is authorized by this review.

| Task | Verdict | Reason |
|---|---|---|
| T0.4 | can start after the listed changes | Keep all binary evidence on gateway-free Linux; prove that the negative fixture actually reached `TestMain`, and establish the phase-start digest before it is needed. |
| T0.7 | can start after the listed changes | The baseline needs an auditable census of nested registrations, a PATH that excludes real Claude, and an evidence handoff to the checker. |
| T0.7b | can start after the listed changes | The checker as specified can approve a deleted helper pair, overlook child-script assertions, and reject valid incomplete later phases. |

The named Needs edges (T0.4 → T0.7 → T0.7b → T0.9) and the ordering of section 9 are sound; do not fix the checker
defects by moving T0.9 earlier.

## Findings of the fifth check

| Item | State | v6 evidence (plan lines) |
|---|---|---|
| 1, checker cycle | partly resolved | 598-599, 611, 712: three modes remove the original T0.7b/T0.9 cycle. The handoff needs package scope; the gate still says "every pair" without limiting it to due work. |
| 2, local test binary | resolved as a placement rule | 461-468, 596, 706, 810-811 forbid local binary execution and require default and `process` checks. The remote proof has a separate false-positive path (finding 1 below). |
| 3, shared helper | partly resolved | 598-599: the schema can represent pairs, but the specified predicate only proves each *site* has a case, and a passing combined subtest does not identify which expectation ran. |
| 5, exemption | resolved as policy | 598, 712 give a closed list and pair-level approval; the checker must enforce the approval and validate the referenced difference. |
| A | resolved as written policy | 464-468, 596, 706: shell only on the Mac; binary negative control on Linux. |
| D | partly resolved | 598-599, 611-628, 826: the prerequisite graph is acyclic; the remaining mismatch is phase-wide mapping versus per-package completion. |
| E | partly resolved | 597-598, 712: matrix lock and case/site keys are improvements; shared-pair deletion and proof of individual obligations remain open. |

Outside the three tasks, one line each: defect 4, T1.0 appears in the implementing rows (612-628; 616 is a corpus
job). Earlier 1 and 7: the requested qualifications are present at 396, 400 and 714; no broader review of migration or
sentry behavior.

## New findings (high confidence; no critical finding)

1. **MAJOR: false green for the binary guard; unsafe local proof if wiring regresses.** 461-468, 596, 706: both the
   shell preflight and the binary print `not a test venue`. After removing the attestation, a normal `make test` can
   satisfy non-zero + diagnostic + absent sentinel without starting the fixture. Conversely, an unverified local
   `make test` is the very path that must not dispatch a binary. Prove dispatch on disposable Linux and use only the
   standalone shell check on the Mac. AC-2(a)'s unconditional missing-attestation failure also contradicts the CI
   alternative at 451-453.
2. **MAJOR: the first tripwire baseline is required before its implementation exists.** 478-479, 595-596, 706(d):
   T0.1 names the target, T0.4 delivers it. A hash first taken in T0.4 cannot prove that nothing changed earlier in
   phase 0. Inherited, not a v6 regression.
3. **MAJOR: baseline census and inputs are not reproducible from the completion artifacts.** 597-598: `693 static
   sites` is exactly 693 line-leading `test(` declarations in 57 files, **plus five** `t.test(` sites
   (`lmstudio:552,589`, `local-llama:276`, `proxy-providers:605,664`, all `.test.js`). `local-llama:269-281` registers a
   parent with three differently named children. The plan says "every case" but specifies neither a retained
   independent declaration/event census nor a completeness rule for a partial runner or file failure. Section 9
   prohibits direct Node; the baseline needs a guarded target distinct from Go conformance. Separately,
   `picker-guard.test.js:110-130` discovers `claude` through `which`, bypassing the preload's explicit missing binary;
   absence of gateway state does not imply absence of Claude from PATH.
4. **MAJOR: removing an obligation can leave every structural predicate true.** 598: remove `(case A, helper site X)`
   where case B also reaches X. X still has a case, both cases stay classified, and matrix ids do not change. Manual
   repair only for wholly unattributed sites does not repair the omission of one edge. A single outer JavaScript parse
   also sees string literals, not `assert.equal` in the child program at `readiness-gate.test.js:252,269,276,291,305`
   or the inline `require('node:assert/strict').equal` in `recovery-lifecycle.test.js:56-82`. Finally
   `lifecycle-lock.test.js:77` contains both an outer assertion and an `assert.fail` which must *not* execute. A line
   number or a passed test name alone is not the behavioral obligation.
5. **MAJOR: mapping scope, result identity and due dates disagree.** 598, 611, 712: `mapping <phase>` requires all
   pairs in the phase; T1.0 wants independently completed packages. AC-8 reads as requiring all source pairs at each
   gate although later targets do not exist; Go-attributed conformance cannot be due before the adapter decision at
   635-636. The results predicate also omits the replacement-test and unsupported-row conditions claimed by AC-8. A
   target needs the Go package plus the full subtest path; cached output or a passing subtest with an untaken branch is
   not per-assertion execution evidence.
6. **MINOR: the helper example confuses call sites and cases.** 598: four correct invocation locations, but 311 and
   321 both belong to the registration at 285. The distinct registrations are 184, 285, 331; the five helper assertion
   sites at 46-50 produce **15**, not 20, distinct case/site pairs.
7. **MINOR: missing inventory handoff.** 635 says "the direct spawn sites that T0.7 listed"; the T0.7 row at 597 does
   not require that output. Record direct launch sites and their owning cases while classifying.

## Proposed plan changes

Exact substitute clauses; unmentioned text and the Needs edges stay. Proposals only: not applied.

- **451-455, venue predicate:** "Provision only the user-approved remote Linux host/account or an identified hosted CI
  runner. Reject the developer machine and local destinations before provisioning; on the destination check the
  password-database home for gateway state and check both listener ports without contacting a gateway. Unknown checks
  fail closed. Require an attestation in that home, except on verified hosted CI; an environment variable alone is
  insufficient. If any condition fails, stop without writing an attestation or running a test."
- **464-468, replace the negative-control paragraph:** "On a gateway-free, non-CI Linux account reserved exclusively
  for the control, build a fresh fixture for each build tag while attested. Invoke each fixture binary directly through
  the remote control harness, not the make preflight: first prove it exits successfully and creates its per-run
  sentinel. Remove the sentinel, withdraw attestation, then require the same fixture to exit with the guard's expected
  status and exact diagnostic and leave no sentinel. Restore the prior attestation on exit, recheck the venue before
  normal tests, and require reprovisioning after interrupted cleanup. Exercise each make target's refusal separately on
  this disposable host and verify that no build/test/Node command ran. On the developer Mac run only the standalone
  shell preflight; do not dispatch a test target to prove it is safe."
- **596, Done when:** "The user approved the venue; provisioning and the checks of §3.8 succeeded on it. The Linux
  version and venue-package tests pass, both negative-control fixtures pass, and the lint rejects missing and non-first
  guards for both tag sets. The standalone developer-host shell preflight returns the expected refusal. No local build
  or process execution is used as evidence for a test."
- **706 (a-b):** "Apply §3.8's same predicate and hosted-CI exception. Record isolated refusals for each missing
  prerequisite, for each protected make target, and the positive and negative binary controls for both tag sets. No
  test binary starts on the developer machine."
- **478-479 and 706 (d), phase-start baseline:** "Before any phase-0 work, the lead captures the approved settings and
  PID-record paths using a read-only digest manifest (path, absent/present, SHA-256; no contents or credential files).
  The phase-0 gate compares that same manifest with the same rules; subsequent phases use the committed tripwire before
  their first task and at their gate. Without a phase-start baseline the verifier must report that interval unverified,
  not backdate it."
- **597, replace the expanded-name sentences:** "Use a venue-gated `make baseline-node` via the remote wrapper.
  Materialize and verify T0.0's whole pinned tree **in that venue**; run locked `npm ci` in the exported plugin, then
  its unmodified package test script with the isolation preload, from an allowlisted environment/PATH on which no real
  `claude` resolves. Record pin, tool versions, suite exit, and terminal runner events keyed by source path,
  registration line/column and full parent/child name and status. Return the machine-readable census to the inventory
  lane and store it independently of the classification rows. Record direct gateway launch sites and their owning cases
  for the adapter handoff."
- **597, Done when:** "All 57 files finished discovery; reconcile the 693 top-level and five nested declaration sites
  with every reported expansion, including skips. Record reasons for skipped or individually failed baseline cases; an
  abort, incomplete registration, missing input, unknown identity or unexplained discrepancy blocks completion. The
  independent classification and exception review is recorded."
- **598, replace the assertion collector and the `structure` predicates:** "In the venue, use a pinned JS parser and
  the preserved expanded-case census to produce the expected relation keyed by case and assertion source
  path/line/column. Include nested tests, helper and callback edges, and JS passed to child interpreters or preloads;
  review explicit case/site additions for generated or unresolved code without evaluating that code. At the pin, the
  calls at 199, 311, 321 and 344 reach five `refusal` assertions from three registered tests (184, 285, 331). Store
  reviewed attribution overrides independently of the editable target mapping. `structure` compares the exact expected
  and submitted pair sets in both directions, enforces unique keys, classification/matrix coverage, the matrix lock,
  closed reasons and valid difference references, but does not require future targets. Each mode rejects its existing
  fixtures; also reject a missing embedded assertion and a deleted shared pair whose site remains covered by another
  case."
- **598, replace the mapping/results predicates:** "`mapping <phase> [package]` checks all due go-port pairs within the
  selected scope for a reviewed package-qualified full Go subtest name or an individually approved exemption. A shared
  subtest must identify a reviewed unconditional check for each obligation; if a check is conditional, give it a
  distinct observable subtest or a recorded assertion event. A negative assertion maps to an observable check that the
  forbidden effect did not occur. `results <phase>` requires fresh, uncached, non-skipped passing evidence for each due
  Go target, the replacement for each due difference, and a passing result with an attributed Go launch for each due
  conformance case. Require approval for each due unsupported row. Reject an absent, skipped, wrong-package, or
  unobserved target and an unapproved exemption."
- **611, handoff completion:** "Each package handoff passes `mapping phase-1 <package>` for its assigned pairs; after
  all package handoffs merge, `mapping phase-1` must pass for the entire phase."
- **712, AC-8 scope:** "Run global `structure` at every gate. Run `mapping <phase>` and `results <phase>` over the
  nonempty due ledger only; later rows may remain open until their assigned gate. Do not assign Go-launch conformance
  evidence before the adapter decision. At phase 5 also check every earlier ledger and require no open row."

## Citation sample

8 of 8 location or source-data checks held: the helper body, the four individual calls, the five adjacent readiness
matches as a group, three relay declarations expanding over two values, and the pinned Ajv manifest/lock entry. The
plan's inference that the four calls represent four cases did **not** hold.

## Limits

Linux SSH permission, account eligibility, hosted-runner identity, toolchain availability, actual source-suite results
and parser output remain execution evidence, not facts verified by this review. This is a review of three tasks, not
an approval of the other phases.

## Lead's spot check of the review (read from the Git objects of the pin, 2026-11-26)

| Claim of the review | Check | Result |
|---|---|---|
| five nested `t.test(` sites besides the 693 | `perl` count over the 57 `test/*.test.js` files | holds: 5 (`lmstudio` 2, `local-llama` 1, `proxy-providers` 2) |
| "693 line-leading `test(` declarations" | same count | 693 is the number of `test(` at line start after optional indentation: 662 unindented and 31 indented. The total holds; "line-leading" and "top-level" are imprecise for the 31 |
| the calls at 311 and 321 belong to one registration | `test/tool-schema-compat.test.js` registrations at 184, 285, 331; calls at 199, 311, 321, 344 | holds: three cases, not four |
| `picker-guard.test.js` finds a real `claude` on PATH | lines 110-121 (`which claude`, skip only when absent) | holds |

Not checked by the lead: findings 4 and 5 (the child-program assertion sites and the mapping/results semantics).
