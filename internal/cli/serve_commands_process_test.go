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

//go:build process && (darwin || linux)

package cli_test

import (
	"net"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestServeShimReportsABindFailureOnce runs serve-shim to a failure it meets only after its state directory, control
// token, executable and port checks: its public port is held by the test's own loopback listener, so the bind fails.
// The failure reaches stderr once, as one line with one "model-gateway: " prefix, the exit status is 1, and nothing
// reaches stdout.
func TestServeShimReportsABindFailureOnce(t *testing.T) {
	tests := map[string]struct {
		settings func(heldPort string) map[string]string
	}{
		"error: public port held by another listener": {
			settings: func(heldPort string) map[string]string {
				return map[string]string{"CODEX_GATEWAY_PORT": heldPort}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var cfg net.ListenConfig
			held, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			port := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)

			status, stdout, stderr := runServeShim(t, tc.settings(port))
			if diff := gocmp.Diff(1, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("", stdout); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			checkReportedOnce(t, stderr, "127.0.0.1:"+port)
		})
	}
}
