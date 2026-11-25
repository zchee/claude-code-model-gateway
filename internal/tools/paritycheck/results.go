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
	"bufio"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type testOutcome struct {
	ran     bool
	passed  bool
	skipped bool
	failed  bool
}

type packageOutcome struct {
	started, passed, failed, cached bool
}

type goEvidence struct {
	file     string
	tests    map[goTest]testOutcome
	packages map[string]packageOutcome
}

func (c *checker) readGoEvents(path string) goEvidence {
	ev := goEvidence{file: path, tests: make(map[goTest]testOutcome), packages: make(map[string]packageOutcome)}
	f, err := os.Open(path)
	if err != nil {
		c.problem(path, "file", "%v", err)
		return ev
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 4<<20)
	n := 0
	for scanner.Scan() {
		n++
		var event struct {
			Action, Package, Test, Output, ImportPath string
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			c.problem(path, strconv.Itoa(n), "invalid Go test event: %v", err)
			continue
		}
		if event.ImportPath != "" && event.Action == "build-output" {
			continue
		}
		if event.ImportPath != "" && event.Action == "build-fail" {
			c.problem(path, event.ImportPath, "build failed")
			continue
		}
		if event.Action == "" || event.Package == "" {
			c.problem(path, strconv.Itoa(n), "Go event has no action or package")
			continue
		}
		if event.Test == "" {
			outcome := ev.packages[event.Package]
			switch event.Action {
			case "start":
				if outcome.started {
					c.problem(path, event.Package, "multiple package runs in one evidence file")
				}
				outcome.started = true
			case "pass":
				outcome.passed = outcome.started
			case "fail":
				outcome.failed = true
				c.problem(path, event.Package, "package failed")
			case "output":
				// go test replays run/pass events on a cache hit, then
				// labels the package summary with (cached).
				parts := strings.Split(strings.TrimSpace(event.Output), "\t")
				if len(parts) >= 3 && strings.TrimSpace(parts[0]) == "ok" && parts[1] == event.Package && strings.TrimSpace(parts[2]) == "(cached)" {
					outcome.cached = true
				}
			}
			ev.packages[event.Package] = outcome
			continue
		}
		key := goTest{pkg: event.Package, name: event.Test}
		outcome := ev.tests[key]
		switch event.Action {
		case "run":
			outcome.ran = ev.packages[event.Package].started
		case "pass":
			outcome.passed = outcome.ran
		case "skip":
			outcome.skipped = true
		case "fail":
			outcome.failed = true
		}
		ev.tests[key] = outcome
	}
	if err := scanner.Err(); err != nil {
		c.problem(path, "file", "read Go test events: %v", err)
	}
	return ev
}

func (c *checker) requireGoPass(t goTest, evidence []goEvidence, file string, key any) {
	found := false
	for _, ev := range evidence {
		outcome, ok := ev.tests[t]
		if !ok {
			continue
		}
		found = true
		pkg := ev.packages[t.pkg]
		switch {
		case pkg.cached:
			c.problem(ev.file, t, "cached Go test result for %s", key)
		case outcome.failed || pkg.failed:
			c.problem(ev.file, t, "failed Go test result for %s", key)
		case outcome.skipped:
			c.problem(ev.file, t, "skipped Go test result for %s", key)
		case !outcome.passed || !pkg.passed:
			c.problem(ev.file, t, "no completed run and pass for %s", key)
		}
	}
	if !found {
		c.problem(file, key, "no exact Go test run for %s in --go-json evidence", t)
	}
}

func (c *checker) results(phase string, files evidenceFiles) {
	cumulative := phase == "phase-5"
	c.checkMappings(phase, "", cumulative)
	var evidence []goEvidence
	for _, path := range files.goJSON {
		evidence = append(evidence, c.readGoEvents(path))
	}
	var conformance map[caseKey]string
	if files.census != "" {
		conformance = c.readCensus(files.census)
	}
	launched := make(map[caseKey]bool)
	if files.launches != "" {
		c.rows(files.launches, caseColumns+" binary", func(n int, row map[string]string) {
			key, ok := c.caseKey(files.launches, n, row)
			if !ok {
				return
			}
			if path := row["binary"]; !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
				c.problem(files.launches, key, "launch needs an absolute, clean Go binary path")
				return
			}
			if _, ok := c.census[key]; !ok && c.census != nil {
				c.problem(files.launches, key, "launch case not in %s", censusFile)
				return
			}
			launched[key] = true
		})
	}
	for key, m := range c.sites {
		if !phaseMatches(m.due, phase, cumulative) {
			continue
		}
		if t, ok := c.goTarget(m.target); ok {
			c.requireGoPass(t, evidence, siteFile, key)
		}
	}
	for key, cl := range c.classes {
		if !phaseMatches(cl.phase, phase, cumulative) {
			continue
		}
		if cl.class == "conformance" {
			if status := conformance[key]; status != "pass" {
				c.problem(classFile, key, "no passing conformance case in --census evidence (status %q)", status)
			}
			if !launched[key] {
				c.problem(classFile, key, "no attributed Go launch in --launches evidence")
			}
		}
	}
	for id, row := range c.matrix {
		if !phaseMatches(row.phase, phase, cumulative) {
			continue
		}
		if test, ok := c.goTarget(row.pending); ok {
			c.requireGoPass(test, evidence, matrixFile, id)
		}
	}
	if cumulative {
		for slug, replacements := range c.diffs {
			c.checkDifference(planFile, slug, slug, true)
			for _, test := range replacements {
				c.requireGoPass(test, evidence, planFile, slug)
			}
		}
	}
}
