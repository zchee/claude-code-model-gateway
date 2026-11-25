# Gateway parity matrix

| Row id | Area | Assertion | Source tests | Due | Go package | Source of the row |
|---|---|---|---|---|---|---|
| alpha | gate | First assertion | `test/source.test.js:1` `Alpha` | phase-2 | `internal/gate` | Pinned case |
| alpha-repeat | gate | A second predicate | `test/source.test.js:1:1` `Alpha` | phase-2 | `internal/gate` | Pinned case |
| beta | gate | Shared helper and child fixture | `test/source.test.js:1` `Beta` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate |
| launch | gate | Launch contract | `test/source.test.js:2` `Launch` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate |
| replacement | gate | New form | no-source-test | phase-4 | `internal/gate` | difference:new-form; supersedes=test/source.test.js:35; superseded-case=`test/source.test.js:3` `Replacement`; pending=internal/gate.TestReplacement/subtest |

## Phase scope ledger

### phase-0 — 0 rows

### phase-1 — 0 rows

### phase-2 — 4 rows

- gate: `alpha`, `alpha-repeat`, `beta`, `launch`

### phase-3 — 0 rows

### phase-4 — 1 row

- gate: `replacement`

### phase-5 — 0 rows

**Total: 5 rows.**
