# Fourth critic check of plan v4 (delta check: phases 0 and 1)

- Recorded: 2026-11-25 22:58:59 JST (from `date`). Reviewer lane: plan-critic-4, model line `claude-gpt-6.2-astra-fast[1m]`.
- Snapshot reviewed: `.omc/drafts/model-gateway-go-port.v4.md` (sha256 prefix 872a0ec59f421734), against pin `4ffb2f672f0c`.
- Condensed record of the lane's single team message. Read-only lane; nothing executed.

## Verdicts

- Phases 0-1: REVISE. Do not approve `make test-local` on the gateway Mac or the phase 0 schedule as written.
- Whole plan: REVISE for this gate; keep the separate design reviews and go-aheads of appendix D.

## The nine phase 0-1 defects of the third check

| # | State | Note |
|---|---|---|
| 1 | partly | venue by default, but the local allowlist is not confinement (finding A) |
| 2 | partly | the waves obey the Needs columns, but T0.1's completion and the first test handoffs do not (C, D) |
| 3 | partly | expanded names, closed reasons, ledgers and negative fixtures are there; suite inputs and assertion coverage are incomplete (B, E) |
| 4 | resolved | one export path; the read-back matches `commands.js:2951-2953` |
| 5 | resolved | no conclusion about the real CLI from a fake |
| 6 | resolved | corpus job and two-process lock test are venue jobs |
| 7 | resolved | Linux evidence plus Darwin static checks; CI recorded as not run |
| 8 | resolved | phase branches start at the verified predecessor |
| 9 | resolved | byte-exact CRLF and tail cases, matching `request-worker.js:1701-1719` |

## Earlier dispositions

| # | State | Note |
|---|---|---|
| 1 | partly | after a crash that follows `--start` and precedes commit, step 0 still requires an empty port; make the preconditions aware of the journal stage for a listener the migration started |
| 2 | resolved | fixture shutdown assigned, built and proven |
| 4 | partly | case coverage does not establish assertion coverage (E) |
| 6 | resolved | hard-link backup plus one rename everywhere |
| 7 | partly | AC-10 rejects a valid first refusal: per INV-02 §5.3 the armed state needs positive above-trigger usage, not a completed turn; replay the source state table |
| 9 | resolved | AC-15a simulated, AC-15b real host |
| 10 | resolved | `port-owned` and `owner-unknown` separated |

## Findings

- A, CRITICAL, §3.8 / AC-2: `make test-local` admits unconfined file access and outbound connections; neither the import rule nor the call list denies them, and the tripwire fires after the write. Drop local test execution until OS confinement passes its negative controls; keep all tests in the venue.
- B, MAJOR, T0.0 / T0.7: the export omits `docs/src/content/docs/getting-started/model-gateway.md`, which the pinned `test/gateway-readiness.test.js:219` and `test/rc-compat-mode.test.js:280` read; the baseline would record artificial failures. Export the guide and require that a fresh export has no missing fixture input.
- C, MAJOR, T0.1 / T0.4: T0.1's "done when" needs the T0.4 runner, and T0.4 needs T0.1 merged. Move the execution of the version tests to T0.4's acceptance.
- D, MAJOR, §4 / §9: the tests for T0.2, T0.3 and T0.8 must be assigned by the merged T0.7 but are scheduled during the wave before those tasks, which is T0.7 itself. Add an explicit test-authoring and merge wave after T0.7.
- E, MAJOR, T0.7 / AC-8: removing one assertion row still passes if its case is named by a surviving row (distinct regex assertions in `gateway-readiness.test.js:221-225`). Reconcile an independent source-assertion inventory with the matrix and the ledgers; add that deletion as a negative fixture.
- Minor: the post-pin commit adds two table entries, not two `test()` sites; both documents also gain the new measurement date, and the test checks the date per row.

## Citation sample

8 of 8 new citations held against the pin.
