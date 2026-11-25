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
	"crypto/sha256"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	censusFile   = "test/conformance/baseline/census.tsv"
	classFile    = "test/conformance/classification.tsv"
	approvalFile = "test/conformance/exemptions-approved.tsv"
	launchFile   = "test/conformance/launch-sites.tsv"
	pairFile     = "test/conformance/assertion-pairs.tsv"
	overFile     = "test/conformance/assertion-overrides.tsv"
	siteFile     = "test/conformance/assertion-sites.tsv"
	matrixFile   = "docs/port/parity-matrix.md"
	lockFile     = "docs/port/parity-matrix.lock"
	planFile     = "docs/port/model-gateway-go-port.md"
	ownersFile   = "docs/port/test-ownership.md"

	caseColumns = "path line column name"
	pairColumns = caseColumns + " site_path site_line site_column"
)

type location struct {
	path      string
	line, col int
}

func (l location) String() string {
	return fmt.Sprintf("%s:%d:%d", l.path, l.line, l.col)
}

type caseKey struct {
	loc  location
	name string
}

func (k caseKey) String() string {
	return fmt.Sprintf("%s %q", k.loc, k.name)
}

type pairKey struct {
	caseKey caseKey
	site    location
}

func (k pairKey) String() string {
	return fmt.Sprintf("%s -> %s", k.caseKey, k.site)
}

type classification struct {
	class, phase, pkg, gate string
}

type mapping struct {
	due, target, review string
}

type checker struct {
	root     string
	problems []string
	module   string
	census   map[caseKey]string
	classes  map[caseKey]classification
	matrix   map[string]matrixRow
	diffs    map[string][]replacement
	sites    map[pairKey]mapping
}

func (c *checker) problem(file string, key any, message string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf("%s: %s: %s", file, key, fmt.Sprintf(message, args...)))
}

func (c *checker) read(file string) (string, bool) {
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.root, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		c.problem(file, "file", "%v", err)
		return "", false
	}
	if !utf8.Valid(data) {
		c.problem(file, "file", "invalid UTF-8")
		return "", false
	}
	return string(data), true
}

// rows reads the inventory's raw, unquoted TSV; the escape sequences stay in
// their encoded form so keys have the same representation in every input.
func (c *checker) rows(file, required string, visit func(int, map[string]string)) bool {
	text, ok := c.read(file)
	if !ok {
		return false
	}
	return c.parseRows(file, text, required, 0, visit)
}

func (c *checker) parseRows(file, text, required string, prefixLines int, visit func(int, map[string]string)) bool {
	header, rest, _ := strings.Cut(text, "\n")
	columns := strings.Split(strings.TrimSuffix(header, "\r"), "\t")
	row := make(map[string]string, len(columns))
	bad := false
	for _, name := range columns {
		if _, duplicate := row[name]; duplicate {
			c.problem(file, "header", "duplicate column %q", name)
			bad = true
		}
		if !headerColumn.MatchString(name) {
			c.problem(file, "header", "invalid column %q", name)
			bad = true
		}
		row[name] = ""
	}
	for name := range strings.FieldsSeq(required) {
		if _, exists := row[name]; !exists {
			c.problem(file, "header", "missing column %q", name)
			bad = true
		}
	}
	if bad {
		return false
	}
	n := 1 + prefixLines
	for line := range strings.SplitSeq(strings.TrimSuffix(rest, "\n"), "\n") {
		if rest == "" {
			break
		}
		n++
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if len(fields) != len(columns) {
			c.problem(file, strconv.Itoa(n), "got %d fields, want %d", len(fields), len(columns))
			continue
		}
		for i, field := range fields {
			row[columns[i]] = field
		}
		visit(n, row)
	}
	return true
}

func validPhase(s string) bool {
	return len(s) == len("phase-0") && strings.HasPrefix(s, "phase-") && s[6] >= '0' && s[6] <= '5'
}

var (
	headerColumn   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	packagePattern = regexp.MustCompile(`^[A-Za-z0-9_~.+-]+(?:/[A-Za-z0-9_~.+-]+)*$`)
)

