// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestParityCheck(t *testing.T) {
	const (
		alphaCase       = `test/source.test.js:1:1 "Alpha"`
		betaCase        = `test/source.test.js:1:1 "Beta"`
		launchCase      = `test/source.test.js:2:1 "Launch"`
		launchPair      = launchCase + " -> test/source.test.js:31:2"
		replacementPair = `test/source.test.js:3:1 "Replacement" -> test/source.test.js:35:2`
		alphaPair       = alphaCase + " -> test/helper.js:20:3"
		betaPair        = betaCase + " -> test/helper.js:20:3"
		embedded        = betaCase + " -> test/source.test.js:25:2"
		sharedEvent     = "TestGate/shared"
		fixtureCommit   = "0123456789012345678901234567890123456789"
	)
	type change struct{ file, old, new string }
	deferLaunch := []change{
		{siteFile, "Launch\ttest/source.test.js\t31\t2\t\t\tphase-2", "Launch\ttest/source.test.js\t31\t2\t\t\tphase-3"},
		{deferralFile, "", "test/source.test.js\t2\t1\tLaunch\ttest/source.test.js\t31\t2\tphase-3\tApproved deferral.\n"},
	}
	reviewLaunch := change{siteFile, "Launch\ttest/source.test.js\t31\t2\t\t\tphase-3", "Launch\ttest/source.test.js\t31\t2\tinternal/gate.TestGate/shared\tReviewed unconditional launch check.\tphase-3"}
	mapping := []string{"mapping", "phase-2"}
	results := []string{"results", "phase-2", "--go-json", "go.json", "--census", "conformance.tsv", "--launches", "launches.tsv"}
	phase4 := []string{"results", "phase-4", "--go-json", "go.json"}
	final := slices.Clone(results)
	final[1] = "phase-5"
	tests := map[string]struct {
		args    []string
		changes []change
		want    []string
	}{
		"success: structure default":        {},
		"success: phase mapping":            {args: mapping},
		"success: results":                  {args: results},
		"success: cumulative final results": {args: final},
		"success: task mapping excludes phase-gated and later pairs": {
			args:    []string{"mapping", "phase-2", "internal/gate"},
			changes: []change{{siteFile, "Beta\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\tReviewed shared check.\tphase-2", "Beta\ttest/helper.js\t20\t3\t\t\tphase-2"}},
		},
		"error: coordinated exempt case deletion still breaks the census digest": {
			changes: []change{{censusFile, "\tDormant\t", ""}, {classFile, "Dormant\t", ""}, {approvalFile, "\tDormant\t", ""}, {pairFile, "\tDormant\t", ""}, {overFile, "\tDormant\t", ""}},
			want:    []string{censusFile + ": census_sha256: digest does not match test/conformance/baseline/run.json"},
		},
		"error: package and phase-gate ownership counts disagree": {
			changes: []change{{ownersFile, "gate | gate | 1 | 0", "gate | gate | 2 | 0"}, {ownersFile, "phase exit | phase exit | 2 | 1", "phase exit | phase exit | 2 | 0"}},
			want: []string{
				ownersFile + ": phase-2 internal/gate: counts due=2 conformance=0 disagree with classification due=1 conformance=0",
				ownersFile + ": phase-2 **phase gate**: counts due=2 conformance=0 disagree with classification due=2 conformance=1",
			},
		},
		"success: matrix ignores unrelated tables and fenced examples": {
			changes: []change{{matrixFile, "# Gateway parity matrix\n", "# Gateway parity matrix\n\n| Heading | Other |\n|---|---|\n| data | note |\n\n```md\n| Row id | Area | Assertion | Source tests | Due | Go package | Source of the row |\n|---|---|---|---|---|---|---|\n| example | gate | unrelated | no-source-test | phase-4 | internal/gate | spare |\n\n## Phase scope ledger\n```\n"}},
		},
		"success: CRLF and a final lock row without newline": {
			changes: []change{{matrixFile, "\n", "\r\n"}, {lockFile, "replacement\n", "replacement"}, {lockFile, "\n", "\r\n"}, {planFile, "\n", "\r\n"}},
		},
		"success: escaped citation resolves to the exact encoded census name": {
			changes: []change{
				{censusFile, "Alpha", `Alpha\\n`},
				{classFile, "Alpha", `Alpha\\n`},
				{pairFile, "Alpha", `Alpha\\n`},
				{siteFile, "Alpha", `Alpha\\n`},
				{matrixFile, "`Alpha`", "`Alpha" + `\n` + "`"},
				{"test/conformance/baseline/run.json", "c46a06446c4b448bdaa26cf4098c132dc56fb6fe9ff800f3722ab45eeed34f7c", "f8b69d1735440f662e6ee2f2ab3a6d45bd3f8aca9858e590c1ffb2782f6e07b3"},
			},
		},
		"error: misspelled matrix marker": {
			changes: []change{{matrixFile, "Shared helper and child fixture", "Shared helper fixture"}, {matrixFile, "`Beta` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate", "`Beta` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate; narrowre-from=phase-2@internal/gate"}},
			want:    []string{matrixFile + `: beta: unknown matrix marker "narrowre-from=phase-2@internal/gate"`},
		},
		"error: matrix cross-link must match the cited classification": {
			changes: []change{{matrixFile, "`Beta` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate", "`Beta` | phase-2 | `internal/gate` | Pinned case; due-at=phase-gate; narrower-from=phase-2@internal/other"}},
			want:    []string{matrixFile + ": beta: narrower-from=phase-2@internal/other does not identify a cited case's phase and package"},
		},
		"error: pending matrix test cannot conflict with an earlier plan schedule": {
			changes: []change{{planFile, "`internal/gate.TestReplacement` (phase-4)", "`internal/gate.TestReplacement/subtest` (phase-3)"}},
			want:    []string{matrixFile + ": replacement: pending test example.org/port/internal/gate.TestReplacement/subtest is scheduled in phase-3 by the plan, earlier than row phase phase-4"},
		},
		"error: missing structured replacement column is not rescued by prose": {
			changes: []change{{planFile, "Replacement tests (phase)", "Old replacement notes"}},
			want:    []string{planFile + `: difference table: missing "Replacement tests (phase)" column`},
		},
		"error: blank TSV header column": {
			changes: []change{{siteFile, "target\treview\tdue", "\treview\tdue"}},
			want:    []string{siteFile + `: header: invalid column ""`, siteFile + `: header: missing column "target"`},
		},
		"error: malformed extra TSV header column": {
			changes: []change{{siteFile, "target\treview\tdue", "target\treview\tdue\textra field"}},
			want:    []string{siteFile + `: header: invalid column "extra field"`},
		},
		"error: missing required TSV header column": {
			changes: []change{{siteFile, "target\treview\tdue", "other\treview\tdue"}},
			want:    []string{siteFile + `: header: missing column "target"`},
		},
		"error: malformed matrix row is not silently ignored": {
			changes: []change{{matrixFile, " |\n\n## Phase scope ledger", " |\nForgotten | gate | assertion | no-source-test | phase-4 | internal/gate | pinned case |\n\n## Phase scope ledger"}},
			want:    []string{matrixFile + ": 10: matrix row must begin with a pipe"},
		},
		"error: an unreferenced difference row after a gap needs a valid replacement": {
			changes: []change{{planFile, "", "\n| — | unreferenced | Old | New | Purpose | Evidence | Human summary. | — |\n"}},
			want: []string{
				planFile + `: unreferenced: invalid replacement test entry "—"`,
				planFile + ": unreferenced: no valid Go replacement test named",
			},
		},
		"error: one malformed replacement cannot hide behind valid entries": {
			changes: []change{{planFile, "<br>`internal/gate.TestPackagedReplacement` (phase-5)", "<br>`internal/gate.TestPackagedReplacement` (phase-5)<br>unreadable"}},
			want:    []string{planFile + `: new-form: invalid replacement test entry "unreadable"`},
		},
		"error: phase ledger bullet does not hide a malformed id": {
			changes: []change{{matrixFile, "- gate: `replacement`", "- gate: `replacement`, `Extra`"}},
			want:    []string{matrixFile + ": phase-4: invalid ledger row id \"`Extra`\""},
		},
		"error: malformed difference row after a gap lacks a leading pipe and has an uppercase slug": {
			args:    final,
			changes: []change{{planFile, "", "\n— | Extra | Old | New | Why | Proof | prose | `internal/gate.TestExtra` (phase-5) |\n"}},
			want: []string{
				planFile + ": 9: invalid difference table row",
				planFile + ": difference table: parsed 1 difference rows, found 2 table-shaped rows",
			},
		},
		"error: difference row has no trailing pipe": {
			changes: []change{{planFile, "", "\n| — | extra | Old | New | Why | Proof | prose | `internal/gate.TestExtra` (phase-5)\n"}},
			want: []string{
				planFile + ": 9: invalid difference table row",
				planFile + ": difference table: parsed 1 difference rows, found 2 table-shaped rows",
			},
		},
		"error: difference row has exactly one cell too few": {
			changes: []change{{planFile, "", "\n| — | extra | Old | New | Why | Proof | `internal/gate.TestExtra` (phase-5) |\n"}},
			want: []string{
				planFile + ": 9: invalid difference table row",
				planFile + ": difference table: parsed 1 difference rows, found 2 table-shaped rows",
			},
		},
		"error: valid lowercase difference row after a prose gap still requires evidence": {
			args:    final,
			changes: []change{{planFile, "", "\nA prose interruption.\n\n| — | extra | Old | New | Why | Proof | prose | `internal/gate.TestExtra` (phase-5) |\n"}},
			want:    []string{planFile + ": extra: no exact Go test run for example.org/port/internal/gate.TestExtra in --go-json evidence"},
		},
		"error: malformed row counted independently by its shape": {
			changes: []change{{planFile, "", "\n| — | omitted | Old | New |\n"}},
			want: []string{
				planFile + ": 9: invalid difference table row",
				planFile + ": difference table: parsed 1 difference rows, found 2 table-shaped rows",
			},
		},
		"error: duplicate census identity with matching digest": {
			changes: []change{
				{censusFile, "", "test/source.test.js\t1\t1\t0\tAlpha\tpass\t-\n"},
				{"test/conformance/baseline/run.json", "c46a06446c4b448bdaa26cf4098c132dc56fb6fe9ff800f3722ab45eeed34f7c", "fd6913af208b71f985ba301e18c105b10835cda2c67cf47dcf19fbc222a6587b"},
			},
			want: []string{censusFile + ": " + alphaCase + ": duplicate case"},
		},
		"error: duplicate classification identity": {
			changes: []change{{classFile, "", "Alpha\t1\t1\ttest/source.test.js\tgo-port\tphase-2\tinternal/gate\ttask\tDuplicated.\n"}},
			want:    []string{classFile + ": " + alphaCase + ": duplicate case"},
		},
		"error: duplicate approval identity": {
			changes: []change{{approvalFile, "", "test/source.test.js\t4\t1\tDormant\tunsupported:dormant-code\tAnother review.\n"}},
			want:    []string{approvalFile + `: test/source.test.js:4:1 "Dormant": duplicate exemption approval`},
		},
		"error: duplicate static launch row": {
			changes: []change{{launchFile, "", "test/source.test.js\t30\t2\tworker\tstartGateway\tLaunch\tserve-worker\n"}},
			want:    []string{launchFile + `: test/source.test.js:30:2: duplicate launch row for "Launch"`},
		},
		"success: a static launch can supply its full owner key": {
			changes: []change{{launchFile, "case\targv", "case\targv\tcase_path\tcase_line\tcase_column"}, {launchFile, "test/source.test.js\t30", "test/helper.js\t30"}, {launchFile, "serve-worker\n", "serve-worker\ttest/source.test.js\t2\t1\n"}},
		},
		"error: a static launch cannot name the wrong registration via full coordinates": {
			changes: []change{{launchFile, "case\targv", "case\targv\tcase_path\tcase_line\tcase_column"}, {launchFile, "serve-worker\n", "serve-worker\ttest/source.test.js\t3\t1\n"}, {launchFile, "", "test/source.test.js\t31\t2\tworker\tstartGateway\tLaunch\tserve-worker\ttest/source.test.js\t2\t1\n"}},
			want:    []string{launchFile + `: test/source.test.js:30:2: launch does not name an exact census case: test/source.test.js:3:1 "Launch"`},
		},
		"error: duplicate generated pair": {
			changes: []change{{pairFile, "", "test/source.test.js\t1\t1\tAlpha\ttest/helper.js\t20\t3\tassert.equal\thelper:check\n"}},
			want:    []string{pairFile + ": " + alphaPair + ": duplicate pair"},
		},
		"error: duplicate override pair": {
			changes: []change{{overFile, "", "test/source.test.js\t4\t1\tDormant\ttest/source.test.js\t38\t2\tremove\tDuplicate reason.\n"}},
			want:    []string{overFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:38:2: duplicate override`},
		},
		"error: duplicate mapping pair": {
			changes: []change{{siteFile, "", "test/source.test.js\t1\t1\tAlpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\tReviewed again.\tphase-2\n"}},
			want:    []string{siteFile + ": " + alphaPair + ": duplicate pair"},
		},
		"error: duplicate matrix row id": {
			changes: []change{{matrixFile, "\n\n## Phase scope ledger", "\n| alpha | gate | Duplicate row | no-source-test | phase-4 | internal/gate | Source |\n\n## Phase scope ledger"}},
			want:    []string{matrixFile + ": alpha: duplicate row id"},
		},
		"error: duplicate matrix lock id": {
			changes: []change{{lockFile, "", "alpha\n"}},
			want:    []string{lockFile + ": alpha: invalid or duplicate row id"},
		},
		"error: duplicate matrix ledger id": {
			changes: []change{{matrixFile, "`alpha`, `alpha-repeat`", "`alpha`, `alpha`, `alpha-repeat`"}, {matrixFile, "phase-2 — 4 rows", "phase-2 — 5 rows"}},
			want:    []string{matrixFile + ": alpha: duplicate phase ledger row"},
		},
		"error: duplicate ownership key": {
			changes: []change{{ownersFile, "", "| phase-2 | `internal/gate` | gate | gate | 1 | 0 |\n"}},
			want:    []string{ownersFile + ": phase-2 internal/gate: duplicate ownership row"},
		},
		"error: duplicate plan slug": {
			changes: []change{{planFile, "", "| — | new-form | Old | New | Reason | Evidence | Human summary. | `internal/gate.TestReplacement` (phase-4) |\n"}},
			want:    []string{planFile + ": new-form: duplicate difference slug", planFile + ": difference table: parsed 1 difference rows, found 2 table-shaped rows"},
		},
		"error: missing classification expansion": {
			changes: []change{{classFile, "Alpha\t1\t1\t", ""}, {ownersFile, "gate | gate | 1 | 0", "gate | gate | 0 | 0"}},
			want:    []string{classFile + ": " + alphaCase + ": missing census case"},
		},
		"error: classification expansion missing from census": {
			changes: []change{{classFile, "", "Outside\t1\t1\ttest/source.test.js\tgo-port\tphase-2\tinternal/gate\ttask\tNew row.\n"}, {ownersFile, "gate | gate | 1 | 0", "gate | gate | 2 | 0"}},
			want:    []string{classFile + `: test/source.test.js:1:1 "Outside": case not in ` + censusFile},
		},
		"error: matrix lost all citations for a due case": {
			changes: []change{
				{matrixFile, "| beta |", ""},
				{matrixFile, "`beta`, `launch`", "`launch`"},
				{matrixFile, "phase-2 — 4 rows", "phase-2 — 3 rows"},
				{matrixFile, "5 rows", "4 rows"},
				{lockFile, "beta", ""},
			},
			want: []string{matrixFile + ": " + betaCase + ": case not cited as proof by any row"},
		},
		"error: lock catches lost row even when another still cites the case": {
			changes: []change{{matrixFile, "| alpha-repeat |", ""}, {matrixFile, "`alpha`, `alpha-repeat`", "`alpha`"}, {matrixFile, "phase-2 — 4 rows", "phase-2 — 3 rows"}, {matrixFile, "5 rows", "4 rows"}},
			want:    []string{matrixFile + ": alpha-repeat: locked row is missing"},
		},
		"error: missing shared-helper pair while another case covers its site": {
			changes: []change{{siteFile, "Beta\ttest/helper.js", ""}},
			want:    []string{siteFile + ": " + betaPair + ": missing expected pair (via helper:check)"},
		},
		"error: missing embedded assertion mapping": {
			changes: []change{{siteFile, "Beta\ttest/source.test.js\t25", ""}, {deferralFile, "Beta\ttest/source.test.js\t25", ""}},
			want:    []string{siteFile + ": " + embedded + ": missing expected pair (via embedded:node-e)"},
		},
		"error: mapping pair absent from generated evidence": {
			changes: []change{{pairFile, "embedded:node-e", ""}},
			want:    []string{siteFile + ": " + embedded + ": pair not in generated evidence after overrides"},
		},
		"error: wrong package in owner table": {
			changes: []change{{ownersFile, "phase-4 | `internal/gate`", "phase-4 | `internal/other`"}},
			want:    []string{classFile + `: test/source.test.js:3:1 "Replacement": phase/package/gate not listed in ` + ownersFile},
		},
		"error: wrong phase for a package in the owner table": {
			changes: []change{{ownersFile, "phase-4 | `internal/gate`", "phase-5 | `internal/gate`"}},
			want:    []string{classFile + `: test/source.test.js:3:1 "Replacement": phase/package/gate not listed in ` + ownersFile},
		},
		"error: missing owner phase gate": {
			changes: []change{{ownersFile, "phase-4 | **phase gate**", ""}},
			want:    []string{classFile + `: test/source.test.js:3:1 "Replacement": phase/package/gate not listed in ` + ownersFile},
		},
		"error: removal override omits its reason": {
			changes: []change{{overFile, "The test-looking call is only fixture text.\n", "\n"}},
			want:    []string{overFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:38:2: override needs a reason`},
		},
		"error: unknown override action": {
			changes: []change{{overFile, "\tremove\t", "\tunknown\t"}},
			want:    []string{overFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:38:2: invalid override action "unknown"`},
		},
		"error: not-ported reason outside its list": {
			args:    mapping,
			changes: []change{{siteFile, "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared", "Alpha\ttest/helper.js\t20\t3\tnot-ported:unknown"}},
			want:    []string{siteFile + ": " + alphaPair + `: invalid not-ported reason "unknown"`},
		},
		"error: not-ported cites unknown difference slug": {
			args:    mapping,
			changes: []change{{siteFile, "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared", "Alpha\ttest/helper.js\t20\t3\tnot-ported:changed-by-difference:unknown"}},
			want:    []string{siteFile + ": " + alphaPair + `: unknown difference slug "unknown"`},
		},
		"error: declared addition is not in generated evidence": {
			changes: []change{{pairFile, "\tassert.equal\toverride", "\tassert.equal\tdirect"}},
			want:    []string{overFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:37:2: added pair must appear in generated evidence via override`},
		},
		"error: generated addition lacks its override": {
			changes: []change{{overFile, "\tadd\t", ""}},
			want:    []string{pairFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:37:2: override attribution has no matching add row`},
		},
		"error: declared removal is still in generated evidence": {
			changes: []change{{pairFile, "", "test/source.test.js\t4\t1\tDormant\ttest/source.test.js\t38\t2\tassert.equal\tdirect\n"}},
			want:    []string{overFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:38:2: removed pair still appears in generated evidence`},
		},
		"error: mapping includes an exempt case": {
			changes: []change{{siteFile, "", "test/source.test.js\t4\t1\tDormant\ttest/source.test.js\t37\t2\t\t\tphase-2\n"}},
			want:    []string{siteFile + `: test/source.test.js:4:1 "Dormant" -> test/source.test.js:37:2: mapping must not include an exempt case`},
		},
		"error: due case disappears from both pair files without an override": {
			changes: []change{{pairFile, "\tReplacement\t", ""}, {siteFile, "\tReplacement\t", ""}},
			want:    []string{pairFile + `: test/source.test.js:3:1 "Replacement": due case has no assertion pair`},
		},
		"error: removal overrides cannot empty a due case": {
			changes: []change{
				{pairFile, "\tReplacement\t", ""},
				{siteFile, "\tReplacement\t", ""},
				{overFile, "", "test/source.test.js\t3\t1\tReplacement\ttest/source.test.js\t35\t2\tremove\tSource call is unreachable.\ntest/source.test.js\t3\t1\tReplacement\ttest/source.test.js\t36\t2\tremove\tSecond source call is unreachable.\n"},
			},
			want: []string{pairFile + `: test/source.test.js:3:1 "Replacement": due case has no assertion pair`},
		},
		"error: reason outside closed list": {
			changes: []change{{classFile, "unsupported:dormant-code", "unsupported:unreviewed"}, {approvalFile, "unsupported:dormant-code", "unsupported:unreviewed"}},
			want:    []string{classFile + `: test/source.test.js:4:1 "Dormant": invalid class or reason "unsupported:unreviewed"`},
		},
		"error: unknown whole-case difference": {
			changes: []change{{classFile, "difference:new-form", "difference:unknown"}},
			want:    []string{classFile + `: test/source.test.js:3:1 "Replacement": unknown difference slug "unknown"`},
		},
		"error: plan identifier under test": {
			changes: []change{{classFile, "Shared assertion.", "Shared assertion. " + "T" + "2.3"}},
			want:    []string{classFile + ": 2: plan marker is not allowed under test/"},
		},
		"error: conformance case lacking launch site": {
			changes: []change{{launchFile, "\tLaunch\t", ""}},
			want:    []string{launchFile + ": " + launchCase + ": no launch site names conformance case"},
		},
		"error: unmapped due pair": {
			args:    mapping,
			changes: []change{{siteFile, "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\tReviewed shared check.\tphase-2", "Alpha\ttest/helper.js\t20\t3\t\t\tphase-2"}},
			want:    []string{siteFile + ": " + alphaPair + `: missing or invalid Go test target ""`},
		},
		"error: not-ported target without reviewer approval": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/shared\tReviewed shared check.", "not-ported:harness-internal\t"}},
			want: []string{
				siteFile + ": " + alphaPair + ": not-ported target needs reviewer approval",
				siteFile + ": " + betaPair + ": not-ported target needs reviewer approval",
			},
		},
		"error: shared target for distinct sites without per-pair review": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/child", "internal/gate.TestGate/shared"}, {siteFile, "\tReviewed shared check.\t", "\t\t"}},
			want: []string{
				siteFile + ": " + alphaPair + `: shared target "internal/gate.TestGate/shared" needs a reviewed unconditional check for this pair`,
				siteFile + ": " + betaPair + `: shared target "internal/gate.TestGate/shared" needs a reviewed unconditional check for this pair`,
			},
		},
		"error: difference without a named replacement": {
			changes: []change{{planFile, "`internal/gate.TestReplacement` (phase-4)<br>`internal/gate.TestPackagedReplacement` (phase-5)", "No test"}},
			want: []string{
				planFile + `: new-form: invalid replacement test entry "No test"`,
				planFile + ": new-form: no valid Go replacement test named",
			},
		},
		"error: difference pair names a test outside its replacement list": {
			args:    []string{"mapping", "phase-4"},
			changes: []change{{siteFile, "internal/gate.TestReplacement/subtest", "internal/gate.TestUnrelated/subtest"}},
			want:    []string{siteFile + ": " + replacementPair + `: target "internal/gate.TestUnrelated/subtest" is not a replacement test for difference "new-form"`},
		},
		"error: difference pair lacks a replacement target": {
			args:    []string{"mapping", "phase-4"},
			changes: []change{{siteFile, "\tinternal/gate.TestReplacement/subtest\t", "\t\t"}},
			want:    []string{siteFile + ": " + replacementPair + `: missing or invalid Go test target ""`},
		},
		"error: approved exemptions cannot cover every pair of a difference case": {
			args:    []string{"mapping", "phase-4"},
			changes: []change{{siteFile, "internal/gate.TestReplacement/subtest\t", "not-ported:source-doc-wording\tApproved."}, {siteFile, "internal/gate.TestReplacement/other\t", "not-ported:source-doc-wording\tApproved."}},
			want:    []string{siteFile + `: test/source.test.js:3:1 "Replacement": difference case needs a mapped replacement test due in phase-4`},
		},
		"error: named difference subtest cannot be replaced by a sibling": {
			args:    []string{"mapping", "phase-4"},
			changes: []change{{planFile, "`internal/gate.TestReplacement`", "`internal/gate.TestReplacement/subtest`"}},
			want:    []string{siteFile + `: test/source.test.js:3:1 "Replacement" -> test/source.test.js:36:2: target "internal/gate.TestReplacement/other" is not a replacement test for difference "new-form"`},
		},
		"error: plan replacement is independently required at its own phase": {
			args:    phase4,
			changes: []change{{"go.json", `"Test":"TestReplacement"`, ""}},
			want:    []string{planFile + ": new-form: no exact Go test run for example.org/port/internal/gate.TestReplacement in --go-json evidence"},
		},
		"success: earlier difference uses its own replacement before the packaging test": {
			args:    phase4,
			changes: []change{{"go.json", "TestPackagedReplacement", ""}},
		},
		"error: final gate misses an otherwise unreferenced plan replacement": {
			args:    final,
			changes: []change{{"go.json", "TestPackagedReplacement", ""}},
			want:    []string{planFile + ": new-form: no exact Go test run for example.org/port/internal/gate.TestPackagedReplacement in --go-json evidence"},
		},
		"success: a later approved pair is tested at its own gate": {
			args: []string{"results", "phase-3", "--go-json", "go.json"},
		},
		"error: an approved deferral does not waive its later Go evidence": {
			args:    []string{"results", "phase-3", "--go-json", "go.json"},
			changes: []change{{"go.json", "TestGate/child", ""}},
			want:    []string{siteFile + ": " + embedded + ": no exact Go test run for example.org/port/internal/gate.TestGate/child in --go-json evidence"},
		},
		"error: changing the due phase alone does not defer reviewed Go pairs": {
			changes: []change{{siteFile, "Reviewed shared check.\tphase-2", "Reviewed shared check.\tphase-3"}},
			want: []string{
				deferralFile + ": " + alphaPair + ": missing deferral approval for phase-3",
				deferralFile + ": " + betaPair + ": missing deferral approval for phase-3",
			},
		},
		"error: deferral approval does not authorize a different phase": {
			changes: []change{{deferralFile, "\tphase-3\t", "\tphase-4\t"}},
			want:    []string{deferralFile + ": " + embedded + ": approved due phase phase-4 does not match mapping due phase phase-3"},
		},
		"error: deferral approval for a nondeferred pair is orphaned": {
			changes: []change{{deferralFile, "", "test/source.test.js\t1\t1\tAlpha\ttest/helper.js\t20\t3\tphase-3\tUnmatched approval.\n"}},
			want:    []string{deferralFile + ": " + alphaPair + ": approval has no deferred mapping row"},
		},
		"error: duplicate deferral approval": {
			changes: []change{{deferralFile, "", "test/source.test.js\t1\t1\tBeta\ttest/source.test.js\t25\t2\tphase-3\tDuplicate approval.\n"}},
			want:    []string{deferralFile + ": " + embedded + ": duplicate deferral approval"},
		},
		"error: blank deferral approval text": {
			changes: []change{{deferralFile, "Inventory reviewer: the child fixture needs the later gate.\n", "\n"}},
			want:    []string{deferralFile + ": " + embedded + ": missing deferral approval text"},
		},
		"error: deferred Go pair has no target at its mapping gate": {
			args:    []string{"mapping", "phase-3"},
			changes: []change{{siteFile, "internal/gate.TestGate/child\tReviewer verified the child check.", "\tApproved mapping."}},
			want:    []string{siteFile + ": " + embedded + `: missing or invalid Go test target ""`},
		},
		"error: deferral approval cannot substitute for mapping review": {
			args:    []string{"mapping", "phase-3"},
			changes: []change{{siteFile, "Reviewer verified the child check.\tphase-3", "\tphase-3"}},
			want:    []string{siteFile + ": " + embedded + `: Go target "internal/gate.TestGate/child" needs reviewer approval`},
		},
		"error: Go pair without review even when no other site shares its target": {
			args:    []string{"mapping", "phase-2", "internal/gate"},
			changes: []change{{siteFile, "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\tReviewed shared check.", "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\t"}},
			want:    []string{siteFile + ": " + alphaPair + `: Go target "internal/gate.TestGate/shared" needs reviewer approval`},
		},
		"error: pair due before case phase": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/shared\tReviewed shared check.\tphase-2", "internal/gate.TestGate/shared\tReviewed shared check.\tphase-1"}},
			want: []string{
				siteFile + ": " + alphaPair + ": due phase phase-1 is earlier than case phase phase-2",
				siteFile + ": " + betaPair + ": due phase phase-1 is earlier than case phase phase-2",
			},
		},
		"error: empty ledger": {
			args: []string{"mapping", "phase-0"},
			want: []string{siteFile + ": phase-0: empty obligation ledger"},
		},
		"error: absent Go evidence": {
			args: []string{"results", "phase-2", "--census", "conformance.tsv", "--launches", "launches.tsv"},
			want: []string{
				siteFile + ": " + alphaPair + ": Go test example.org/port/internal/gate.TestGate/shared requires --go-json evidence",
				siteFile + ": " + betaPair + ": Go test example.org/port/internal/gate.TestGate/shared requires --go-json evidence",
			},
		},
		"error: deferred conformance pair does not waive its case gate": {
			args:    []string{"results", "phase-2", "--go-json", "go.json", "--launches", "launches.tsv"},
			changes: deferLaunch,
			want:    []string{classFile + ": " + launchCase + ": conformance case requires --census evidence"},
		},
		"success: structure can accept an approved deferral before its target is mapped": {
			changes: deferLaunch,
		},
		"error: approved conformance deferral still needs a target at the later results gate": {
			args:    []string{"results", "phase-3", "--go-json", "go.json"},
			changes: deferLaunch,
			want:    []string{siteFile + ": " + launchPair + `: missing or invalid Go test target ""`},
		},
		"success: reviewed deferred conformance target passes at the later gate": {
			args:    []string{"results", "phase-3", "--go-json", "go.json"},
			changes: append(slices.Clone(deferLaunch), reviewLaunch),
		},
		"error: deferred conformance target needs evidence in its later phase": {
			args:    []string{"results", "phase-3", "--go-json", "go.json"},
			changes: append(slices.Clone(deferLaunch), reviewLaunch, change{"go.json", sharedEvent, ""}),
			want:    []string{siteFile + ": " + launchPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence"},
		},
		"error: conformance launch evidence input omitted": {
			args: []string{"results", "phase-2", "--go-json", "go.json", "--census", "conformance.tsv"},
			want: []string{classFile + ": " + launchCase + ": conformance case requires --launches evidence"},
		},
		"error: Go evidence is from another commit": {
			args:    results,
			changes: []change{{"go.json", fixtureCommit, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			want:    []string{"go.json: metadata: commit does not match --commit"},
		},
		"error: conformance census is from another run": {
			args:    results,
			changes: []change{{"conformance.tsv", "venue-fixture", "another-run"}},
			want:    []string{"conformance.tsv: metadata: run identifier differs from the other evidence"},
		},
		"error: launch binary differs within the run": {
			args:    results,
			changes: []change{{"launches.tsv", "", "test/source.test.js\t2\t1\tLaunch\t/venue/bin/other\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"}},
			want:    []string{"launches.tsv: " + launchCase + ": launch binary path or digest differs within the run"},
		},
		"error: launch binary digest differs at the same path": {
			args:    results,
			changes: []change{{"launches.tsv", "", "test/source.test.js\t2\t1\tLaunch\t/venue/bin/gateway\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdee\n"}},
			want:    []string{"launches.tsv: " + launchCase + ": launch binary path or digest differs within the run"},
		},
		"error: a launch row has no usable binary digest": {
			args:    results,
			changes: []change{{"launches.tsv", "", "test/source.test.js\t2\t1\tLaunch\t/venue/bin/gateway\t-\n"}},
			want:    []string{"launches.tsv: " + launchCase + ": launch needs a SHA-256 binary digest"},
		},
		"error: named pending matrix target did not run": {
			args:    phase4,
			changes: []change{{matrixFile, "pending=internal/gate.TestReplacement/subtest", "pending=internal/gate.TestReplacement/standalone"}},
			want:    []string{matrixFile + ": replacement: no exact Go test run for example.org/port/internal/gate.TestReplacement/standalone in --go-json evidence"},
		},
		"error: skipped Go subtest": {
			args:    results,
			changes: []change{{"go.json", `"Action":"pass","Package":"example.org/port/internal/gate","Test":"` + sharedEvent, `"Action":"skip","Package":"example.org/port/internal/gate","Test":"` + sharedEvent}},
			want: []string{
				"go.json: example.org/port/internal/gate.TestGate/shared: skipped Go test result for " + alphaPair,
				"go.json: example.org/port/internal/gate.TestGate/shared: skipped Go test result for " + betaPair,
			},
		},
		"error: Go test passed in wrong package": {
			args: results,
			changes: []change{
				{"go.json", sharedEvent, ""},
				{"go.json", "", `{"Action":"start","Package":"example.org/port/internal/other"}
{"Action":"run","Package":"example.org/port/internal/other","Test":"TestGate"}
{"Action":"run","Package":"example.org/port/internal/other","Test":"TestGate/shared"}
{"Action":"pass","Package":"example.org/port/internal/other","Test":"TestGate/shared"}
{"Action":"pass","Package":"example.org/port/internal/other","Test":"TestGate"}
{"Action":"output","Package":"example.org/port/internal/other","Output":"ok  \texample.org/port/internal/other\t0.014s\n"}
{"Action":"pass","Package":"example.org/port/internal/other"}
`},
			},
			want: []string{
				siteFile + ": " + alphaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
				siteFile + ": " + betaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
			},
		},
		"error: Go package results replayed from cache": {
			args:    results,
			changes: []change{{"go.json", "0.014s", "(cached)"}},
			want: []string{
				"go.json: example.org/port/internal/gate.TestGate/shared: cached Go test result for " + alphaPair,
				"go.json: example.org/port/internal/gate.TestGate/shared: cached Go test result for " + betaPair,
			},
		},
		"error: conformance result is skipped": {
			args:    results,
			changes: []change{{"conformance.tsv", "\tpass\t-", "\tskip\tnot supported"}},
			want:    []string{classFile + ": " + launchCase + `: no passing conformance case in --census evidence (status "skip")`},
		},
		"error: conformance result fails": {
			args:    results,
			changes: []change{{"conformance.tsv", "\tpass\t-", "\tfail\tassertion failed"}},
			want:    []string{classFile + ": " + launchCase + `: no passing conformance case in --census evidence (status "fail")`},
		},
		"error: duplicate conformance result": {
			args:    results,
			changes: []change{{"conformance.tsv", "", "test/source.test.js\t2\t1\t0\tLaunch\tpass\t-\n"}},
			want:    []string{"conformance.tsv: " + launchCase + ": duplicate case"},
		},
		"error: launch path is not absolute": {
			args:    results,
			changes: []change{{"launches.tsv", "", "test/source.test.js\t2\t1\tLaunch\tbin/gateway\t0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"}},
			want:    []string{"launches.tsv: " + launchCase + ": launch needs an absolute, clean Go binary path"},
		},
		"error: passing test name is only a prefix of the target": {
			args:    results,
			changes: []change{{siteFile, "internal/gate.TestGate/shared", "internal/gate.TestGate/shared_longer"}},
			want: []string{
				siteFile + ": " + alphaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared_longer in --go-json evidence",
				siteFile + ": " + betaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared_longer in --go-json evidence",
			},
		},
		"error: test fails after its pass event": {
			args:    results,
			changes: []change{{"go.json", `"Action":"pass","Package":"example.org/port/internal/gate","Test":"TestGate/shared"}` + "\n", `"Action":"pass","Package":"example.org/port/internal/gate","Test":"TestGate/shared"}` + "\n" + `{"Action":"fail","Package":"example.org/port/internal/gate","Test":"TestGate/shared"}` + "\n"}},
			want:    []string{"go.json: example.org/port/internal/gate.TestGate/shared: out-of-order test fail event"},
		},
		"error: package cannot pass before its active test completes": {
			args:    results,
			changes: []change{{"go.json", `{"Action":"run","Package":"example.org/port/internal/gate","Test":"TestGate/shared"}`, `{"Action":"pass","Package":"example.org/port/internal/gate"}` + "\n" + `{"Action":"run","Package":"example.org/port/internal/gate","Test":"TestGate/shared"}`}},
			want:    []string{"go.json: example.org/port/internal/gate: out-of-order package pass event"},
		},
		"error: package pass without a successful uncached summary": {
			args:    results,
			changes: []change{{"go.json", "0.014s", ""}},
			want: []string{
				"go.json: example.org/port/internal/gate.TestGate/shared: missing uncached package summary for " + alphaPair,
				"go.json: example.org/port/internal/gate.TestGate/shared: missing uncached package summary for " + betaPair,
			},
		},
		"success: mapping accepts a module-root test by full import path": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/shared", "example.org/port.TestRoot/shared"}},
		},
		"error: parent passed without the named subtest": {
			args:    results,
			changes: []change{{"go.json", sharedEvent, ""}},
			want: []string{
				siteFile + ": " + alphaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
				siteFile + ": " + betaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
			},
		},
		"error: passing source test lacks attributed Go launch": {
			args:    results,
			changes: []change{{"launches.tsv", "/venue/bin/gateway", ""}},
			want:    []string{classFile + ": " + launchCase + ": no attributed Go launch in --launches evidence"},
		},
		"error: exempt case without recorded approval": {
			changes: []change{{approvalFile, "\tDormant\t", ""}},
			want:    []string{approvalFile + `: test/source.test.js:4:1 "Dormant": missing approval for unsupported:dormant-code`},
		},
		"error: approval for a different exemption class": {
			changes: []change{{approvalFile, "unsupported:dormant-code", "unsupported:windows-only"}},
			want:    []string{approvalFile + `: test/source.test.js:4:1 "Dormant": approved class "unsupported:windows-only" does not match classification "unsupported:dormant-code"`},
		},
		"error: orphan approval": {
			changes: []change{{approvalFile, "", "test/source.test.js\t1\t1\tAlpha\tunsupported:dormant-code\tReview.\n"}},
			want:    []string{approvalFile + ": " + alphaCase + ": approval does not name an exempt case"},
		},
		"error: due matrix row lacks its pending target": {
			args:    []string{"mapping", "phase-4"},
			changes: []change{{matrixFile, "; pending=internal/gate.TestReplacement/subtest |", " |"}},
			want:    []string{matrixFile + ": replacement: no-source-test row needs a pending Go test"},
		},
		"success: structure does not require future matrix targets": {
			changes: []change{{matrixFile, "; pending=internal/gate.TestReplacement/subtest |", " |"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS("testdata/valid")); err != nil {
				t.Fatal(err)
			}
			for _, change := range tt.changes {
				file := filepath.Join(root, filepath.FromSlash(change.file))
				contents, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				text := string(contents)
				switch {
				case change.old == "":
					text += change.new
				case change.new == "":
					var kept []string
					for line := range strings.SplitSeq(text, "\n") {
						if !strings.Contains(line, change.old) {
							kept = append(kept, line)
						}
					}
					text = strings.Join(kept, "\n")
				default:
					text = strings.ReplaceAll(text, change.old, change.new)
				}
				if text == string(contents) {
					t.Fatalf("mutation did not change %s: %q", change.file, change.old)
				}
				if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			args := tt.args
			if len(args) != 0 && args[0] == "results" {
				args = append(slices.Clone(args), "--commit", fixtureCommit)
			}
			exit := run(root, root, args, &stdout, &stderr)
			wantExit := 0
			if len(tt.want) != 0 {
				wantExit = 1
			}
			if exit != wantExit {
				t.Errorf("run(%q) exit = %d, want %d\nstdout: %s\nstderr: %s", tt.args, exit, wantExit, &stdout, &stderr)
			}
			var got []string
			if stderr.Len() != 0 {
				text := strings.ReplaceAll(stderr.String(), root+string(filepath.Separator), "")
				got = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			}
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("diagnostics differ (-want +got):\n%s", diff)
			}
		})
	}
}
