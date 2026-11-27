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

//go:build process && (linux || darwin)

package serveworker_test

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"worker": func(t testing.TB, args []string) {
		mode := args[0]
		// DispatchHelper has consumed its private directory at fd 3. Shift the inherited pipe ends into the worker's slots.
		if mode == "supervised" || mode == "missing" || mode == "no-generation" || mode == "stdin-live" {
			if err := unix.Dup2(4, 3); err != nil {
				t.Fatal(err)
			}
			if err := unix.Close(4); err != nil {
				t.Fatal(err)
			}
			if mode == "supervised" || mode == "no-generation" {
				if err := unix.Dup2(5, 4); err != nil {
					t.Fatal(err)
				}
				if err := unix.Close(5); err != nil {
					t.Fatal(err)
				}
			}
		}
		lookup := os.LookupEnv
		if mode == "unguarded" || mode == "node-fixture" {
			lookup = func(key string) (string, bool) {
				if key == "MODEL_GATEWAY_TEST_SANDBOX" || (key == "NODE_TEST_CONTEXT" && mode == "unguarded") {
					return "", false
				}
				return os.LookupEnv(key)
			}
		}
		opts := serveworker.Options{Lookup: lookup, Home: os.UserHomeDir, Cwd: os.Getwd, Version: "fixture-build", Stdout: os.Stdout, Stderr: os.Stderr}
		if err := serveworker.Run(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(os.Stdout, "worker returned cleanly")
	}, authStatusHelper: authStatusOutput, serveworker.UnwindCensusHelper: func(t testing.TB, _ []string) { serveworker.UnwindCensus(t) }})
}

func pipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}
