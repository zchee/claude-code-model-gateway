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

package procinfo

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"hold": func(t testing.TB, _ []string) {
		// The launcher reserves fd 3 for the working directory; fd 4 carries this process's number.
		if _, err := unix.Write(4, []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
	}})
}

// holdChild starts a fixture child that stays alive until finish is called and
// returns its process number as the child itself reports it. ended is closed
// when the fixture runner has returned, whether finish ended the child or the
// runner's deadline did.
func holdChild(t *testing.T) (pid int, finish func() error, ended <-chan struct{}, output *bytes.Buffer) {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, "hold")
	if err != nil {
		t.Fatal(err)
	}
	stdin, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = release.Close() })
	report, reportOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = report.Close(); _ = reportOut.Close() })
	output = new(bytes.Buffer)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, output, output
	cmd.ExtraFiles = []*os.File{reportOut}
	done := make(chan error, 1)
	runnerEnded := make(chan struct{})
	go func() {
		err := env.RunFixtureWithin(cmd, fixtureLifetime)
		// This process's copy of the writer would hide the end of the report
		// from the read below when the child exits without reporting.
		_ = reportOut.Close()
		close(runnerEnded)
		done <- err
	}()
	finish = sync.OnceValue(func() error { _ = release.Close(); return <-done })
	t.Cleanup(func() { _ = finish() })
	if err := report.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(report).ReadString('\n')
	if err != nil {
		t.Fatalf("fixture child did not report its pid: %v; run: %v; output: %s", err, finish(), output)
	}
	pid, err = strconv.Atoi(strings.TrimSuffix(line, "\n"))
	if err != nil || pid <= 0 {
		t.Fatalf("fixture child reported %q: %v", line, err)
	}
	return pid, finish, runnerEnded, output
}

// fixtureLifetime is the longest a fixture runner lets the child live.
const fixtureLifetime = 30 * time.Second

func TestProcessStartToken(t *testing.T) {
	child, finish, ended, output := holdChild(t)
	// A whole-host table scan needs more than the default probe budget on a busy host.
	p := New(func(key string) (string, bool) { return "30000", key == "CODEX_GATEWAY_PROBE_TIMEOUT_MS" })
	// The runner kills the child when its lifetime ends, so every read of the
	// live child must finish well before then; this deadline leaves a third of
	// the lifetime as headroom and bounds each probe below the probe budget.
	live, cancel := context.WithTimeout(t.Context(), fixtureLifetime*2/3)
	defer cancel()
	// One census for both processes, taken first, while the child is surely alive.
	table, err := p.Table(live)
	if err != nil {
		t.Fatalf("process table: %v", err)
	}
	tests := map[string]struct{ pid int }{
		"success: this test process":       {pid: os.Getpid()},
		"success: a running fixture child": {pid: child},
	}
	tokens := make(map[int]StartToken, len(tests))
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			first, err := readStartToken(t, live, p, tt.pid)
			if err != nil || first == "" {
				t.Fatalf("first read of pid %d = %q, %v; want a token", tt.pid, first, err)
			}
			second, err := readStartToken(t, live, p, tt.pid)
			if err != nil {
				t.Fatalf("second read of pid %d: %v", tt.pid, err)
			}
			if diff := gocmp.Diff(first, second); diff != "" {
				t.Errorf("two reads of pid %d (-first +second): %s", tt.pid, diff)
			}
			id, err := p.Identity(live, tt.pid)
			if err != nil {
				t.Fatalf("identity of pid %d: %v", tt.pid, err)
			}
			if id.StartToken != first {
				t.Errorf("identity token %q; want %q", id.StartToken, first)
			}
			row, ok := table[tt.pid]
			if !ok {
				t.Fatalf("process table has no row for pid %d", tt.pid)
			}
			if row.StartToken != first {
				t.Errorf("table row token %q; want %q", row.StartToken, first)
			}
			tokens[tt.pid] = first
		})
	}
	// Two processes that started within one tick of the kernel's start clock may
	// share a token, so the start fields, read here without the package, decide
	// whether these two must differ.
	selfStart, err := kernelStart(os.Getpid())
	if err != nil {
		t.Fatalf("start of the test process: %v", err)
	}
	childStart, err := kernelStart(child)
	if err != nil {
		t.Fatalf("start of the fixture child: %v", err)
	}
	select {
	case <-ended:
		t.Fatalf("the fixture runner ended the child before the reads of the live child finished; output: %s", output)
	default:
	}
	selfToken, childToken := tokens[os.Getpid()], tokens[child]
	switch {
	case selfToken == "" || childToken == "":
		// A subtest above has already failed for the missing token.
	case childStart == selfStart:
		t.Logf("fixture child and test process share the recorded start %d; their tokens may be equal", childStart)
	case selfToken == childToken:
		t.Errorf("test process (start %d) and fixture child (start %d) share token %q", selfStart, childStart, selfToken)
	}
	t.Run("success: a reaped child's number gives either a token or an error with an empty token", func(t *testing.T) {
		if err := finish(); err != nil {
			t.Fatalf("fixture child: %v; output: %s", err, output)
		}
		// The number may already belong to a new process, and a second lookup
		// cannot disprove a first sample, so this makes no claim about the
		// number's next holder. A stale token and reuse under the same start are
		// decided by the controlled samples of the reuse tests.
		token, err := readStartToken(t, t.Context(), p, child)
		switch {
		case err != nil && token != "":
			t.Errorf("StartToken(%d) after exit = %q, %v; want an empty token with the error", child, token, err)
		case err == nil && token == "":
			t.Errorf("StartToken(%d) after exit returned an empty token without an error", child)
		}
	})
}
