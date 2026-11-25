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

// Package cli registers and runs the model-gateway commands.
// Its Go-only __complete and __completeNoDesc commands are Cobra's shell
// completion protocol. Other unknown commands print the source usage text.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zchee/claude-code-model-gateway/internal/cli/argscan"
	"github.com/zchee/claude-code-model-gateway/internal/cli/completions"
	"github.com/zchee/claude-code-model-gateway/internal/cli/version"
)

// Run dispatches argv, including the executable name, and returns an exit status.
// It uses lookup and home to resolve paths without reading the process environment.
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer, lookup func(string) (string, bool), home func() (string, error)) int {
	return run(argv, stdin, stdout, stderr, lookup, home, sourceCommands())
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer, lookup func(string) (string, bool), home func() (string, error), commands []sourceCommand) int {
	name := commandName(argv)
	root := newRoot(name, commands, lookup, home)
	var args []string
	var command string
	if len(argv) > 1 {
		args = argv[1:]
		command = args[0]
	}
	// The first token is the command even when it looks like a flag. Cobra's
	// command finder can skip flags, so check the exact token before delegating.
	// Go-only exceptions: Cobra's generated scripts use __complete and
	// __completeNoDesc. Cobra's completion code reads the live environment and
	// may write to the process's stderr, outside the injected lookups and writers.
	if command != cobra.ShellCompRequestCmd && command != cobra.ShellCompNoDescRequestCmd &&
		!slices.ContainsFunc(root.Commands(), func(cmd *cobra.Command) bool { return cmd.Name() == command }) {
		state, ok := lookup("MODEL_GATEWAY_STATE_DIR")
		if !ok || state == "" {
			dir, err := home()
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "model-gateway: cannot determine the home directory: %v\n", err)
				return 1
			}
			state = filepath.Join(dir, ".claude", installedName)
		}
		requestLog, ok := lookup("CODEX_GATEWAY_REQUEST_LOG_PATH")
		if !ok {
			requestLog = ""
		}
		_, _ = fmt.Fprintln(stdout, argscan.Usage(name, state, requestLog))
		if command != "" {
			return 1
		}
		return 0
	}
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		return 1
	}
	return 0
}

const installedName = "model-gateway"

// commandName keeps help and shell completions aligned with either installed binary name.
func commandName(argv []string) string {
	if len(argv) > 0 {
		name := strings.TrimSuffix(filepath.Base(argv[0]), ".exe")
		if name == "claude-code-model-gateway" {
			return name
		}
	}
	return installedName
}

type scanMode uint8

const (
	scanNone scanMode = iota
	scanPresence
	scanPairs
	scanFirst
	scanStdin
)

type sourceCommand struct {
	name, short string
	hidden      bool
	mode        scanMode
	flags       []string // Exact presence tokens, or option names in pair mode.
	handle      func(invocation) error
}

type invocation struct {
	name    string
	scan    *commandScan // Nil for the early stdin-only dispatch.
	streams argscan.Streams
	stdin   io.Reader
	lookup  func(string) (string, bool)
	home    func() (string, error)
}

type commandScan struct {
	present     map[string]bool
	pairOptions []string
	pairs       []argscan.Pair
	first       string
	hasFirst    bool
}

