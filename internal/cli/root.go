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

	"github.com/spf13/cobra"

	"github.com/zchee/claude-code-model-gateway/internal/cli/completions"
	"github.com/zchee/claude-code-model-gateway/internal/cli/version"
)

// Run dispatches args (excluding the executable name) using the supplied streams and returns an exit status.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRoot()
	if args == nil {
		// Cobra treats nil as a request to read process arguments.
		args = []string{}
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

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:               "model-gateway",
		Short:             "Manage the Claude Code model gateway",
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
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
			RunE: func(cmd *cobra.Command, _ []string) error {
				return fmt.Errorf("%s: not implemented", cmd.Name())
			},
		}
		cmd.SetErrPrefix(root.Name() + ":")
		root.AddCommand(cmd)
	}
	root.AddCommand(version.NewCommand(), completions.NewCommand())
	return root
}
