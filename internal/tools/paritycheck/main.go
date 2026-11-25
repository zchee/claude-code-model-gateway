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

// Command paritycheck validates source case inventories and gate evidence.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

const usage = `usage:
  hack/parity-check [--root DIR] [structure]
  hack/parity-check [--root DIR] mapping <phase> [package]
  hack/parity-check [--root DIR] results <phase> [options]

Results options (provide each file needed by the selected gate):
  --commit SHA         required full commit of the gated checkout
  --go-json PATH       metadata line, then go test -json -count=1; repeatable
  --census PATH        metadata line, then conformance census TSV
  --launches PATH      metadata line, then launch TSV including binary and binary_sha256

Each metadata line is a JSON object with run_id and commit. Evidence paths are
relative to the caller. See docs/port/parity-check.md.`

func main() {
	if len(os.Args) < 3 {
		_, _ = fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2], os.Args[3:], os.Stdout, os.Stderr))
}

func run(root, caller string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 && args[0] == "--root" {
		if len(args) < 2 || args[1] == "" || args[1][0] == '-' {
			_, _ = fmt.Fprintln(stderr, usage)
			return 2
		}
		root, args = args[1], args[2:]
		if !filepath.IsAbs(root) {
			root = filepath.Join(caller, root)
		}
	}
	mode := "structure"
	if len(args) != 0 {
		mode, args = args[0], args[1:]
	}
	phase, pkg, ev := "", "", evidenceFiles{}
	switch mode {
	case "--help", "-h", "help":
		_, _ = fmt.Fprintln(stdout, usage)
		return 0
	case "structure":
		if len(args) != 0 {
			_, _ = fmt.Fprintln(stderr, usage)
			return 2
		}
	case "mapping", "results":
		if len(args) == 0 || !validPhase(args[0]) {
			_, _ = fmt.Fprintln(stderr, usage)
			return 2
		}
		phase, args = args[0], args[1:]
		if mode == "mapping" {
			if len(args) > 1 || len(args) == 1 && !validPackage(args[0]) {
				_, _ = fmt.Fprintln(stderr, usage)
				return 2
			}
			if len(args) == 1 {
				pkg = args[0]
			}
		} else if !ev.parse(caller, args, stderr) {
			return 2
		}
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}

	c := checker{root: root}
	c.structure()
	switch mode {
	case "mapping":
		c.mapping(phase, pkg)
	case "results":
		c.results(phase, ev)
	}
	if len(c.problems) != 0 {
		slices.Sort(c.problems)
		for _, p := range slices.Compact(c.problems) {
			_, _ = fmt.Fprintln(stderr, p)
		}
		return 1
	}
	if _, err := fmt.Fprintln(stdout, "parity-check:", mode, "passed"); err != nil {
		_, _ = fmt.Fprintln(stderr, "parity-check: output:", err)
		return 1
	}
	return 0
}

var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type evidenceFiles struct {
	goJSON                   []string
	census, launches, commit string
}

func (ev *evidenceFiles) parse(caller string, args []string, w io.Writer) bool {
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(caller, path)
	}
	flags := flag.NewFlagSet("results", flag.ContinueOnError)
	flags.SetOutput(w)
	flags.Usage = func() { _, _ = fmt.Fprintln(w, usage) }
	flags.StringVar(&ev.commit, "commit", "", "full commit of the gated checkout")
	flags.Func("go-json", "path to Go test events (repeatable)", func(path string) error {
		if path == "" {
			return fmt.Errorf("empty path")
		}
		ev.goJSON = append(ev.goJSON, resolve(path))
		return nil
	})
	for name, value := range map[string]*string{"census": &ev.census, "launches": &ev.launches} {
		flags.Func(name, "path to evidence TSV", func(path string) error {
			if path == "" || *value != "" {
				return fmt.Errorf("expected one nonempty path")
			}
			*value = resolve(path)
			return nil
		})
	}
	if err := flags.Parse(args); err != nil {
		return false
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return false
	}
	if !commitPattern.MatchString(ev.commit) {
		_, _ = fmt.Fprintln(w, "results: --commit requires a full lowercase Git commit ID")
		return false
	}
	return true
}
