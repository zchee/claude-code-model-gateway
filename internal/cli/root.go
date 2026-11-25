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
	name := commandName(argv)
	root := newRoot(name)
	var args []string
	var command string
	if len(argv) > 1 {
		args = argv[1:]
		command = args[0]
	}
	// The first token is the command even when it looks like a flag. Cobra's
	// command finder can skip flags, so check the exact token before delegating.
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

func newRoot(name string) *cobra.Command {
	root := &cobra.Command{
		Use:               name,
		Short:             "Manage the Claude Code model gateway",
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.SetErrPrefix(installedName + ":")
	for _, stub := range []struct {
		name, short string
		hidden      bool
	}{
		{name: "setup", short: "Install the proxy binary"},
		{name: "login", short: "Run the ChatGPT OAuth flow"},
		{name: "start", short: "Start the proxy and shim"},
		{name: "stop", short: "Stop the proxy and shim"},
		{name: "ensure", short: "Maintain installed wiring and start unless intentionally stopped"},
		{name: "recover", short: "Restore readiness without settings, pin, or cache maintenance"},
		{name: "readiness-gate", short: "Check readiness for a hook event", hidden: true},
		{name: "status", short: "Show what is running"},
		{name: "models", short: "Show the models advertised to Claude Code"},
		{name: "catalog", short: "Print the sidequest-readable model catalog"},
		{name: "pin", short: "Show or persist model pin overrides"},
		{name: "context-window", short: "Show or persist context windows per backend"},
		{name: "env", short: "Print the Claude Code environment block or change wiring"},
		{name: "doctor", short: "Run a full gateway health check"},
		{name: "remote-control", short: "Manage opt-in remote-control compatibility mode"},
		{name: "serve-shim", short: "Run the router in the foreground (internal)"},
		{name: "serve-worker", short: "Run the worker in the foreground", hidden: true},
	} {
		cmd := &cobra.Command{
			Use:                stub.name,
			Short:              stub.short,
			Hidden:             stub.hidden,
			DisableFlagParsing: true,
			SilenceUsage:       true,
			RunE: func(cmd *cobra.Command, args []string) error {
				// The readiness gate is dispatched before the source reads its argument tokens.
				if cmd.Name() == "readiness-gate" {
					return stubRun(cmd.Name(), argscan.Args{})
				}
				return stubRun(cmd.Name(), argscan.Scan(args))
			},
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(version.NewCommand(), completions.NewCommand())
	return root
}

func stubRun(name string, _ argscan.Args) error {
	return fmt.Errorf("%s: not implemented", name)
}
