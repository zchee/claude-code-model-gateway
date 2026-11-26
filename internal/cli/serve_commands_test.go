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

//go:build darwin || linux

package cli_test

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
)

// TestServeCommandsReachTheirImplementations runs serve-shim and serve-worker with a setting that only the command's
// own implementation refuses, before it binds a listener, starts a child or touches the filesystem. The exit status
// and the refusal show that the command is wired to its implementation.
func TestServeCommandsReachTheirImplementations(t *testing.T) {
	tests := map[string]struct {
		command        string
		setting, value string
		want           string
	}{
		"error: serve-shim refuses a relative state directory": {
			command: "serve-shim", setting: "MODEL_GATEWAY_STATE_DIR", value: "relative/state",
			want: `model-gateway: the gateway state directory "relative/state" must be a clean absolute path`,
		},
		"error: serve-worker refuses a malformed descriptor setting": {
			command: "serve-worker", setting: "MODEL_GATEWAY_CONTROL_FDS", value: "3",
			want: "model-gateway: MODEL_GATEWAY_CONTROL_FDS must name the report and liveness descriptors as two numbers, such as 3,4",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			// The refusing setting is in both environments the command could read, so that no wiring of the command
			// reaches a listener, a child or the state directory.
			t.Setenv(tc.setting, tc.value)
			env := map[string]string{tc.setting: tc.value, "HOME": home}
			lookup := func(key string) (string, bool) {
				v, ok := env[key]
				return v, ok
			}
			var stdout, stderr bytes.Buffer
			got := cli.Run([]string{"model-gateway", tc.command}, strings.NewReader(""), &stdout, &stderr, lookup, func() (string, error) { return home, nil })
			if diff := gocmp.Diff(1, got); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			// The refusal is printed once, as one line with one "model-gateway: " prefix.
			if diff := gocmp.Diff(tc.want+"\n", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the refused command left %d entries in its home directory", len(entries))
			}
		})
	}
}

// TestServeShimReportsARefusalOnce runs serve-shim past its state-directory check, with a clean absolute state
// directory and an absolute worker executable, to a port setting it refuses before it binds a listener or starts a
// worker. The refusal reaches stderr once, as one line with one "model-gateway: " prefix.
func TestServeShimReportsARefusalOnce(t *testing.T) {
	tests := map[string]struct {
		settings map[string]string
		fragment string
	}{
		"error: worker port equal to the public port": {
			// A privileged port: a wiring that binds before it checks the ports fails here instead of serving.
			settings: map[string]string{"CODEX_GATEWAY_PORT": "1", "CODEX_GATEWAY_WORKER_PORT": "1"},
			fragment: "CODEX_GATEWAY_WORKER_PORT 1 is the public port",
		},
		"error: public port out of range": {
			settings: map[string]string{"CODEX_GATEWAY_PORT": "70000"},
			fragment: "CODEX_GATEWAY_PORT is not a valid port",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			status, stdout, stderr := runServeShim(t, tc.settings)
			if diff := gocmp.Diff(1, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			checkReportedOnce(t, stderr, tc.fragment)
		})
	}
}

// runServeShim runs serve-shim through cli.RunProcess with a fresh home and state directory under t.TempDir and the
// given settings, as both its environment list and its lookup. The worker executable is an absolute path to a file
// that does not exist, so the command gets past its executable check but could never start a worker.
func runServeShim(t *testing.T, settings map[string]string) (status int, stdout, stderr string) {
	t.Helper()
	home := t.TempDir()
	env := map[string]string{"HOME": home, "MODEL_GATEWAY_STATE_DIR": t.TempDir()}
	maps.Copy(env, settings)
	environ := make([]string, 0, len(env))
	for name, value := range env {
		environ = append(environ, name+"="+value)
	}
	executable := filepath.Join(t.TempDir(), "model-gateway")
	var out, errOut bytes.Buffer
	status = cli.RunProcess([]string{"model-gateway", "serve-shim"}, strings.NewReader(""), &out, &errOut, cli.Process{
		Lookup: func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		},
		Environ:    func() []string { return environ },
		Home:       func() (string, error) { return home, nil },
		Cwd:        func() (string, error) { return home, nil },
		Executable: func() (string, error) { return executable, nil },
	})
	return status, out.String(), errOut.String()
}

// checkReportedOnce checks that stderr holds exactly one line, ended by a newline, that starts with exactly one
// "model-gateway: " prefix, names nothing else as model-gateway, and carries fragment.
func checkReportedOnce(t *testing.T, stderr, fragment string) {
	t.Helper()
	const prefix = "model-gateway: "
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	switch {
	case !strings.HasSuffix(stderr, "\n") || len(lines) != 1:
		t.Errorf("stderr holds %d line(s); want the failure printed once as exactly one line:\n%s", len(lines), stderr)
	case !strings.HasPrefix(lines[0], prefix):
		t.Errorf("stderr line %q does not start with %q", lines[0], prefix)
	case strings.Count(lines[0], "model-gateway:") != 1:
		t.Errorf("stderr line %q carries the %q prefix more than once", lines[0], prefix)
	case !strings.Contains(lines[0], fragment):
		t.Errorf("stderr line %q does not carry the failure %q", lines[0], fragment)
	}
}
