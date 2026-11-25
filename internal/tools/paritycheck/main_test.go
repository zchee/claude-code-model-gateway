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
		replacementPair = `test/source.test.js:3:1 "Replacement" -> test/source.test.js:35:2`
		alphaPair       = alphaCase + " -> test/helper.js:20:3"
		betaPair        = betaCase + " -> test/helper.js:20:3"
		embedded        = betaCase + " -> test/source.test.js:25:2"
		sharedEvent     = "TestGate/shared"
	)
	type change struct{ file, old, new string }
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
			changes: []change{{siteFile, "Beta\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\t\tphase-2", "Beta\ttest/helper.js\t20\t3\t\t\tphase-2"}},
		},
		"error: missing classification expansion": {
			changes: []change{{classFile, "Alpha\t1\t1\t", ""}},
			want:    []string{classFile + ": " + alphaCase + ": missing census case"},
		},
		"error: classification expansion missing from census": {
			changes: []change{{classFile, "", "Outside\t1\t1\ttest/source.test.js\tgo-port\tphase-2\tinternal/gate\ttask\tNew row.\n"}},
			want:    []string{classFile + `: test/source.test.js:1:1 "Outside": case not in ` + censusFile},
		},
		"error: matrix lost all citations for a due case": {
			changes: []change{{matrixFile, "| beta |", ""}},
			want: []string{
				matrixFile + ": beta: ledger names a missing matrix row",
				matrixFile + ": beta: locked row is missing",
				matrixFile + ": phase scope ledger: declared total 5 does not match the 4 matrix rows",
				matrixFile + ": " + betaCase + ": case not cited as proof by any row",
			},
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
			changes: []change{{siteFile, "Beta\ttest/source.test.js\t25", ""}},
			want:    []string{siteFile + ": " + embedded + ": missing expected pair (via embedded:node-e)"},
		},
		"error: mapping pair absent from generated evidence": {
			changes: []change{{pairFile, "embedded:node-e", ""}},
			want:    []string{siteFile + ": " + embedded + ": pair not in generated evidence after overrides"},
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
			want:    []string{pairFile + `: test/source.test.js:3:1 "Replacement": due case has no assertion pair or removal override`},
		},
		"success: removal override explains a due case without pairs": {
			changes: []change{
				{pairFile, "\tReplacement\t", ""},
				{siteFile, "\tReplacement\t", ""},
				{overFile, "", "test/source.test.js\t3\t1\tReplacement\ttest/source.test.js\t35\t2\tremove\tSource call is unreachable.\n"},
			},
		},
		"error: reason outside closed list": {
			changes: []change{{classFile, "unsupported:dormant-code", "unsupported:unreviewed"}},
			want: []string{
				approvalFile + `: test/source.test.js:4:1 "Dormant": approved class "unsupported:dormant-code" does not match classification "unsupported:unreviewed"`,
				classFile + `: test/source.test.js:4:1 "Dormant": invalid class or reason "unsupported:unreviewed"`,
			},
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
			changes: []change{{siteFile, "Alpha\ttest/helper.js\t20\t3\tinternal/gate.TestGate/shared\t\tphase-2", "Alpha\ttest/helper.js\t20\t3\t\t\tphase-2"}},
			want:    []string{siteFile + ": " + alphaPair + `: missing or invalid Go test target ""`},
		},
		"error: not-ported target without reviewer approval": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/shared", "not-ported:harness-internal"}},
			want: []string{
				siteFile + ": " + alphaPair + ": not-ported target needs reviewer approval",
				siteFile + ": " + betaPair + ": not-ported target needs reviewer approval",
			},
		},
		"error: shared target for distinct sites without per-pair review": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/child", "internal/gate.TestGate/shared"}},
			want: []string{
				siteFile + ": " + alphaPair + `: shared target "internal/gate.TestGate/shared" needs a reviewed unconditional check for this pair`,
				siteFile + ": " + betaPair + `: shared target "internal/gate.TestGate/shared" needs a reviewed unconditional check for this pair`,
			},
		},
		"error: difference without a named replacement": {
			changes: []change{{planFile, "`internal/gate.TestReplacement` (phase-4); `internal/gate.TestPackagedReplacement` (phase-5)", "No test"}},
			want:    []string{planFile + ": new-form: no valid Go replacement test named"},
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
		"success: earlier difference uses its own replacement before the packaging test": {
			args:    phase4,
			changes: []change{{"go.json", "TestPackagedReplacement", ""}},
		},
		"error: final gate misses an otherwise unreferenced plan replacement": {
			args:    final,
			changes: []change{{"go.json", "TestPackagedReplacement", ""}},
			want:    []string{planFile + ": new-form: no exact Go test run for example.org/port/internal/gate.TestPackagedReplacement in --go-json evidence"},
		},
		"error: pair due before case phase": {
			args:    mapping,
			changes: []change{{siteFile, "internal/gate.TestGate/shared\t\tphase-2", "internal/gate.TestGate/shared\t\tphase-1"}},
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
				siteFile + ": " + alphaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
				siteFile + ": " + betaPair + ": no exact Go test run for example.org/port/internal/gate.TestGate/shared in --go-json evidence",
			},
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
			exit := run(root, root, tt.args, &stdout, &stderr)
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
