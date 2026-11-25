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

// Package argscan reads source command arguments without interpreting them as Cobra flags.
package argscan

import (
	"io"
	"iter"
	"slices"
)

// Args is a read-only view of the tokens following a command name.
type Args struct {
	tokens []string
}

// Scan views the arguments in their original order. The caller must not mutate args while using the view.
func Scan(args []string) Args {
	return Args{tokens: args}
}

// Has reports whether any token is exactly equal to flag, even in a value position or after --.
func (a Args) Has(flag string) bool {
	return slices.Contains(a.tokens, flag)
}

// Len returns the number of raw argument tokens.
func (a Args) Len() int {
	return len(a.tokens)
}

// First returns the first raw argument and whether it exists; a flag-looking token is not skipped.
func (a Args) First() (string, bool) {
	if len(a.tokens) == 0 {
		return "", false
	}
	return a.tokens[0], true
}

// Pair holds an option token and its following value, if one exists.
type Pair struct {
	Option   string
	Value    string
	HasValue bool
}

// Pairs yields consecutive option/value pairs for commands such as pin and context-window.
// It preserves invalid options and repeated pairs for the command to handle, and never splits on '='.
func (a Args) Pairs() iter.Seq[Pair] {
	return func(yield func(Pair) bool) {
		for i := 0; i < len(a.tokens); i += 2 {
			pair := Pair{Option: a.tokens[i], HasValue: i+1 < len(a.tokens)}
			if pair.HasValue {
				pair.Value = a.tokens[i+1]
			}
			if !yield(pair) {
				return
			}
		}
	}
}

// Streams separates ordinary log lines from direct output and diagnostics.
type Streams struct {
	Log    io.Writer
	Output io.Writer
	Error  io.Writer
}

// Streams routes ordinary log lines to stderr when --json appears as an exact token.
// Direct output still goes to stdout, and diagnostics still go to stderr.
func (a Args) Streams(stdout, stderr io.Writer) Streams {
	log := stdout
	if a.Has("--json") {
		log = stderr
	}
	return Streams{Log: log, Output: stdout, Error: stderr}
}