func sourceCommands() []sourceCommand {
	return []sourceCommand{
		// lib/commands.js:2936.
		{name: "setup", short: "Install the proxy binary", mode: scanPresence, flags: []string{"--preserve-wiring"}, handle: stubRun},
		// lib/commands.js:2937-2943.
		{name: "login", short: "Run the ChatGPT OAuth flow", mode: scanPresence, flags: []string{"--device"}, handle: stubRun},
		// lib/commands.js:2944-2948.
		{name: "start", short: "Start the proxy and shim", mode: scanNone, handle: stubRun},
		// lib/commands.js:2951-2960.
		{name: "stop", short: "Stop the proxy and shim", mode: scanNone, handle: stubRun},
		// lib/commands.js:2961-2964.
		{name: "ensure", short: "Maintain installed wiring and start unless intentionally stopped", mode: scanPresence, flags: []string{"--quiet"}, handle: stubRun},
		// lib/commands.js:633-638,2949.
		{name: "recover", short: "Restore readiness without settings, pin, or cache maintenance", mode: scanNone, handle: stubRun},
		// bin/model-gateway.js:4-6 and lib/readiness-gate.js:52-63.
		{name: "readiness-gate", short: "Check readiness for a hook event", hidden: true, mode: scanStdin, handle: stubRun},
		// lib/commands.js:955-968,3027.
		{name: "status", short: "Show what is running", mode: scanNone, handle: stubRun},
		// lib/commands.js:3028-3032.
		{name: "models", short: "Show the models advertised to Claude Code", mode: scanNone, handle: stubRun},
		// lib/commands.js:2078-2080,2097-2100.
		{name: "catalog", short: "Print the sidequest-readable model catalog", mode: scanPresence, flags: []string{"--json", "--refresh"}, handle: stubRun},
		// lib/commands.js:1075-1103.
		{name: "pin", short: "Show or persist model pin overrides", mode: scanPairs, flags: []string{"--opus", "--sonnet", "--fable"}, handle: stubRun},
		// lib/commands.js:1001-1012 and lib/context-window.js:20-29.
		{
			name: "context-window", short: "Show or persist context windows per backend", mode: scanPairs,
			flags: []string{"--claude", "--codex", "--grok", "--codex-compact-at", "--grok-compact-at"}, handle: stubRun,
		},
		// lib/commands.js:1134-1146.
		{
			name: "env", short: "Print the Claude Code environment block or change wiring", mode: scanPresence,
			flags: []string{"--reconcile", "--remove", "--write-project", "--write-user", "--mode", "--show-mode"}, handle: stubRun,
		},
		// bin/model-gateway.js:7 and lib/commands.js:1397,3037.
		{name: "doctor", short: "Run a full gateway health check", mode: scanNone, handle: stubRun},
		// lib/remote-control.js:137-145,194-198.
		{name: "remote-control", short: "Manage opt-in remote-control compatibility mode", mode: scanFirst, flags: []string{"--confirm"}, handle: stubRun},
		// lib/commands.js:2456-2905,3039.
		{name: "serve-shim", short: "Run the router in the foreground (internal)", mode: scanNone, handle: stubRun},
		// lib/commands.js:3040 and lib/request-worker.js:1208 onward.
		{name: "serve-worker", short: "Run the worker in the foreground", hidden: true, mode: scanNone, handle: stubRun},
	}
}

func newRoot(name string, commands []sourceCommand, lookup func(string) (string, bool), home func() (string, error)) *cobra.Command {
	root := &cobra.Command{
		Use:               name,
		Short:             "Manage the Claude Code model gateway",
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetErrPrefix(installedName + ":")
	for _, spec := range commands {
		cmd := &cobra.Command{
			Use:                spec.name,
			Short:              spec.short,
			Hidden:             spec.hidden,
			DisableFlagParsing: true,
			SilenceUsage:       true,
			RunE: func(cmd *cobra.Command, args []string) error {
				stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
				call := invocation{name: spec.name, stdin: cmd.InOrStdin(), lookup: lookup, home: home}
				if spec.mode == scanStdin {
					call.streams = argscan.Streams{Log: stdout, Output: stdout, Error: stderr}
					return spec.handle(call)
				}
				scan := argscan.Scan(args)
				call.streams = scan.Streams(stdout, stderr)
				call.scan = &commandScan{}
				if spec.mode == scanPresence || spec.mode == scanFirst {
					call.scan.present = make(map[string]bool, len(spec.flags))
					for _, flag := range spec.flags {
						call.scan.present[flag] = scan.Has(flag)
					}
				}
				if spec.mode == scanFirst {
					call.scan.first, call.scan.hasFirst = scan.First()
				}
				if spec.mode == scanPairs {
					// Keep unrecognized option tokens so that handlers can reject them.
					call.scan.pairOptions = spec.flags
					call.scan.pairs = slices.Collect(scan.Pairs())
				}
				return spec.handle(call)
			},
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(version.NewCommand(), completions.NewCommand())
	return root
}

func stubRun(call invocation) error {
	return fmt.Errorf("%s: not implemented", call.name)
}
