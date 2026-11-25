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

package cli_test

import (
	"bytes"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
)

func TestRunStubs(t *testing.T) {
	tests := map[string]struct{ command string }{
		"error: setup":          {command: "setup"},
		"error: login":          {command: "login"},
		"error: start":          {command: "start"},
		"error: stop":           {command: "stop"},
		"error: ensure":         {command: "ensure"},
		"error: recover":        {command: "recover"},
		"error: readiness-gate": {command: "readiness-gate"},
		"error: status":         {command: "status"},
		"error: models":         {command: "models"},
		"error: catalog":        {command: "catalog"},
		"error: pin":            {command: "pin"},
		"error: context-window": {command: "context-window"},
		"error: env":            {command: "env"},
		"error: doctor":         {command: "doctor"},
		"error: remote-control": {command: "remote-control"},
		"error: serve-shim":     {command: "serve-shim"},
		"error: serve-worker":   {command: "serve-worker"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdin, stdout, stderr bytes.Buffer
			args := []string{"claude-code-model-gateway", tt.command, "--help", "--unknown", "value"}
			got := cli.Run(args, &stdin, &stdout, &stderr)
			if diff := gocmp.Diff(1, got); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("model-gateway: "+tt.command+": not implemented\n", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunCompletions(t *testing.T) {
	tests := map[string]struct {
		args       []string
		wantStatus int
		wantError  string
		wantLine   string
	}{
		"success: bash for stable name": {
			args:     []string{"model-gateway", "completions", "bash"},
			wantLine: "    complete -o default -F __start_model-gateway model-gateway",
		},
		"success: bash for go install name": {
			args:     []string{"claude-code-model-gateway", "completions", "bash"},
			wantLine: "    complete -o default -F __start_claude-code-model-gateway claude-code-model-gateway",
		},
		"success: zsh for stable name": {
			args:     []string{"model-gateway", "completions", "zsh"},
			wantLine: "compdef _model-gateway model-gateway",
		},
		"success: zsh for go install name": {
			args:     []string{"claude-code-model-gateway", "completions", "zsh"},
			wantLine: "compdef _claude-code-model-gateway claude-code-model-gateway",
		},
		"success: fish for stable name": {
			args:     []string{"model-gateway", "completions", "fish"},
			wantLine: `complete -c model-gateway -n 'not __model_gateway_requires_order_preservation && __model_gateway_prepare_completions' -f -a '$__model_gateway_comp_results'`,
		},
		"success: fish for go install name": {
			args:     []string{"claude-code-model-gateway", "completions", "fish"},
			wantLine: `complete -c claude-code-model-gateway -n 'not __claude_code_model_gateway_requires_order_preservation && __claude_code_model_gateway_prepare_completions' -f -a '$__claude_code_model_gateway_comp_results'`,
		},
		"success: powershell for stable name": {
			args:     []string{"model-gateway", "completions", "powershell"},
			wantLine: `Register-ArgumentCompleter -CommandName 'model-gateway' -ScriptBlock ${__model_gatewayCompleterBlock}`,
		},
		"success: powershell for go install name": {
			args:     []string{"claude-code-model-gateway", "completions", "powershell"},
			wantLine: `Register-ArgumentCompleter -CommandName 'claude-code-model-gateway' -ScriptBlock ${__claude_code_model_gatewayCompleterBlock}`,
		},
		"error: unknown shell": {
			args:       []string{"model-gateway", "completions", "unknown"},
			wantStatus: 1,
			wantError:  "model-gateway: unsupported shell \"unknown\"\n",
		},
		"error: missing shell": {
			args:       []string{"claude-code-model-gateway", "completions"},
			wantStatus: 1,
			wantError:  "model-gateway: accepts 1 arg(s), received 0\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdin, stdout, stderr bytes.Buffer
			got := cli.Run(tt.args, &stdin, &stdout, &stderr)
			if diff := gocmp.Diff(tt.wantStatus, got); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantError, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
			if tt.wantLine != "" {
				for line := range strings.SplitSeq(stdout.String(), "\n") {
					if line == tt.wantLine {
						return
					}
				}
				t.Errorf("stdout does not contain the registration line %q", tt.wantLine)
			} else if diff := gocmp.Diff("", stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	tests := map[string]struct {
		program   string
		wantError string
	}{
		"error: extra argument with stable name": {
			program:   "model-gateway",
			wantError: "model-gateway: unknown command \"extra\" for \"model-gateway version\"\n",
		},
		"error: extra argument with go install name": {
			program:   "claude-code-model-gateway",
			wantError: "model-gateway: unknown command \"extra\" for \"claude-code-model-gateway version\"\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdin, stdout, stderr bytes.Buffer
			if diff := gocmp.Diff(1, cli.Run([]string{tt.program, "version", "extra"}, &stdin, &stdout, &stderr)); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantError, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	tests := map[string]struct {
		args []string
		name string
	}{
		"success: explicit help for go install name": {
			args: []string{"claude-code-model-gateway", "--help"},
			name: "claude-code-model-gateway",
		},
		"success: executable with no command": {
			args: []string{"model-gateway"},
			name: "model-gateway",
		},
		"success: empty argument vector": {name: "model-gateway"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdin, stdout, stderr bytes.Buffer
			if diff := gocmp.Diff(0, cli.Run(tt.args, &stdin, &stdout, &stderr)); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
			help := stdout.String()
			if !strings.Contains(help, "\n  "+tt.name+" [command]\n") {
				t.Errorf("help does not show the invoked name %q", tt.name)
			}
			for _, command := range []string{"completions", "serve-shim"} {
				if !strings.Contains(help, "\n  "+command+" ") {
					t.Errorf("help does not list %q", command)
				}
			}
			for _, command := range []string{"completion", "readiness-gate", "serve-worker"} {
				if strings.Contains(help, "\n  "+command+" ") {
					t.Errorf("help lists %q, which should not be visible", command)
				}
			}
		})
	}
}
