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
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
)

// TestServeCommandsPrintAPrefixedCauseOnce runs serve-shim and serve-worker against a state directory whose control
// token file holds no valid token. Both commands refuse to start, before they bind a listener or start a worker, with
// an error that wraps the token reader's own refusal, which already starts with "model-gateway: ". The refusal still
// reaches stderr as one line that carries the prefix once.
func TestServeCommandsPrintAPrefixedCauseOnce(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T, state string) (status int, stdout, stderr string)
	}{
		"error: serve-shim refuses an invalid control token": {
			run: func(t *testing.T, state string) (int, string, string) {
				return runServeShim(t, map[string]string{"MODEL_GATEWAY_STATE_DIR": state})
			},
		},
		"error: serve-worker refuses an invalid control token": {
			run: func(t *testing.T, state string) (int, string, string) {
				home := t.TempDir()
				// The state directory is in both environments the command could read, so that no wiring of the command
				// reads a token elsewhere.
				t.Setenv("MODEL_GATEWAY_STATE_DIR", state)
				env := map[string]string{"HOME": home, "MODEL_GATEWAY_STATE_DIR": state}
				lookup := func(key string) (string, bool) {
					v, ok := env[key]
					return v, ok
				}
				var stdout, stderr bytes.Buffer
				status := cli.Run([]string{"model-gateway", "serve-worker"}, strings.NewReader(""), &stdout, &stderr, lookup, func() (string, error) { return home, nil })
				return status, stdout.String(), stderr.String()
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			const invalid = "not-a-control-token\n"
			token := filepath.Join(state, "control-token")
			if err := os.WriteFile(token, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			status, stdout, stderr := tc.run(t, state)
			if diff := gocmp.Diff(1, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			checkReportedOnce(t, stderr, "invalid local control token")
			// The refusal leaves the token file as it found it.
			got, err := os.ReadFile(token)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(invalid, string(got)); diff != "" {
				t.Errorf("control token file mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
