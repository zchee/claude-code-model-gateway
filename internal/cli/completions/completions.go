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

// Package completions implements the model-gateway completions command.
package completions

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand returns a command that writes a completion script for the requested shell.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "completions <shell>",
		Short:        "Generate shell completions",
		Args:         cobra.ExactArgs(1),
		ValidArgs:    []string{"bash", "zsh", "fish", "powershell"},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, out := cmd.Root(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}
