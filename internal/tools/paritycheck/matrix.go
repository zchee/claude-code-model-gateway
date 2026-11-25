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
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	tsvEscapes        = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\r", `\r`, "\n", `\n`)
	slugPattern       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	tableSeparator    = regexp.MustCompile(`^\|(?:\s*:?-{3,}:?\s*\|)+$`)
	replacementCell   = regexp.MustCompile("^`([^`]+)` \\((phase-[0-5])\\)$")
	differenceHeading = regexp.MustCompile(`(?m)^#{1,3} [^\n]+$`)
	citation          = regexp.MustCompile("^`([^`]+)` `(.+)`$")
	ledgerHeading     = regexp.MustCompile(`^### (phase-[0-5]) — ([0-9]+) rows?$`)
	ledgerTotal       = regexp.MustCompile(`^\*\*Total: ([0-9]+) rows?\.\*\*$`)
	markerPrefix      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*[=:]`)
	sourcePosition    = regexp.MustCompile(`^[^\s:]+:[1-9][0-9]*(?::[1-9][0-9]*)?$`)
)

type matrixRow struct {
	area, phase, pending, difference string
	noSourceTest                     bool
}

// tableFields splits on unescaped pipes, retaining empty boundary fields.
func tableFields(line string) []string {
	var fields []string
	start, backslashes := 0, 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			backslashes++
			continue
		case '|':
			if backslashes%2 == 0 {
				fields = append(fields, strings.TrimSpace(strings.ReplaceAll(line[start:i], `\|`, `|`)))
				start = i + 1
			}
		}
		backslashes = 0
	}
	return append(fields, strings.TrimSpace(strings.ReplaceAll(line[start:], `\|`, `|`)))
}

func tableCells(line string) []string {
	fields := tableFields(line)
	if len(fields) < 3 || fields[0] != "" || fields[len(fields)-1] != "" {
		return nil
	}
	return fields[1 : len(fields)-1]
}

type replacement struct {
	test  goTest
	phase string
}

func (c *checker) readDifferences() {
	text, ok := c.read(planFile)
	if !ok {
		return
	}
	c.diffs = make(map[string][]replacement)
	before, section, found := strings.Cut(markdownOutsideFences(text), "### 3.10 Differences from the source\n")
	if !found {
		c.problem(planFile, "difference table", "missing section")
		return
	}
	if next := differenceHeading.FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	const phaseColumn = "Replacement tests (phase)"
	hasHeader := false
	candidates := 0
	lineNum := strings.Count(before, "\n") + 1
	for line := range strings.SplitSeq(section, "\n") {
		lineNum++
		line = strings.TrimSpace(line)
		if !hasHeader {
			cells := tableCells(line)
			hasHeader = len(cells) == 8 && cells[0] == "#" && cells[1] == "slug" && cells[6] == "Required Go replacement test" && cells[7] == phaseColumn
			continue
		}
		// A gap does not end this table. Count table-shaped lines without
		// relying on a valid slug or on both boundary pipes.
		if tableSeparator.MatchString(line) || len(tableFields(line)) < 2 {
			continue
		}
		candidates++
		cells := tableCells(line)
		if len(cells) != 8 || !slugPattern.MatchString(cells[1]) {
			c.problem(planFile, strconv.Itoa(lineNum), "invalid difference table row")
			continue
		}
		slug := cells[1]
		if _, dup := c.diffs[slug]; dup {
			c.problem(planFile, slug, "duplicate difference slug")
			continue
		}
		var targets []replacement
		for entry := range strings.SplitSeq(cells[7], "<br>") {
			parts := replacementCell.FindStringSubmatch(entry)
			if parts != nil {
				if t, ok := c.goTarget(parts[1]); ok {
					targets = append(targets, replacement{test: t, phase: parts[2]})
					continue
				}
			}
			c.problem(planFile, slug, "invalid replacement test entry %q", entry)
		}
		c.diffs[slug] = targets
		c.checkDifference(planFile, slug, slug, true)
	}
	if !hasHeader {
		c.problem(planFile, "difference table", "missing %q column", phaseColumn)
		c.diffs = nil
	} else if len(c.diffs) == 0 || len(c.diffs) != candidates {
		c.problem(planFile, "difference table", "parsed %d difference rows, found %d table-shaped rows", len(c.diffs), candidates)
	}
}

func (c *checker) checkDifference(file string, key any, slug string, replacement bool) {
	if c.diffs == nil {
		return
	}
	targets, ok := c.diffs[slug]
	if !ok {
		c.problem(file, key, "unknown difference slug %q", slug)
	} else if replacement && len(targets) == 0 {
		c.problem(planFile, slug, "no valid Go replacement test named")
	}
}

// markdownOutsideFences keeps line numbers while excluding fenced examples.
func markdownOutsideFences(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var fence byte
	var width int
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if len(line) != 0 && (line[0] == '`' || line[0] == '~') {
			n := 0
			for n < len(line) && line[n] == line[0] {
				n++
			}
			if fence == 0 && n >= 3 {
				fence, width = line[0], n
			} else if line[0] == fence && n >= width && line[n:] == "" {
				fence = 0
				lines[i] = ""
			}
		}
		if fence != 0 {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func (c *checker) checkMatrix() {
	text, ok := c.read(matrixFile)
	if !ok {
		return
	}
	// A citation may omit the column only if path, line and full name
	// resolve to one exact census key.
	byCitation := make(map[string][]caseKey, len(c.census)*2)
	for key := range c.census {
		location := fmt.Sprintf("%s:%d", key.loc.path, key.loc.line)
		byCitation[location+"\t"+key.name] = append(byCitation[location+"\t"+key.name], key)
		byCitation[key.loc.String()+"\t"+key.name] = []caseKey{key}
	}
	resolve := func(id, ref string) (caseKey, bool) {
		parts := citation.FindStringSubmatch(ref)
		if parts == nil {
			c.problem(matrixFile, id, "invalid source citation %q", ref)
			return caseKey{}, false
		}
		// The Markdown code span is the readable name; the TSV escapes
		// literal backslashes and control characters on write.
		name := tsvEscapes.Replace(parts[2])
		keys := byCitation[parts[1]+"\t"+name]
		if len(keys) != 1 {
			if c.census != nil {
				c.problem(matrixFile, id, "citation must name one exact census case: %s", ref)
			}
			return caseKey{}, false
		}
		return keys[0], true
	}

	c.matrix = make(map[string]matrixRow)
	covered := make(map[caseKey]bool)
	before, ledger, found := strings.Cut(markdownOutsideFences(text), "\n## Phase scope ledger\n")
	if !found {
		c.problem(matrixFile, "phase scope ledger", "missing heading")
	}
	header := []string{"Row id", "Area", "Assertion", "Source tests", "Due", "Go package", "Source of the row"}
	lineNum := 0
	inTable := false
	for line := range strings.SplitSeq(before, "\n") {
		lineNum++
		line = strings.TrimSpace(line)
		cells := tableCells(line)
		if slices.Equal(cells, header) {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if len(tableFields(line)) >= 2 {
				c.problem(matrixFile, strconv.Itoa(lineNum), "matrix row must begin with a pipe")
			} else {
				inTable = false
			}
			continue
		}
		if tableSeparator.MatchString(line) {
			continue
		}
		if len(cells) != 7 {
			c.problem(matrixFile, strconv.Itoa(lineNum), "got %d table cells, want seven", len(cells))
			continue
		}
		id := cells[0]
		if !slugPattern.MatchString(id) {
			c.problem(matrixFile, strconv.Itoa(lineNum), "invalid row id %q", id)
			continue
		}
		if _, duplicate := c.matrix[id]; duplicate {
			c.problem(matrixFile, id, "duplicate row id")
			continue
		}
		row := matrixRow{area: cells[1], phase: cells[4], noSourceTest: cells[3] == "no-source-test"}
		if row.area == "" || cells[2] == "" || !validPhase(row.phase) || !validPackage(strings.Trim(cells[5], "`")) || cells[6] == "" {
			c.problem(matrixFile, id, "invalid area, assertion, phase, package or provenance")
		}
		superseded := make(map[caseKey]bool)
		crossLinks := make(map[string]string)
		supersedes, phaseGate, shipped := false, false, false
		for marker := range strings.SplitSeq(cells[6], ";") {
			marker = strings.TrimSpace(marker)
			switch k, value, _ := strings.Cut(marker, "="); k {
			case "supersedes", "assertion-site":
				if !sourcePosition.MatchString(value) {
					c.problem(matrixFile, id, "invalid %s location %q", k, value)
				}
				if k == "supersedes" {
					supersedes = true
				}
			case "shipped-source":
				shipped = true
				if value == "." || !fs.ValidPath(value) || strings.ContainsAny(value, "\\\t\x00") {
					c.problem(matrixFile, id, "invalid shipped-source path %q", value)
				}
			case "integration-from", "narrower-from":
				origin, note, _ := strings.Cut(value, " ")
				phase, pkg, ok := strings.Cut(origin, "@")
				if !ok || !validPhase(phase) || !validPackage(pkg) || (note != "" && (!strings.HasPrefix(note, "(") || !strings.HasSuffix(note, ")"))) {
					c.problem(matrixFile, id, "invalid %s marker %q", k, value)
					continue
				}
				if k == "integration-from" && phase >= row.phase {
					c.problem(matrixFile, id, "integration-from must name a phase earlier than %s", row.phase)
				}
				crossLinks[origin] = k
			case "superseded-case":
				if key, ok := resolve(id, value); ok {
					superseded[key] = true
				}
			case "due-at":
				if value != "phase-gate" {
					c.problem(matrixFile, id, "invalid due-at marker %q", value)
				}
				phaseGate = value == "phase-gate"
			case "pending":
				test, ok := c.goTarget(value)
				if !ok || row.pending != "" {
					c.problem(matrixFile, id, "invalid or duplicate pending target %q", value)
				}
				for _, replacements := range c.diffs {
					for _, replacement := range replacements {
						if replacement.test == test && replacement.phase < row.phase {
							c.problem(matrixFile, id, "pending test %s is scheduled in %s by the plan, earlier than row phase %s", test, replacement.phase, row.phase)
						}
					}
				}
				row.pending = value
			case "mixed-with":
				c.checkDifference(matrixFile, id, value, false)
			default:
				if slug, ok := strings.CutPrefix(marker, "difference:"); ok {
					c.checkDifference(matrixFile, id, slug, false)
					if row.difference != "" {
						c.problem(matrixFile, id, "multiple differences listed")
					}
					row.difference = slug
				} else if markerPrefix.MatchString(marker) {
					c.problem(matrixFile, id, "unknown matrix marker %q", marker)
				}
			}
		}
		if shipped && !phaseGate {
			c.problem(matrixFile, id, "shipped-source needs due-at=phase-gate")
		}
		if supersedes != (len(superseded) != 0) && c.census != nil {
			c.problem(matrixFile, id, "supersedes and superseded-case must be paired")
		}
		c.matrix[id] = row
		origins := make(map[string]bool)
		for key := range superseded {
			if cl, ok := c.classes[key]; ok {
				origins[cl.phase+"@"+cl.pkg] = true
			}
		}
		if !row.noSourceTest {
			for ref := range strings.SplitSeq(cells[3], "<br>") {
				key, ok := resolve(id, strings.TrimSpace(ref))
				if !ok {
					continue
				}
				if superseded[key] {
					c.problem(matrixFile, id, "cites superseded case %s as proof", key)
					continue
				}
				cl, ok := c.classes[key]
				if !ok {
					continue
				}
				origins[cl.phase+"@"+cl.pkg] = true
				if cl.class != "conformance" && cl.class != "go-port" {
					c.problem(matrixFile, id, "cites %s as proof despite class %q", key, cl.class)
					continue
				}
				if cl.gate == "phase" && !phaseGate {
					c.problem(matrixFile, id, "phase-gated citation %s needs due-at=phase-gate", key)
				}
				covered[key] = true
			}
		}
		for origin, kind := range crossLinks {
			if c.classes != nil && !origins[origin] {
				c.problem(matrixFile, id, "%s=%s does not identify a cited case's phase and package", kind, origin)
			}
		}
	}
	for key, cl := range c.classes {
		if (cl.class == "conformance" || cl.class == "go-port") && !covered[key] && c.census != nil {
			if _, ok := c.census[key]; ok {
				c.problem(matrixFile, key, "case not cited as proof by any row")
			}
		}
	}
	c.checkMatrixLock()
	if found {
		c.checkLedger(ledger)
	}
}

func (c *checker) checkMatrixLock() {
	text, ok := c.read(lockFile)
	if !ok {
		return
	}
	locked := make(map[string]bool)
	for id := range strings.SplitSeq(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n") {
		if !slugPattern.MatchString(id) || locked[id] {
			c.problem(lockFile, id, "invalid or duplicate row id")
		}
		locked[id] = true
		if _, ok := c.matrix[id]; !ok {
			c.problem(matrixFile, id, "locked row is missing")
		}
	}
	for id := range c.matrix {
		if !locked[id] {
			c.problem(lockFile, id, "matrix row not in lock")
		}
	}
}

func (c *checker) checkLedger(text string) {
	seen := make(map[string]bool)
	counts := make(map[string]int)
	declared := make(map[string]int)
	phase := ""
	total := -1
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "### "):
			parts := ledgerHeading.FindStringSubmatch(line)
			if parts == nil {
				c.problem(matrixFile, "phase scope ledger", "invalid phase heading %q", line)
				phase = ""
				continue
			}
			phase = parts[1]
			if _, duplicate := declared[phase]; duplicate {
				c.problem(matrixFile, phase, "duplicate ledger heading")
			}
			declared[phase], _ = strconv.Atoi(parts[2])
		case strings.HasPrefix(line, "- "):
			area, list, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
			if phase == "" || !ok || list == "" {
				c.problem(matrixFile, "phase scope ledger", "invalid ledger entry %q", line)
				continue
			}
			for field := range strings.SplitSeq(list, ", ") {
				id, ok := unquoteCode(field)
				if !ok || !slugPattern.MatchString(id) {
					c.problem(matrixFile, phase, "invalid ledger row id %q", field)
					continue
				}
				if seen[id] {
					c.problem(matrixFile, id, "duplicate phase ledger row")
				}
				seen[id] = true
				counts[phase]++
				row, ok := c.matrix[id]
				if !ok {
					c.problem(matrixFile, id, "ledger names a missing matrix row")
				} else if row.phase != phase || row.area != area {
					c.problem(matrixFile, id, "ledger phase/area %s/%s disagrees with matrix %s/%s", phase, area, row.phase, row.area)
				}
			}
		case strings.HasPrefix(line, "**Total:"):
			parts := ledgerTotal.FindStringSubmatch(line)
			if parts == nil || total >= 0 {
				c.problem(matrixFile, "phase scope ledger", "invalid or duplicate total %q", line)
				continue
			}
			total, _ = strconv.Atoi(parts[1])
		}
	}
	for phase := range 6 {
		name := "phase-" + strconv.Itoa(phase)
		want, ok := declared[name]
		if !ok {
			c.problem(matrixFile, name, "missing phase ledger heading")
		} else if counts[name] != want {
			c.problem(matrixFile, name, "ledger declares %d rows, lists %d", want, counts[name])
		}
	}
	for id := range c.matrix {
		if !seen[id] {
			c.problem(matrixFile, id, "matrix row is missing from the phase ledger")
		}
	}
	if total != len(c.matrix) {
		c.problem(matrixFile, "phase scope ledger", "declared total %d does not match the %d matrix rows", total, len(c.matrix))
	}
}

func unquoteCode(s string) (string, bool) {
	if len(s) < 2 || s[0] != '`' || s[len(s)-1] != '`' {
		return "", false
	}
	return s[1 : len(s)-1], true
}