func validPackage(s string) bool {
	return s != "." && fs.ValidPath(s) && packagePattern.MatchString(s)
}

func positive(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0 && strconv.Itoa(n) == s
}

func parseLocation(path, line, col string) (location, bool) {
	l, lOK := positive(line)
	k, kOK := positive(col)
	return location{path: path, line: l, col: k}, path != "." && fs.ValidPath(path) && !strings.ContainsAny(path, "\\\r\t\x00") && lOK && kOK
}

func (c *checker) caseKey(file string, n int, row map[string]string) (caseKey, bool) {
	loc, ok := parseLocation(row["path"], row["line"], row["column"])
	key := caseKey{loc: loc, name: row["name"]}
	if !ok || strings.TrimSpace(key.name) == "" {
		c.problem(file, strconv.Itoa(n), "invalid case key (%q, %q, %q, %q)", row["path"], row["line"], row["column"], row["name"])
		return key, false
	}
	return key, true
}

func (c *checker) pairKey(file string, n int, row map[string]string) (pairKey, bool) {
	key, ok := c.caseKey(file, n, row)
	loc, siteOK := parseLocation(row["site_path"], row["site_line"], row["site_column"])
	if !siteOK {
		c.problem(file, strconv.Itoa(n), "invalid assertion site (%q, %q, %q)", row["site_path"], row["site_line"], row["site_column"])
	}
	if ok && c.census != nil {
		if _, exists := c.census[key]; !exists {
			c.problem(file, key, "case not in %s", censusFile)
		}
	}
	return pairKey{caseKey: key, site: loc}, ok && siteOK
}

func (c *checker) structure() {
	if text, ok := c.read("go.mod"); ok {
		for line := range strings.SplitSeq(text, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
				c.module = fields[1]
				break
			}
		}
		if !validPackage(c.module) {
			c.problem("go.mod", "module", "missing or invalid module path")
		}
	}
	if text, ok := c.read(censusFile); ok {
		c.checkCensusDigest(text)
		c.census = c.readCensus(censusFile, text, 0)
	}
	c.readDifferences()
	c.readClasses()
	c.checkExemptionApprovals()
	c.checkLaunchSites()
	c.checkMatrix()
	c.checkPairs()
	c.checkMarkers()
}

func (c *checker) checkCensusDigest(text string) {
	const runFile = "test/conformance/baseline/run.json"
	metadata, ok := c.read(runFile)
	if !ok {
		return
	}
	var run struct {
		CensusDigest string `json:"census_sha256"`
	}
	if err := json.Unmarshal([]byte(metadata), &run); err != nil {
		c.problem(runFile, "census_sha256", "invalid baseline metadata: %v", err)
		return
	}
	if !digestPattern.MatchString(run.CensusDigest) {
		c.problem(runFile, "census_sha256", "missing or invalid SHA-256 digest")
		return
	}
	if fmt.Sprintf("%x", sha256.Sum256([]byte(text))) != run.CensusDigest {
		c.problem(censusFile, "census_sha256", "digest does not match %s", runFile)
	}
}

func (c *checker) readCensus(file, text string, prefixLines int) map[caseKey]string {
	statuses := make(map[caseKey]string)
	if !c.parseRows(file, text, caseColumns+" depth status detail", prefixLines, func(n int, row map[string]string) {
		key, ok := c.caseKey(file, n, row)
		if !ok {
			return
		}
		if _, duplicate := statuses[key]; duplicate {
			c.problem(file, key, "duplicate case")
			return
		}
		status := row["status"]
		switch status {
		case "pass", "fail", "skip", "todo", "cancelled":
		default:
			c.problem(file, key, "invalid status %q", status)
		}
		if _, ok := positive(row["depth"]); !ok && row["depth"] != "0" {
			c.problem(file, key, "invalid depth %q", row["depth"])
		}
		statuses[key] = status
	}) {
		return nil
	}
	if file == censusFile && len(statuses) == 0 {
		c.problem(file, "cases", "empty census")
	}
	return statuses
}

