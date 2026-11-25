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
			args := []string{tt.command, "--help", "--unknown", "value"}
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
	}{
		"success: bash":       {args: []string{"completions", "bash"}},
		"success: zsh":        {args: []string{"completions", "zsh"}},
		"success: fish":       {args: []string{"completions", "fish"}},
		"success: powershell": {args: []string{"completions", "powershell"}},
		"error: unknown shell": {
			args:       []string{"completions", "unknown"},
			wantStatus: 1,
			wantError:  "Error: unsupported shell \"unknown\"\n",
		},
		"error: missing shell": {
			args:       []string{"completions"},
			wantStatus: 1,
			wantError:  "Error: accepts 1 arg(s), received 0\n",
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
			if tt.wantStatus == 0 {
				if !strings.Contains(stdout.String(), "model-gateway") {
					t.Errorf("stdout does not contain a script for model-gateway: %q", stdout.String())
				}
			} else if diff := gocmp.Diff("", stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	tests := map[string]struct{ args []string }{
		"success: explicit help": {args: []string{"--help"}},
		"success: no arguments":  {},
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
