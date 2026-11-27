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

package serveworker_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	gocmp "github.com/google/go-cmp/cmp"
)

const (
	// authStatusHelper is the fixture helper that stands in for the proxy binary's auth status command.
	authStatusHelper = "auth-status-output"
	// maxStatusOutput is the per-stream output limit of the source's auth status call: execFile's
	// default maxBuffer, which the pinned call does not override (lib/request-worker.js:49-53).
	maxStatusOutput = 1 << 20
	// statusHoldLimit ends a holding helper that nothing stopped, so no fixture outlives its test by
	// much; the worker's own limit for the command is shorter.
	statusHoldLimit = 30 * time.Second
)

// authStatusOutput is the helper behind authStatusHelper. Its arguments are the stream to write
// ("stdout" or "stderr"), the number of bytes to write, "exit", "hold" or "exit-invalid-utf8", and the
// file that receives its process ID. The output is statusOutput's, so whatever is read of it matches
// the auth status pattern. With "hold" the helper keeps running after its output until it is
// stopped, or until statusHoldLimit. The helper ends the process itself, so the test binary's own
// result line never adds to the stream and the byte count stays exact.
func authStatusOutput(t testing.TB, args []string) {
	if len(args) != 4 {
		t.Fatalf("%s wants stream, size, exit|hold and pid file; got %q", authStatusHelper, args)
	}
	out := os.Stdout
	if args[0] == "stderr" {
		out = os.Stderr
	}
	size, err := strconv.Atoi(args[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(args[3], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	data := statusOutput(size, args[2] == "exit-invalid-utf8")
	// A write that fails because the reader stopped reading is what a capped stream looks like.
	_, _ = out.Write(data)
	if args[2] == "hold" {
		time.Sleep(statusHoldLimit)
	}
	syscall.Exit(0)
}

// statusOutput is what the auth status helper prints: size bytes of lines that name an account or,
// with invalid, one such line followed by lines that each hold a single byte that is never valid
// UTF-8, cut to size.
func statusOutput(size int, invalid bool) []byte {
	line := []byte("signed in to account fixture\n")
	if !invalid {
		return bytes.Repeat(line, size/len(line)+1)[:size]
	}
	return append(line, bytes.Repeat([]byte("\xff\n"), size/2+1)...)[:size]
}

// decodedLen is the length in bytes of p once decoded as UTF-8 with every invalid byte replaced by
// U+FFFD, as a Node stream with the 'utf8' encoding hands its text over.
func decodedLen(p []byte) int {
	n := 0
	for len(p) > 0 {
		r, size := utf8.DecodeRune(p)
		n += utf8.RuneLen(r)
		p = p[size:]
	}
	return n
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// installStatusFixture puts an executable at the proxy binary's path that runs authStatusOutput. The
// worker runs that path itself with its own arguments, so the launcher re-enters this package's
// fixture helper as gatewaytest's runner does: the test binary, the helper selector, and the
// helper's working directory open on descriptor 3.
func installStatusFixture(t *testing.T, home, state string, args ...string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "status-fixture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	words := []string{shellQuote(exe), shellQuote("-test.run=^TestGatewayFixtureHelper$"), "--", shellQuote("--gatewaytest-helper=" + authStatusHelper)}
	for _, a := range args {
		words = append(words, shellQuote(a))
	}
	script := "#!/bin/sh\nexec 3<" + shellQuote(dir) + " || exit 1\nexec " + strings.Join(words, " ") + "\n"
	bin := filepath.Join(state, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude-code-proxy"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestAuthStatusOutputIsCapped decides what the readiness check makes of an auth status command that
// prints more than the source's per-stream limit: the probe reports no Codex sign-in, never an
// authenticated one, and the command has been stopped and reaped by the time the check completes. A
// command that prints exactly the limit is read whole.
func TestAuthStatusOutputIsCapped(t *testing.T) {
	tests := map[string]struct {
		stream   string
		size     int
		mode     string
		wantAuth bool
	}{
		"success: exactly the limit on standard output is read whole and names the signed-in account": {
			stream: "stdout", size: maxStatusOutput, mode: "exit", wantAuth: true,
		},
		"error: more than the limit on standard output is not a signed-in account": {
			stream: "stdout", size: maxStatusOutput + 4096, mode: "exit",
		},
		"error: more than the limit on standard error is not a signed-in account": {
			stream: "stderr", size: maxStatusOutput + 4096, mode: "exit",
		},
		"error: a command still running after more than the limit on standard output is stopped and reaped": {
			stream: "stdout", size: maxStatusOutput + 4096, mode: "hold",
		},
		"error: a command still running after more than the limit on standard error is stopped and reaped": {
			stream: "stderr", size: maxStatusOutput + 4096, mode: "hold",
		},
		// The worker counts the raw bytes a stream carries. The source decodes each stream as UTF-8
		// first and counts the bytes of the decoded text, where every invalid byte has become a
		// three-byte replacement character, so it would refuse this output as over the limit and
		// report no sign-in. The two counts agree for every output that is valid UTF-8.
		"success: output under the limit in raw bytes names the signed-in account although its decoded text is over the limit": {
			stream: "stdout", size: maxStatusOutput - 4096, mode: "exit-invalid-utf8", wantAuth: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.mode == "exit-invalid-utf8" {
				out := statusOutput(tt.size, true)
				if raw, decoded := len(out), decodedLen(out); raw > maxStatusOutput || decoded <= maxStatusOutput {
					t.Fatalf("test precondition: output of %d raw bytes decodes to %d bytes; want at most %d raw and more than %d decoded", raw, decoded, maxStatusOutput, maxStatusOutput)
				}
			}
			home, env, state := sandbox(t)
			pidFile := filepath.Join(home, "status-fixture.pid")
			installStatusFixture(t, home, state, tt.stream, strconv.Itoa(tt.size), tt.mode, pidFile)
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			// The first /healthz starts the check; the poll waits for its completed summary. A command
			// that is not stopped at the limit holds the check past the failure bound.
			doc := pollHealth(t, w.public, "a completed readiness check", func(doc map[string]any) bool {
				state, _ := codexReadiness(t, doc)["state"].(string)
				return state != "" && state != "checking"
			})
			checks, ok := codexReadiness(t, doc)["checks"].(map[string]any)
			if !ok {
				t.Fatalf("a completed readiness check reports no checks: %v", codexReadiness(t, doc))
			}
			got := map[string]any{"proxyBinary": checks["proxyBinary"], "codexAuth": checks["codexAuth"]}
			if diff := gocmp.Diff(map[string]any{"proxyBinary": true, "codexAuth": tt.wantAuth}, got); diff != "" {
				t.Errorf("readiness checks (-want +got):\n%s", diff)
			}

			raw, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("the auth status fixture never ran: %v", err)
			}
			pid, err := strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			// A process that exited but was not waited for still answers signal 0 as a zombie.
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("the auth status command (pid %d) was not stopped and reaped when the check completed: signal 0 gave %v", pid, err)
			}
		})
	}
}