func (c *checker) readClasses() {
	type owner struct{ phase, pkg string }
	type counts struct{ due, conformance int }
	const phaseGate = "**phase gate**"
	owners := make(map[owner]counts)
	actual := make(map[owner]counts)
	ownership, ownersOK := c.read(ownersFile)
	if ownersOK {
		for line := range strings.SplitSeq(ownership, "\n") {
			cells := tableCells(strings.TrimSpace(line))
			if len(cells) == 0 || !strings.HasPrefix(cells[0], "phase-") {
				continue
			}
			if len(cells) != 6 || !validPhase(cells[0]) {
				c.problem(ownersFile, "ownership table", "invalid ownership row %q", line)
				continue
			}
			key := owner{cells[0], strings.Trim(cells[1], "`")}
			if _, duplicate := owners[key]; duplicate {
				c.problem(ownersFile, key.phase+" "+key.pkg, "duplicate ownership row")
				continue
			}
			due, dueOK := positive(cells[4])
			conf, confOK := positive(cells[5])
			if (!dueOK && cells[4] != "0") || (!confOK && cells[5] != "0") || conf > due {
				c.problem(ownersFile, key.phase+" "+key.pkg, "invalid ownership counts %q, %q", cells[4], cells[5])
			}
			owners[key] = counts{due, conf}
		}
	}
	classes := make(map[caseKey]classification)
	if !c.rows(classFile, caseColumns+" class phase package gate note", func(n int, row map[string]string) {
		key, ok := c.caseKey(classFile, n, row)
		if !ok {
			return
		}
		if _, duplicate := classes[key]; duplicate {
			c.problem(classFile, key, "duplicate case")
			return
		}
		cl := classification{class: row["class"], phase: row["phase"], pkg: row["package"], gate: row["gate"]}
		classes[key] = cl
		kind, reason, _ := strings.Cut(cl.class, ":")
		switch {
		case cl.class == "conformance", cl.class == "go-port":
		case kind == "difference":
			c.checkDifference(classFile, key, reason, true)
		case kind == "js-only" && (reason == "node-harness-self-test" || reason == "js-source-lint" || reason == "out-of-root-dependency" || reason == "developer-probe"):
		case kind == "unsupported" && (reason == "windows-only" || reason == "dormant-code"):
		default:
			c.problem(classFile, key, "invalid class or reason %q", cl.class)
		}
		if cl.exempt() {
			if cl.phase != "" || cl.pkg != "" || cl.gate != "" {
				c.problem(classFile, key, "exempt case must leave phase, package and gate empty")
			}
		} else {
			if !validPhase(cl.phase) || !validPackage(cl.pkg) || (cl.gate != "task" && cl.gate != "phase") {
				c.problem(classFile, key, "invalid phase, package or gate: %q, %q, %q", cl.phase, cl.pkg, cl.gate)
			} else if ownersOK {
				ownerKey := owner{cl.phase, cl.pkg}
				_, hasPackage := owners[ownerKey]
				if cl.gate == "phase" {
					ownerKey.pkg = phaseGate
				}
				if _, ok := owners[ownerKey]; !ok || !hasPackage {
					c.problem(classFile, key, "phase/package/gate not listed in %s", ownersFile)
				}
				n := actual[ownerKey]
				n.due++
				if cl.class == "conformance" {
					n.conformance++
				}
				actual[ownerKey] = n
			}
		}
	}) {
		return
	}
	c.classes = classes
	for key, declared := range owners {
		if got := actual[key]; got != declared {
			c.problem(ownersFile, key.phase+" "+key.pkg, "counts due=%d conformance=%d disagree with classification due=%d conformance=%d", declared.due, declared.conformance, got.due, got.conformance)
		}
	}
	if c.census == nil {
		return
	}
	for key := range c.census {
		if _, ok := classes[key]; !ok {
			c.problem(classFile, key, "missing census case")
		}
	}
	for key := range classes {
		if _, ok := c.census[key]; !ok {
			c.problem(classFile, key, "case not in %s", censusFile)
		}
	}
}

func (cl classification) exempt() bool {
	return strings.HasPrefix(cl.class, "js-only:") || strings.HasPrefix(cl.class, "unsupported:")
}

