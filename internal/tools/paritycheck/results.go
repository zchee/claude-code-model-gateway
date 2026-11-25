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
	"time"
)

type testOutcome struct {
	ran     bool
	passed  bool
	skipped bool
	failed  bool
}

type packageOutcome struct {
	started, terminal, passed, failed, summary, cached bool
	active                                             int
}

type goEvidence struct {
	file     string
	tests    map[goTest]testOutcome
	packages map[string]packageOutcome
}

type evidenceRun struct {
	commit, id string
}

func (r *evidenceRun) check(c *checker, file string, line []byte) {
	var meta struct {
		ID     string `json:"run_id"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(line, &meta); err != nil {
		c.problem(file, "metadata", "invalid run metadata: %v", err)
		return
	}
	if strings.TrimSpace(meta.ID) == "" || strings.TrimSpace(meta.ID) != meta.ID || strings.ContainsAny(meta.ID, "\r\n\t") {
		c.problem(file, "metadata", "missing or invalid run identifier")
	} else if r.id == "" {
		r.id = meta.ID
	} else if meta.ID != r.id {
		c.problem(file, "metadata", "run identifier differs from the other evidence")
	}
	if meta.Commit != r.commit {
		c.problem(file, "metadata", "commit does not match --commit")
	}
}

func (r *evidenceRun) readTSV(c *checker, file string) (string, bool) {
	text, ok := c.read(file)
	if !ok {
		return "", false
	}
	line, body, _ := strings.Cut(text, "\n")
	r.check(c, file, []byte(line))
	return body, true
}

func (c *checker) readGoEvents(path string, run *evidenceRun) goEvidence {
	ev := goEvidence{file: path, tests: make(map[goTest]testOutcome), packages: make(map[string]packageOutcome)}
	f, err := os.Open(path)
	if err != nil {
		c.problem(path, "file", "%v", err)
		return ev
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, 4<<20)
	if !scanner.Scan() {
		c.problem(path, "metadata", "missing run metadata")
		return ev
	}
	run.check(c, path, scanner.Bytes())
	n := 1
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
		pkg := ev.packages[event.Package]
		if event.Test == "" {
			switch event.Action {
			case "start":
				if pkg.started {
					c.problem(path, event.Package, "multiple package starts in one evidence file")
				}
				pkg.started = true
			case "pass", "fail", "skip":
				if !pkg.started || pkg.terminal || (event.Action == "pass" && pkg.active != 0) {
					c.problem(path, event.Package, "out-of-order package %s event", event.Action)
					continue
				}
				pkg.terminal = true
				pkg.passed = event.Action == "pass"
				pkg.failed = event.Action == "fail"
				if pkg.failed {
					c.problem(path, event.Package, "package failed")
				}
			case "output":
				// Cached runs replay test events, then replace the duration in the package summary.
				parts := strings.Split(strings.TrimSpace(event.Output), "\t")
				if len(parts) >= 3 && strings.TrimSpace(parts[0]) == "ok" && parts[1] == event.Package {
					if !pkg.started || pkg.terminal || pkg.active != 0 || pkg.summary {
						c.problem(path, event.Package, "out-of-order package summary")
						continue
					}
					duration := strings.TrimSpace(parts[2])
					pkg.cached = duration == "(cached)"
					elapsed, err := time.ParseDuration(duration)
					pkg.summary = pkg.cached || (err == nil && elapsed >= 0 && strings.HasSuffix(duration, "s"))
				}
			}
			ev.packages[event.Package] = pkg
			continue
		}
		if event.Action == "output" {
			continue
		}
		key := goTest{pkg: event.Package, name: event.Test}
		outcome := ev.tests[key]
		if !pkg.started || pkg.terminal || pkg.summary || (event.Action == "run") == outcome.ran || outcome.passed || outcome.skipped || outcome.failed {
			c.problem(path, key, "out-of-order test %s event", event.Action)
			continue
		}
		switch event.Action {
		case "run":
			outcome.ran = true
			pkg.active++
		case "pass", "skip", "fail":
			pkg.active--
			outcome.passed = event.Action == "pass"
			outcome.skipped = event.Action == "skip"
			outcome.failed = event.Action == "fail"
		}
		ev.tests[key] = outcome
		ev.packages[event.Package] = pkg
	}
	if err := scanner.Err(); err != nil {
		c.problem(path, "file", "read Go test events: %v", err)
	}
	return ev
}

func (c *checker) requireGoPass(t goTest, evidence []goEvidence, file string, key any) {
	if len(evidence) == 0 {
		c.problem(file, key, "Go test %s requires --go-json evidence", t)
		return
	}
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
		case !pkg.summary:
			c.problem(ev.file, t, "missing uncached package summary for %s", key)
		}
	}
	if !found {
		c.problem(file, key, "no exact Go test run for %s in --go-json evidence", t)
	}
}

func (c *checker) results(phase string, files evidenceFiles) {
	cumulative := phase == "phase-5"
	c.checkMappings(phase, "", cumulative)
	run := evidenceRun{commit: files.commit}
	var evidence []goEvidence
	for _, path := range files.goJSON {
		evidence = append(evidence, c.readGoEvents(path, &run))
	}
	var conformance map[caseKey]string
	if files.census != "" {
		if text, ok := run.readTSV(c, files.census); ok {
			conformance = c.readCensus(files.census, text, 1)
		}
	}
	launched := make(map[caseKey]bool)
	if files.launches != "" {
		if text, ok := run.readTSV(c, files.launches); ok {
			launched = c.readLaunchEvidence(files.launches, text)
		}
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
			if files.census == "" {
				c.problem(classFile, key, "conformance case requires --census evidence")
			} else if status := conformance[key]; status != "pass" {
				c.problem(classFile, key, "no passing conformance case in --census evidence (status %q)", status)
			}
			if files.launches == "" {
				c.problem(classFile, key, "conformance case requires --launches evidence")
			} else if !launched[key] {
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
	for slug, replacements := range c.diffs {
		for _, replacement := range replacements {
			if phaseMatches(replacement.phase, phase, cumulative) {
				c.requireGoPass(replacement.test, evidence, planFile, slug)
			}
		}
	}
}

func (c *checker) readLaunchEvidence(file, text string) map[caseKey]bool {
	type image struct{ path, digest string }
	var built image
	launched := make(map[caseKey]bool)
	c.parseRows(file, text, caseColumns+" binary binary_sha256", 1, func(n int, row map[string]string) {
		key, ok := c.caseKey(file, n, row)
		if !ok {
			return
		}
		observed := image{row["binary"], row["binary_sha256"]}
		if p := observed.path; !filepath.IsAbs(p) || filepath.Clean(p) != p || filepath.Dir(p) == p || strings.ContainsAny(p, "\r\n\t\x00") {
			c.problem(file, key, "launch needs an absolute, clean Go binary path")
			return
		}
		if !digestPattern.MatchString(observed.digest) {
			c.problem(file, key, "launch needs a SHA-256 binary digest")
			return
		}
		if built.path == "" {
			built = observed
		} else if built != observed {
			c.problem(file, key, "launch binary path or digest differs within the run")
			return
		}
		if _, ok := c.census[key]; !ok && c.census != nil {
			c.problem(file, key, "launch case not in %s", censusFile)
			return
		}
		launched[key] = true
	})
	return launched
}
