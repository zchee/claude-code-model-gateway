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

package cli

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/argscan"
	"github.com/zchee/claude-code-model-gateway/internal/cli/version"
)

const sandboxHome = "sandbox-home"

type testEnv map[string]string

func (e testEnv) lookup(key string) (string, bool) {
	value, ok := e[key]
	return value, ok
}

func testHome() (string, error) {
	return sandboxHome, nil
}

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
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdin, stdout, stderr bytes.Buffer
			args := []string{"claude-code-model-gateway", tt.command, "--help", "--unknown", "value"}
			got := Run(args, &stdin, &stdout, &stderr, testEnv(nil).lookup, testHome)
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

func TestSourceInvocations(t *testing.T) {
	commonArgs := []string{
		"--unknown-before", "enable", "--unknown-after", "--", "--preserve-wiring", "--device",
		"--quiet", "--json", "--refresh=true", "--write-project", "--mode", "--remove=", "--confirm", "--show-mode",
	}
	tests := map[string]struct {
		command  string
		args     []string
		wantScan *commandScan
	}{
		"success: setup": {command: "setup", wantScan: &commandScan{present: map[string]bool{"--preserve-wiring": true}}},
		"success: login": {command: "login", wantScan: &commandScan{present: map[string]bool{"--device": true}}},
		"success: start": {command: "start", wantScan: &commandScan{}},
		"success: stop":  {command: "stop", wantScan: &commandScan{}},
		"success: ensure": {
			command: "ensure", wantScan: &commandScan{present: map[string]bool{"--quiet": true}},
		},
		"success: recover":        {command: "recover", wantScan: &commandScan{}},
		"success: readiness-gate": {command: "readiness-gate"},
		"success: status":         {command: "status", wantScan: &commandScan{}},
		"success: models":         {command: "models", wantScan: &commandScan{}},
		"success: catalog": {
			command: "catalog", wantScan: &commandScan{present: map[string]bool{"--json": true, "--refresh": false}},
		},
		"success: pin": {
			command: "pin", args: []string{"--unknown-before", "start", "--opus", "--json", "--unknown-after", "end", "--fable"},
			wantScan: &commandScan{
				pairOptions: []string{"--opus", "--sonnet", "--fable"},
				pairs: []argscan.Pair{
					{Option: "--unknown-before", Value: "start", HasValue: true},
					{Option: "--opus", Value: "--json", HasValue: true},
					{Option: "--unknown-after", Value: "end", HasValue: true},
					{Option: "--fable"},
				},
			},
		},
		"success: context-window": {
			command: "context-window",
			args:    []string{"--unknown-before", "", "--codex", "--json", "--grok-compact-at", "cap", "--unknown-after", "end", "--claude"},
			wantScan: &commandScan{
				pairOptions: []string{"--claude", "--codex", "--grok", "--codex-compact-at", "--grok-compact-at"},
				pairs: []argscan.Pair{
					{Option: "--unknown-before", HasValue: true},
					{Option: "--codex", Value: "--json", HasValue: true},
					{Option: "--grok-compact-at", Value: "cap", HasValue: true},
					{Option: "--unknown-after", Value: "end", HasValue: true},
					{Option: "--claude"},
				},
			},
		},
		"success: env": {
			command: "env", wantScan: &commandScan{present: map[string]bool{
				"--reconcile": false, "--remove": false, "--write-project": true, "--write-user": false, "--mode": true, "--show-mode": true,
			}},
		},
		"success: doctor":       {command: "doctor", wantScan: &commandScan{}},
		"success: serve-shim":   {command: "serve-shim", wantScan: &commandScan{}},
		"success: serve-worker": {command: "serve-worker", wantScan: &commandScan{}},
	}
	if got := len(sourceCommands()); got != len(tests) {
		t.Fatalf("command count: got %d, want %d", got, len(tests))
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			args := tt.args
			if args == nil {
				args = commonArgs
			}
			const input = `{"hook_event_name":"UserPromptSubmit"}`
			stdin := strings.NewReader(input)
			var stdout, stderr bytes.Buffer
			commands := sourceCommands()
			var calls int
			for i := range commands {
				if commands[i].name != tt.command {
					continue
				}
				commands[i].handle = func(call invocation) error {
					calls++
					if call.name != tt.command {
						t.Errorf("name: got %q, want %q", call.name, tt.command)
					}
					if diff := gocmp.Diff(tt.wantScan, call.scan, gocmp.AllowUnexported(commandScan{})); diff != "" {
						t.Errorf("scan mismatch (-want +got):\n%s", diff)
					}
					if call.stdin != stdin {
						t.Fatal("handler did not receive the input reader")
					}
					if got, err := io.ReadAll(call.stdin); err != nil || string(got) != input {
						t.Errorf("input: got %q, err %v; want %q", got, err, input)
					}
					if got, ok := call.lookup("fixture"); !ok || got != "value" {
						t.Errorf("lookup: got %q, %t; want value, true", got, ok)
					}
					if got, err := call.home(); err != nil || got != sandboxHome {
						t.Errorf("home: got %q, err %v; want %q", got, err, sandboxHome)
					}
					_, _ = io.WriteString(call.streams.Log, "report\n")
					_, _ = io.WriteString(call.streams.Output, "{}\n")
					_, _ = io.WriteString(call.streams.Error, "diagnostic\n")
					return nil
				}
			}
			argv := slices.Concat([]string{"model-gateway", tt.command}, args)
			if status := run(argv, stdin, &stdout, &stderr, testEnv{"fixture": "value"}.lookup, testHome, commands); status != 0 || calls != 1 {
				t.Fatalf("dispatch: got status %d, %d calls; want status 0, one call; stderr %q", status, calls, stderr.String())
			}
			wantOut, wantErr := "{}\n", "report\ndiagnostic\n"
			if tt.wantScan == nil {
				wantOut, wantErr = "report\n{}\n", "diagnostic\n"
			}
			if diff := gocmp.Diff(wantOut, stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantErr, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRoutingUnknownCliOption(t *testing.T) {
	for command, flag := range map[string]string{
		"setup": "--preserve-wiring", "login": "--device", "ensure": "--quiet", "catalog": "--refresh", "env": "--write-project",
	} {
		t.Run(command, func(t *testing.T) {
			scan := func(t *testing.T, args []string) *commandScan {
				t.Helper()
				commands := sourceCommands()
				var got *commandScan
				var calls int
				for i := range commands {
					if commands[i].name == command {
						commands[i].handle = func(call invocation) error {
							calls++
							got = call.scan
							return nil
						}
						break
					}
				}
				argv := slices.Concat([]string{"model-gateway", command}, args)
				var stderr bytes.Buffer
				if status := run(argv, strings.NewReader(""), io.Discard, &stderr, testEnv(nil).lookup, testHome, commands); status != 0 || calls != 1 || got == nil {
					t.Fatalf("dispatch %q: got status %d, %d calls, scan %+v; want status 0, one call and a scan; stderr %q", argv, status, calls, got, stderr.String())
				}
				return got
			}
			tests := map[string]struct {
				args    []string
				control []string
			}{
				"success: unknown flag alone":      {args: []string{"--unknown"}},
				"success: before known flag":       {args: []string{"--unknown", flag}, control: []string{flag}},
				"success: after known flag":        {args: []string{flag, "--unknown"}, control: []string{flag}},
				"success: unknown flag with value": {args: []string{"--unknown=value", flag}, control: []string{flag}},
				"success: after double dash":       {args: []string{"--", "--unknown", flag}, control: []string{"--", flag}},
			}
			for name, tt := range tests {
				t.Run(name, func(t *testing.T) {
					want := scan(t, tt.control)
					if wantKnown := slices.Contains(tt.control, flag); want.present[flag] != wantKnown {
						t.Fatalf("control scan for %q: got %t, want %t", flag, want.present[flag], wantKnown)
					}
					if diff := gocmp.Diff(want, scan(t, tt.args), gocmp.AllowUnexported(commandScan{})); diff != "" {
						t.Errorf("scan mismatch (-control +unknown flag):\n%s", diff)
					}
				})
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
			got := Run(tt.args, &stdin, &stdout, &stderr, testEnv(nil).lookup, testHome)
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

func TestRunCompletionRequests(t *testing.T) {
	tests := map[string]struct {
		command       string
		noDescription bool
	}{
		"success: Cobra completion with optional descriptions": {command: "__complete"},
		"success: Cobra completion without descriptions":       {command: "__completeNoDesc", noDescription: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := []string{"model-gateway", tt.command, "set"}
			if status := Run(args, strings.NewReader(""), &stdout, &stderr, testEnv(nil).lookup, testHome); status != 0 {
				t.Fatalf("completion status: got %d, want 0; stderr %q", status, stderr.String())
			}
			line, directive, _ := strings.Cut(stdout.String(), "\n")
			if choice, _, described := strings.Cut(line, "\t"); choice != "setup" || (tt.noDescription && described) {
				t.Errorf("unexpected completion candidate %q", line)
			}
			if diff := gocmp.Diff(":4\n", directive); diff != "" {
				t.Errorf("completion directive mismatch (-want +got):\n%s", diff)
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
			if diff := gocmp.Diff(1, Run([]string{tt.program, "version", "extra"}, &stdin, &stdout, &stderr, testEnv(nil).lookup, testHome)); diff != "" {
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

// TestRunProcessWithoutLookups runs commands with a zero Process, which supplies none of the process facts. A command
// that needs a missing fact fails with an exit status, never with a panic.
func TestRunProcessWithoutLookups(t *testing.T) {
	tests := map[string]struct {
		args        []string
		wantSuccess bool
		wantStdout  string // Compared only for a success.
	}{
		"error: unknown command": {args: []string{"model-gateway", "no-such-command"}},
		"success: version":       {args: []string{"model-gateway", "version"}, wantSuccess: true, wantStdout: version.String() + "\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// The panic is recovered here so that it fails this case instead of ending the package's test binary.
			status, panicked := func() (status int, panicked any) {
				defer func() { panicked = recover() }()
				return RunProcess(tt.args, strings.NewReader(""), &stdout, &stderr, Process{}), nil
			}()
			if panicked != nil {
				t.Fatalf("RunProcess panicked with a zero Process: %v", panicked)
			}
			if !tt.wantSuccess {
				if status == 0 {
					t.Errorf("exit status 0; want a failure status (stdout %q, stderr %q)", stdout.String(), stderr.String())
				}
				if stdout.Len()+stderr.Len() == 0 {
					t.Error("the command failed without writing anything to stdout or stderr")
				}
				return
			}
			if diff := gocmp.Diff(0, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantStdout, stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInheritedObjectNamesAreUnknownCommands(t *testing.T) {
	type result struct {
		Status         int
		Stdout, Stderr string
	}
	run := func(command string) result {
		var stdin, stdout, stderr bytes.Buffer
		status := Run([]string{"model-gateway", command}, &stdin, &stdout, &stderr, testEnv(nil).lookup, testHome)
		return result{Status: status, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	// bin/model-gateway.js:4-10,13-16 and lib/commands.js:2935-3041:
	// the JavaScript object inherits these names; Go recognizes only registered commands.
	want := run("not-a-command")
	if want.Status != 1 || want.Stderr != "" || !strings.HasPrefix(want.Stdout, "usage: model-gateway <command>\n") {
		t.Fatalf("unknown command returned status %d, stdout %q, stderr %q", want.Status, want.Stdout, want.Stderr)
	}
	tests := map[string]struct{ command string }{
		"error: inherited toString method":       {command: "toString"},
		"error: inherited prototype object":      {command: "__proto__"},
		"error: inherited constructor":           {command: "constructor"},
		"error: inherited hasOwnProperty method": {command: "hasOwnProperty"},
		"error: inherited valueOf method":        {command: "valueOf"},
		"error: inherited getter helper":         {command: "__defineGetter__"},
		"error: inherited setter helper":         {command: "__defineSetter__"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(want, run(tt.command)); diff != "" {
				t.Errorf("unknown command mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRemoteControlIsUnknownCommand(t *testing.T) {
	type result struct {
		Status         int
		Stdout, Stderr string
	}
	run := func(command string, args []string) result {
		var stdout, stderr bytes.Buffer
		argv := slices.Concat([]string{"model-gateway", command}, args)
		status := Run(argv, strings.NewReader(""), &stdout, &stderr, testEnv(nil).lookup, testHome)
		return result{Status: status, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	tests := map[string]struct{ args []string }{
		"error: no arguments":   {},
		"error: with arguments": {args: []string{"enable", "--confirm"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := run("not-a-command", tt.args)
			if want.Status != 1 || want.Stderr != "" || !strings.HasPrefix(want.Stdout, "usage: model-gateway <command>\n") {
				t.Fatalf("unknown command returned status %d, stdout %q, stderr %q", want.Status, want.Stdout, want.Stderr)
			}
			if diff := gocmp.Diff(want, run("remote-control", tt.args)); diff != "" {
				t.Errorf("unknown command mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	tests := map[string]struct {
		args       []string
		name       string
		env        testEnv
		homeErr    error
		forbidHome bool
		wantStatus int
		wantStderr string
	}{
		// bin/model-gateway.js:4-10 and lib/runtime.js:16-18: no command, default state paths.
		"success: empty argument vector": {name: "model-gateway"},
		// lib/commands.js:127,153-197 and lib/runtime.js:16-18: unset paths use defaults.
		"success: executable with unset path overrides": {args: []string{"model-gateway"}, name: "model-gateway"},
		// lib/commands.js:127,153-197 and lib/runtime.js:16-18: empty paths also use defaults.
		"success: executable with empty path overrides": {
			args: []string{"model-gateway"}, name: "model-gateway",
			env: testEnv{"MODEL_GATEWAY_STATE_DIR": "", "CODEX_GATEWAY_REQUEST_LOG_PATH": "", "CLAUDE_CONFIG_DIR": "separate-config"},
		},
		"success: empty command before known command": {
			args: []string{"model-gateway", "", "version"}, name: "model-gateway",
		},
		// bin/model-gateway.js:4-10 and lib/commands.js:2935-3041: help spellings are not commands.
		"error: help word": {
			args: []string{"model-gateway", "help", "setup"}, name: "model-gateway", wantStatus: 1,
		},
		"error: long help switch with go install name": {
			args: []string{"claude-code-model-gateway", "--help"}, name: "claude-code-model-gateway", wantStatus: 1,
		},
		"error: short help switch": {
			args: []string{"model-gateway", "-h"}, name: "model-gateway", wantStatus: 1,
		},
		// bin/model-gateway.js:4-10 and lib/commands.js:2935-3041: these are not command names.
		"error: top-level version switch": {
			args: []string{"model-gateway", "--version"}, name: "model-gateway", wantStatus: 1,
		},
		"error: top-level unknown short switch": {
			args: []string{"model-gateway", "-x"}, name: "model-gateway", wantStatus: 1,
		},
		"error: similar to Cobra completion request": {
			args: []string{"model-gateway", "__complete-extra", "set"}, name: "model-gateway", wantStatus: 1,
		},
		"error: similar to Cobra completion request without descriptions": {
			args: []string{"model-gateway", "__completeNoDesc-extra", "set"}, name: "model-gateway", wantStatus: 1,
		},
		"error: arbitrary double underscore command": {
			args: []string{"model-gateway", "__unknown", "set"}, name: "model-gateway", wantStatus: 1,
		},
		// bin/model-gateway.js:4-10 and lib/commands.js:199-201: a leading flag or -- is not skipped.
		"error: flag before command": {
			args: []string{"model-gateway", "--unknown=value", "version"}, name: "model-gateway", wantStatus: 1,
		},
		"error: separator before command": {
			args: []string{"model-gateway", "--", "version"}, name: "model-gateway", wantStatus: 1,
		},
		// bin/model-gateway.js:4-10 and lib/commands.js:127,153-197,215-218: usage bypasses the log writer.
		"error: unknown command with json and path override": {
			args: []string{"model-gateway", "statu", "--json"}, name: "model-gateway", wantStatus: 1,
			env: testEnv{"CODEX_GATEWAY_REQUEST_LOG_PATH": "relative request log.jsonl"},
		},
		// lib/commands.js:127,153-197: paths in usage follow the active state directory.
		"success: overridden state does not need home": {
			args: []string{"model-gateway"}, name: "model-gateway", env: testEnv{"MODEL_GATEWAY_STATE_DIR": "other-state"},
			forbidHome: true,
		},
		// lib/runtime.js:16 and bin/model-gateway.js:13-16: usage cannot resolve default paths on a home lookup failure.
		"error: home lookup failure": {
			args: []string{"model-gateway"}, name: "model-gateway", homeErr: errors.New("lookup unavailable"), wantStatus: 1,
			wantStderr: "model-gateway: cannot determine the home directory: lookup unavailable\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stateDir := tt.env["MODEL_GATEWAY_STATE_DIR"]
			if stateDir == "" {
				stateDir = filepath.Join(sandboxHome, ".claude", "model-gateway")
			}
			requestLog := tt.env["CODEX_GATEWAY_REQUEST_LOG_PATH"]
			if requestLog == "" {
				requestLog = filepath.Join(stateDir, "logs", "request-routes.jsonl")
			}
			wantStdout := strings.NewReplacer(
				"<program>", tt.name,
				"<bin>", filepath.Join(stateDir, "bin"),
				"<logs>", filepath.Join(stateDir, "logs"),
				"<catalog>", filepath.Join(stateDir, "catalog.json"),
				"<pins>", filepath.Join(stateDir, "pins.json"),
				"<context>", filepath.Join(stateDir, "context-window.json"),
				"<request-log>", requestLog,
			).Replace(sourceUsage)
			if tt.wantStderr != "" {
				wantStdout = ""
			}
			home := func() (string, error) {
				if tt.forbidHome {
					t.Fatal("home lookup called despite explicit state directory")
				}
				return sandboxHome, tt.homeErr
			}
			var stdin, stdout, stderr bytes.Buffer
			if diff := gocmp.Diff(tt.wantStatus, Run(tt.args, &stdin, &stdout, &stderr, tt.env.lookup, home)); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantStdout, stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantStderr, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Adapted from lib/commands.js:153-197, with the newline from bin/model-gateway.js:8.
// The command list includes only commands carried over to Go.
const sourceUsage = `usage: <program> <command>

  setup            download the claude-code-proxy binary (v-latest) into <bin>
  login [--device] run the ChatGPT OAuth flow (--device for headless device-code)
  start | stop     start/stop the proxy + shim (detached, logs in <logs>)
  ensure [--quiet] maintain installed wiring and start unless intentionally stopped
  recover          restore readiness without settings, pin or cache maintenance
  status           show what's running
  models           show the model list the shim advertises to Claude Code
  catalog [--json] [--refresh] print the sidequest-readable model catalog (<catalog>);
                   --refresh exits non-zero and says why on stderr when it could not write a
                   fresh one, and prints the retained catalog unchanged
  pin [--opus|--sonnet|--fable <model|default>]
                   show pin sources and newest catalog ids, or persist an override (<pins>);
                   default removes the override and follows detected/catalog defaults, never backwards
  context-window [--claude <tokens|full>] [--codex <tokens|full>] [--grok <tokens|full>]
                   show or persist the context window per backend (<context>); the
                   orchestrator and every executor share it. Defaults: claude full, codex 272000,
                   grok full. Without compact-at, a gateway cap compacts 85000 below the cap.
                   --codex-compact-at <tokens|cap> and --grok-compact-at <tokens|cap> set a direct
                   maximum before compaction (positive whole tokens); cap removes the override.
                   The cap and backend window minus 40000 still limit the effective maximum.
                   A saved compact-at ignores CODEX_GATEWAY_COMPACT_TRIGGER, reported unchanged.
                   Example: --codex-compact-at 242000 leaves the 272000 cap unchanged. OpenAI bills
                   input above 272k tokens at 2x; the crossing turn and compaction request can still
                   exceed 272k. Existing installations keep their current policy until configured.
                   --claude sets the native autoCompactWindow in project-wired settings; native
                   engine headroom applies and the exact trigger is unverified. Claude compact-at
                   is unsupported. No same-model main/subagent split is applied.
                   CODEX_GATEWAY_CONTEXT_WINDOW is superseded and only applies when no codex value is saved.
  env [--write-project | --write-user | --remove] [--reconcile]
                   print the Claude Code env block, or merge/remove wiring
                   --remove clears owned routing, discovery, static env and alias pins; preserves and names user pins
                   (--write-project writes .claude/settings.local.json; --write-user
                   is an opt-in shared fallback in ~/.claude/settings.json)
  doctor           full health check, Anthropic catalog age/fallback, pin sources and newest ids
  serve-shim       (internal) run the router in the foreground

  Request route logging (on by default; set CODEX_GATEWAY_REQUEST_LOG=0 to opt out):
    CODEX_GATEWAY_REQUEST_LOG=0
    Writes JSONL route metadata to <request-log>. Override the path with
    CODEX_GATEWAY_REQUEST_LOG_PATH. It records no request bodies, prompts, tools, or auth.
`
