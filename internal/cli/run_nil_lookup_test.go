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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
	"github.com/zchee/claude-code-model-gateway/internal/cli/version"
)

// TestRunWithoutLookup runs commands through cli.Run with a nil environment lookup, which stands for an environment
// with no variables. No command panics: one that cannot start fails with exit status 1 and one diagnostic line.
func TestRunWithoutLookup(t *testing.T) {
	tests := map[string]struct {
		command    string
		wantStatus int
		// wantStdout is compared exactly; a nil value only requires stdout to be something other than empty.
		wantStdout *string
		failure    bool
	}{
		"error: serve-shim without a worker executable": {
			// cli.Run supplies no executable, so the supervisor refuses to start; the state directory it may create lies
			// under the temporary home.
			command: "serve-shim", wantStatus: 1, wantStdout: new(""), failure: true,
		},
		"error: unknown command prints the usage text": {command: "no-such-command", wantStatus: 1},
		"success: version": {command: "version", wantStdout: new(version.String() + "\n")},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			var stdout, stderr bytes.Buffer
			// The panic is recovered here so that it fails this case instead of ending the package's test binary.
			status, panicked := func() (status int, panicked any) {
				defer func() { panicked = recover() }()
				return cli.Run([]string{"model-gateway", tc.command}, strings.NewReader(""), &stdout, &stderr, nil, func() (string, error) { return home, nil }), nil
			}()
			if panicked != nil {
				t.Fatalf("cli.Run panicked with a nil lookup: %v", panicked)
			}
			if diff := gocmp.Diff(tc.wantStatus, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			switch {
			case tc.wantStdout != nil:
				if diff := gocmp.Diff(*tc.wantStdout, stdout.String()); diff != "" {
					t.Errorf("stdout mismatch (-want +got):\n%s", diff)
				}
			case stdout.Len() == 0:
				t.Error("stdout is empty; want the usage text")
			}
			if tc.failure {
				checkReportedOnce(t, stderr.String(), "")
				return
			}
			if diff := gocmp.Diff("", stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
