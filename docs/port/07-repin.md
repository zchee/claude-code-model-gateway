# Moving the porting inventories from 4ffb2f672f0c to a8501f251755

- Written: 2026-11-25 23:02:55 JST (from `date`). Decision: the user, 2026-11-25 ("raise the baseline to a8501f251755").
- INV-01 to INV-05 were written by five lanes against `4ffb2f672f0c`. This note records how they and the work plan
  were moved to `a8501f251755`, the head of `dev`, so that a later move can repeat the procedure.
- Nothing of the plugin was executed. The rewrite ran once; the script is kept beside this note as
  `repin-4ffb2f6-to-a8501f2.py` and must not be run again on the moved files (it would shift them twice).

## 1. The two commits

| | old pin | new pin |
|---|---|---|
| commit | `4ffb2f672f0c` | `a8501f25175572071a0d085d34346dcf67e49b23` |
| tree of `plugins/model-gateway` | `c9c1fc9e0257e0c9c2849090e179f10e33202cdb` | `aaf4f26b527c2ff08dbc47707a5f69e6ec2cf40a` |
| root tree | not recorded | `22be5dae8859c7307906f938fa364b7ffadf006c` (1,005 files, 21.5 MiB) |
| JavaScript in `lib/`, `bin/`, `hooks/` | 33 files, 13,056 lines | 33 files, 13,060 lines |
| `*.test.js` | 57 files, 21,677 lines | 57 files, 21,680 lines |
| all of `test/` | 65 files, 23,539 lines | 65 files, 23,542 lines |
| `lib/runtime.js` | 797 lines | 801 lines |
| `test/context-window.test.js` | 2,018 lines | 2,021 lines |
| plugin version | 0.56.3 | 0.56.3 |

Commits between them: `6ffbf58d3` (2026-11-25 21:52, "give Vertex Gemini 4 Hydrogen and Neon their 1M window"), the
only one that touches the plugin (4 files, 21 insertions, 14 deletions), and `a8501f251`, which changes only
`.claude/.codebase-info/`. No `test()` call is added or removed; the 693 static test sites stand.

## 2. What `6ffbf58d3` changes

| File | Change |
|---|---|
| `lib/runtime.js` | the constant `VERTEX_GEMINI_IDS` (old lines 67-70) is replaced by the helper `vertexGeminiSpellings` (new line 67); the nine Gemini ids move into the first evidence group (new lines 70-73); a second evidence group `models.dev google-vertex 2026-11-25` with `gemini-4-hydrogen` and `gemini-4-neon` is inserted after the first group (new lines 85-88). Result: four more policy rows (bare id and `vertex/<id>` for each), window 1,048,576, placed between `vertex/meta/llama-4-scout-17b-16e-instruct-maas` and the `vertex/openai/*` rows. Vertex rows 45 to 49; bare Gemini ids 9 to 11 |
| `test/context-window.test.js` | two table cases for the new ids with `measuredOn: '2026-11-25'`; the measurement assertion checks each row's date instead of a fixed date; the registered list gains four ids; counts 45 to 49 and 9 to 11; the comment names proxy commit `ef6b0bc` |
| `README.md` (line 60) | "49 registered ids" and the second catalog date |
| `skills/model-gateway/SKILL.md` (line 141) | the same |

## 3. Line mapping (old line to new line)

`lib/runtime.js`:

| old | new | note |
|---|---|---|
| 1-66 | same | |
| 67-70 | 67 | content replaced |
| 71-72 | 68-69 | |
| 73 | 70-73 | content replaced |
| 74-84 | same | |
| 85 and later | +4 | |

`test/context-window.test.js`: 1-293 same; 294-331 +2 (lines 306, 314, 320 and 325 changed in place); 332 and later
+3 (lines 340, 365 and 369 changed in place). `README.md` and `SKILL.md` change one line each in place. INV-05 line 559
cites `SKILL.md:139-155`, a range that contains the changed line 141; its statement does not depend on the changed
wording. No citation points at `README.md:60`.

## 4. What was rewritten

| Document | rewritten | already right |
|---|---:|---:|
| `01-cli.md` | 11 | 11 |
| `02-worker.md` | 44 | 15 |
| `03-supervision.md` | 4 | 13 |
| `04-policy-state.md` | 136 | 41 |
| `05-tests-skills.md` | 20 | 2 |
| the work plan | 6 | 0 |
| total | 221 | 82 |

Citation forms that were handled:

- `runtime.js:<lines>` with or without a directory, including comma-separated lists (26 lists, each read in context and
  confirmed to be a list of lines);
- the shorthand `:<lines>` in backticks after a `runtime.js` citation on the same line;
- the same shorthand on lines of INV-04 that name no source file before it but stand in a passage about
  `lib/runtime.js`: document lines 275, 291, 294 (policy functions), 863, 867, 868, 872, 873 (install resolution), 963
  (open questions), in the numbering after the row insertion of section 5. They were found by listing every such
  shorthand of INV-04 with its section and reading the list;
- seven more shorthand citations in the same install-resolution table (document lines 869-871 and 874-877). The
  script missed them because those rows also name `bin/model-gateway.js`, which it took for the governing file; the
  verification lane found them (`verify-repin.md`) and they were corrected by hand;
- the bare line ranges in the last column of the hand-written rows table of INV-04 §3.2 (12 rows);
- `test/context-window.test.js:<lines>` and the abbreviated `context-window:<lines>` of INV-05;
- three notes that list which ranges of `lib/runtime.js` a lane read (INV-02 lines 15 and 29, INV-03 line 19). They were
  shifted mechanically; the changed lines 67-73 and the inserted lines 85-88 were read for this move, not by those lanes.

The other inventories were searched for shorthand and bare line cells governed by `lib/runtime.js`; none was found
outside INV-04 (the hits were other files' lines or not citations).

## 5. Content changes

- INV-04 §3.2: "45 generated Vertex rows" is now 49; the generated-rows table has the new group as rows 39-42 and the
  following groups renumbered 43-46 and 47-49; the range of the generating code is `lib/runtime.js:67-107`.
- INV-05 §2: `context-window` is 2,021 lines.
- The header of each inventory names the new commit and this note.

## 6. Check

For every citation whose range touches no changed line and does not span an insertion point, the script compared the
cited lines of the old commit with the mapped lines of the new commit: 0 differences, in the dry run and in the applied
run. Four ranges touch a changed line or span an insertion and were decided by hand: the three read-range notes above,
and INV-04 §3.2 `67-103`, now `67-107` (the whole block from the helper to the end of `vertexWindowPolicyRows`).
Spot checks after the rewrite: `lib/runtime.js:229` is the spread of `vertexWindowPolicyRows()`, `:239` closes
`MODEL_WINDOW_POLICY`, `:240` is the `CODEX_CONTEXT_WINDOWS` alias.

A separate lane then read 216 citations of `lib/runtime.js` and all 18 citations of the context-window test against
the new commit (`verify-repin.md`). It found the seven missed shorthand citations named in section 4 and six statements
that were imprecise before the move; all were corrected.

Limits: the script's check shows that each citation still points at the same source text. It does not re-examine whether the
inventories' statements about that text are right; that is unchanged from the lanes' work. Pre-images of the inventories
were not kept; the mapping of section 3 is reversible. The review records (`review-*.md`) were left as written and cite
the old commit.