func (c *checker) checkExemptionApprovals() {
	seen := make(map[caseKey]bool)
	if !c.rows(approvalFile, caseColumns+" class approved", func(n int, row map[string]string) {
		key, ok := c.caseKey(approvalFile, n, row)
		if !ok {
			return
		}
		if seen[key] {
			c.problem(approvalFile, key, "duplicate exemption approval")
			return
		}
		seen[key] = true
		if strings.TrimSpace(row["approved"]) == "" {
			c.problem(approvalFile, key, "missing approval text")
		}
		if c.classes == nil {
			return
		}
		if cl := c.classes[key]; !cl.exempt() {
			c.problem(approvalFile, key, "approval does not name an exempt case")
		} else if row["class"] != cl.class {
			c.problem(approvalFile, key, "approved class %q does not match classification %q", row["class"], cl.class)
		}
	}) {
		return
	}
	for key, cl := range c.classes {
		if cl.exempt() && !seen[key] {
			c.problem(approvalFile, key, "missing approval for %s", cl.class)
		}
	}
}

func (c *checker) checkLaunchSites() {
	type launchOwner struct{ path, name string }
	type launchKey struct {
		site            location
		owner           caseKey
		kind, via, argv string
	}
	byOwner := make(map[launchOwner][]caseKey)
	for key := range c.census {
		short := launchOwner{key.loc.path, key.name}
		byOwner[short] = append(byOwner[short], key)
	}
	seen := make(map[launchKey]bool)
	launched := make(map[caseKey]bool)
	if !c.rows(launchFile, "path line column kind via case argv", func(n int, row map[string]string) {
		loc, ok := parseLocation(row["path"], row["line"], row["column"])
		if !ok {
			c.problem(launchFile, strconv.Itoa(n), "invalid launch site location")
			return
		}
		switch row["kind"] {
		case "cli", "worker", "supervisor", "shim", "hook", "helper":
		default:
			c.problem(launchFile, loc, "invalid launch kind %q", row["kind"])
			return
		}
		owner := caseKey{name: row["case"]}
		if owner.name != "-" && c.census != nil {
			_, withPath := row["case_path"]
			_, withLine := row["case_line"]
			_, withCol := row["case_column"]
			if withPath || withLine || withCol {
				owner.loc, ok = parseLocation(row["case_path"], row["case_line"], row["case_column"])
				if _, exists := c.census[owner]; !ok || !exists {
					c.problem(launchFile, loc, "launch does not name an exact census case: %s", owner)
					return
				}
			} else {
				matches := byOwner[launchOwner{loc.path, owner.name}]
				if len(matches) != 1 {
					c.problem(launchFile, loc, "launch name %q identifies %d census cases; need one", owner.name, len(matches))
					return
				}
				owner = matches[0]
			}
			launched[owner] = true
		}
		key := launchKey{loc, owner, row["kind"], row["via"], row["argv"]}
		if seen[key] {
			c.problem(launchFile, loc, "duplicate launch row for %q", owner.name)
		}
		seen[key] = true
	}) {
		return
	}
	for key, cl := range c.classes {
		if cl.class == "conformance" && c.census != nil && !launched[key] {
			c.problem(launchFile, key, "no launch site names conformance case")
		}
	}
}

var planMarker = regexp.MustCompile(`\b(?:T[0-9]+\.[0-9]+[a-z]?|(?:OD|AC|INV|D|PC|R|K|W|L|S)-?[0-9]+[a-z]?)\b`)

func (c *checker) checkMarkers() {
	root := os.DirFS(c.root)
	_ = fs.WalkDir(root, "test", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			c.problem(path, "file", "%v", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			c.problem(path, "file", "not a regular test inventory file")
			return nil
		}
		text, ok := c.read(path)
		if !ok {
			return nil
		}
		n := 0
		for line := range strings.SplitSeq(text, "\n") {
			n++
			if planMarker.MatchString(line) {
				c.problem(path, strconv.Itoa(n), "plan marker is not allowed under test/")
			}
		}
		return nil
	})
}
