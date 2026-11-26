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
	"os"
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
			// The command line may put its own "model-gateway:" in front of the implementation's message.
			line, ok := strings.CutSuffix(stderr.String(), "\n")
			if !ok || strings.Contains(line, "\n") || (line != tc.want && line != "model-gateway: "+tc.want) {
				t.Errorf("stderr %q; want one line carrying the implementation's refusal %q", stderr.String(), tc.want)
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
