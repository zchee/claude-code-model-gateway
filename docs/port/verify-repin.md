# Verification of the move to a8501f251755

- Recorded: 2026-11-25 23:10:57 JST (from `date`). Lane: repin-verifier, model line `claude-gpt-6.2-sol-fast[1m]`. Read-only; nothing executed.
- Scope: `07-repin.md`, the five inventories and the work plan after the move. Verdict of the lane: FAIL, with the
  list below. Every item was then checked against the Git objects of `a8501f251755` by the lead and corrected; the
  corrections were not verified by another lane.

## What the lane confirmed

- Both commits and both plugin trees resolve; the root tree of the new pin has 1,005 files. Counts match: 33 files and
  13,060 lines of JavaScript in `lib/`, `bin/`, `hooks/`; 57 test files with 21,680 lines; 65 files and 23,542 lines in
  `test/`; `lib/runtime.js` 801 lines; `test/context-window.test.js` 2,021 lines; 693 static test sites unchanged.
  `git diff -U0` confirms both line maps (4 plugin files, 21 insertions, 14 deletions).
- 216 explicit citations of `lib/runtime.js`, the policy shorthand, the 12 cells of the hand-written rows table and
  all 18 citations of the context-window test were read against the pinned source.
- INV-04 §3.2: all 49 generated Vertex keys in 11 groups match in order, window, advertised cap and source string; no
  key collides with a hand-written or Muse key; the spread is at `lib/runtime.js:229`.
- Plan header and T0.0: commit, both tree hashes, counts, the three files outside the plugin and the five test lines
  cited for them; no fourth outside input in the test tree.

## What it found, and the correction

| Where | Finding | Correction |
|---|---|---|
| INV-04 §11, table rows at lines 869-871 and 874-877 | seven shorthand citations of `lib/runtime.js` still had the old numbering (the rows also name `bin/model-gateway.js`, which the rewrite took for the governing file) | now `:643-646`, `:648-671`, `:682-693`, `:735-747`, `:749-764`, `:559-561`, `:766-773` |
| `07-repin.md` | it said no inventory citation covers the changed line of `SKILL.md`; INV-05 line 559 cites `SKILL.md:139-155`, which contains line 141 | the note is corrected; the statement at INV-05 line 559 does not depend on the changed wording |
| INV-04 §3.2, lines 177-179 | "two callers depend on the key order": `policyIdForAdvertisedPart` iterates in key order, but its respelled keys have distinct advertised parts, so only `lib/context-window.js:67` depends on the order today | reworded |
| INV-04 line 897 | `$1` is not expanded by `String.prototype.replace` with a string pattern | removed from the list |
| INV-05 line 118 | `test/context-window.test.js:121-128` is port wiring, not an HTTP fixture | now `:144-167` |
| INV-05 line 304 | no test sets `CODEX_GATEWAY_KEEP_PLAN_TOOLS` | the row says so and cites the product lines |
| INV-05 line 362 | the stale pin stays in the cache and stops being the effective default | reworded with `:1398-1401` |
| INV-05 line 363 | `context-window:1457-1519` shows no egress; credential-free is asserted at `:1647-1681` | both cited |

The last six rows were in the inventories before the move. They show that an inventory statement can be imprecise even
when its citation resolves: the pinned source, not the inventory, is the authority for an implementing lane.
