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
	"regexp"
	"slices"
	"strings"
)

func (c *checker) checkPairs() {
	expected := make(map[pairKey]string)
	pairsOK := c.rows(pairFile, pairColumns+" call via", func(n int, row map[string]string) {
		key, ok := c.pairKey(pairFile, n, row)
		if !ok {
			return
		}
		if _, duplicate := expected[key]; duplicate {
			c.problem(pairFile, key, "duplicate pair")
			return
		}
		if strings.TrimSpace(row["call"]) == "" || strings.TrimSpace(row["via"]) == "" {
			c.problem(pairFile, key, "missing call or attribution")
		}
		expected[key] = row["via"]
	})
	removed := make(map[caseKey]bool)
	added := make(map[pairKey]bool)
	seen := make(map[pairKey]bool)
	overridesOK := c.rows(overFile, pairColumns+" action reason", func(n int, row map[string]string) {
		key, ok := c.pairKey(overFile, n, row)
		if !ok {
			return
		}
		if seen[key] {
			c.problem(overFile, key, "duplicate override")
			return
		}
		seen[key] = true
		if strings.TrimSpace(row["reason"]) == "" {
			c.problem(overFile, key, "override needs a reason")
			return
		}
		via, exists := expected[key]
		switch row["action"] {
		case "add":
			if pairsOK && via != "override" {
				c.problem(overFile, key, "added pair must appear in generated evidence via override")
			}
			added[key] = true
		case "remove":
			if exists {
				c.problem(overFile, key, "removed pair still appears in generated evidence")
			}
			removed[key.caseKey] = true
		default:
			c.problem(overFile, key, "invalid override action %q", row["action"])
		}
	})

	sites := make(map[pairKey]mapping)
	sitesOK := c.rows(siteFile, pairColumns+" target review due", func(n int, row map[string]string) {
		key, ok := c.pairKey(siteFile, n, row)
		if !ok {
			return
		}
		if _, duplicate := sites[key]; duplicate {
			c.problem(siteFile, key, "duplicate pair")
			return
		}
		m := mapping{due: row["due"], target: row["target"], review: row["review"]}
		sites[key] = m
		cl, classified := c.classes[key.caseKey]
		switch {
		case classified && cl.exempt():
			c.problem(siteFile, key, "mapping must not include an exempt case")
		case !validPhase(m.due):
			c.problem(siteFile, key, "invalid due phase %q", m.due)
		case classified && m.due < cl.phase:
			c.problem(siteFile, key, "due phase %s is earlier than case phase %s", m.due, cl.phase)
		}
	})
	c.sites = sites
	if !pairsOK || !overridesOK {
		return
	}

	obligations := make(map[caseKey]bool)
	for key, via := range expected {
		obligations[key.caseKey] = true
		if via == "override" && !added[key] {
			c.problem(pairFile, key, "override attribution has no matching add row")
		}
		if _, ok := sites[key]; sitesOK && !ok {
			if cl, classified := c.classes[key.caseKey]; classified && !cl.exempt() {
				c.problem(siteFile, key, "missing expected pair (via %s)", via)
			}
		}
	}
	for key := range sites {
		if _, ok := expected[key]; !ok {
			c.problem(siteFile, key, "pair not in generated evidence after overrides")
		}
	}
	for key, cl := range c.classes {
		if _, classified := c.census[key]; classified && !cl.exempt() && !obligations[key] && !removed[key] {
			c.problem(pairFile, key, "due case has no assertion pair or removal override")
		}
	}
}

type goTest struct {
	pkg, name string
}

func (t goTest) String() string { return t.pkg + "." + t.name }

// The first component of a Go test name follows testing's TestFoo rule;
// following components are the names as reported by go test -json.
var testName = regexp.MustCompile(`^Test(?:[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{Nd}_][\p{L}\p{Nd}_]*)?(?:/[^\p{Z}\p{C}\s/]+)*$`)

func (c *checker) goTarget(target string) (goTest, bool) {
	if c.module == "" {
		return goTest{}, false
	}
	// The package may contain slashes or dots. The first valid .Test
	// separator leaves any dots in the subtest name intact.
	offset := 0
	for part := range strings.SplitSeq(target, ".Test") {
		offset += len(part)
		if offset == len(target) {
			break
		}
		pkg, name := target[:offset], target[offset+1:]
		offset += len(".Test")
		if rel, full := strings.CutPrefix(pkg, c.module+"/"); full {
			pkg = rel
		} else if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
			return goTest{}, false // A full import path must be below the module.
		}
		if validPackage(pkg) && testName.MatchString(name) {
			return goTest{pkg: c.module + "/" + pkg, name: name}, true
		}
	}
	return goTest{}, false
}

func (c *checker) mapping(phase, pkg string) {
	c.checkMappings(phase, pkg, false)
}

func (c *checker) checkMappings(phase, pkg string, cumulative bool) {
	type sharing struct {
		first     location
		different bool
	}
	shared := make(map[goTest]sharing)
	for key, m := range c.sites {
		if t, ok := c.goTarget(m.target); ok {
			group, seen := shared[t]
			if !seen {
				group.first = key.site
			} else if group.first != key.site {
				group.different = true
			}
			shared[t] = group
		}
	}
	inScope := 0
	for key, m := range c.sites {
		cl := c.classes[key.caseKey]
		if !phaseMatches(m.due, phase, cumulative) || (pkg != "" && (cl.pkg != pkg || cl.gate != "task")) {
			continue
		}
		inScope++
		slug, difference := strings.CutPrefix(cl.class, "difference:")
		if m.target == "" && !difference && cl.class != "go-port" {
			continue
		}
		if reason, exempt := strings.CutPrefix(m.target, "not-ported:"); exempt {
			switch reason {
			case "js-runtime-artifact", "harness-internal", "source-doc-wording":
			default:
				if slug, ok := strings.CutPrefix(reason, "changed-by-difference:"); ok {
					c.checkDifference(siteFile, key, slug, true)
				} else {
					c.problem(siteFile, key, "invalid not-ported reason %q", reason)
				}
			}
			if strings.TrimSpace(m.review) == "" {
				c.problem(siteFile, key, "not-ported target needs reviewer approval")
			}
			continue
		}
		test, ok := c.goTarget(m.target)
		if !ok {
			c.problem(siteFile, key, "missing or invalid Go test target %q", m.target)
			continue
		}
		if replacements := c.diffs[slug]; difference && len(replacements) != 0 && !slices.ContainsFunc(replacements, func(r goTest) bool {
			name, _, _ := strings.Cut(test.name, "/")
			want, _, _ := strings.Cut(r.name, "/")
			return test.pkg == r.pkg && name == want
		}) {
			c.problem(siteFile, key, "target %q is not a replacement test for difference %q", m.target, slug)
		}
		if shared[test].different && strings.TrimSpace(m.review) == "" {
			c.problem(siteFile, key, "shared target %q needs a reviewed unconditional check for this pair", m.target)
		}
	}
	if pkg == "" {
		for id, row := range c.matrix {
			if !phaseMatches(row.phase, phase, cumulative) {
				continue
			}
			if row.noSourceTest && row.pending == "" {
				c.problem(matrixFile, id, "no-source-test row needs a pending Go test")
			}
			if row.difference != "" {
				c.checkDifference(matrixFile, id, row.difference, true)
			}
		}
	}
	if inScope == 0 {
		scope := phase
		if pkg != "" {
			scope += " " + pkg
		}
		c.problem(siteFile, scope, "empty obligation ledger")
	}
}

func phaseMatches(due, phase string, cumulative bool) bool {
	return due == phase || (cumulative && validPhase(due) && due < phase)
}
