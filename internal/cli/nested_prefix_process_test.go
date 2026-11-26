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
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// TestServeWorkerPrintsAPrefixedReportFailureOnce runs a supervised serve-worker whose generation is too long for its
// listening report. The worker binds its listener, then fails to report it, with an error that wraps the report
// writer's own refusal, which already starts with "model-gateway: ". The failure still reaches stderr as one line that
// carries the prefix once, and the TCP readiness line, which follows the report, never reaches stdout.
func TestServeWorkerPrintsAPrefixedReportFailureOnce(t *testing.T) {
	tests := map[string]struct {
		generation string
	}{
		"error: generation too long for the listening report": {generation: strings.Repeat("g", 300)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// A short home keeps the worker's Unix socket path within the platform's limit, so that no socket warning
			// joins the failure on stderr.
			home, err := os.MkdirTemp("", "mg-gen-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(home); err != nil {
					t.Error(err)
				}
			})
			// The worker takes ownership of the descriptors it is given and closes them; the test keeps the other ends.
			var report, live [2]int
			if err := syscall.Pipe(report[:]); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Pipe(live[:]); err != nil {
				_, _ = syscall.Close(report[0]), syscall.Close(report[1])
				t.Fatal(err)
			}
			reportRead, liveWrite := os.NewFile(uintptr(report[0]), "report reader"), os.NewFile(uintptr(live[1]), "liveness writer")
			defer func() { _, _ = reportRead.Close(), liveWrite.Close() }()

			env := gatewaytest.NewEnv(home, nil, nil, map[string]string{
				"MODEL_GATEWAY_STATE_DIR":         filepath.Join(home, "state"),
				"MODEL_GATEWAY_CONTROL_FDS":       strconv.Itoa(report[1]) + "," + strconv.Itoa(live[0]),
				"MODEL_GATEWAY_WORKER_GENERATION": tc.generation,
			})
			environ := make([]string, 0, len(env))
			for k, v := range env {
				environ = append(environ, k+"="+v)
			}
			var stdout, stderr bytes.Buffer
			status := cli.RunProcess([]string{"model-gateway", "serve-worker"}, strings.NewReader(""), &stdout, &stderr, cli.Process{
				Lookup: func(key string) (string, bool) {
					v, ok := env[key]
					return v, ok
				},
				Environ: func() []string { return environ },
				Home:    func() (string, error) { return home, nil },
				Cwd:     func() (string, error) { return home, nil },
			})
			if diff := gocmp.Diff(1, status); diff != "" {
				t.Errorf("exit status mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(stdout.String(), "listening on 127.0.0.1:") {
				t.Errorf("stdout carries the TCP readiness line of a worker that never reported its port:\n%s", stdout.String())
			}
			checkReportedOnce(t, stderr.String(), "MODEL_GATEWAY_WORKER_GENERATION")
		})
	}
}
