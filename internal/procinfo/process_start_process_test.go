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
	"errors"
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
// returns its process number as the child itself reports it.
func holdChild(t *testing.T) (pid int, finish func() error, output *bytes.Buffer) {
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
	report, reportOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = report.Close(); _ = reportOut.Close() })
	output = new(bytes.Buffer)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, output, output
	cmd.ExtraFiles = []*os.File{reportOut}
	done := make(chan error, 1)
	go func() { done <- env.RunFixtureWithin(cmd, 30*time.Second) }()
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
	return pid, finish, output
}

func TestProcessStartToken(t *testing.T) {
	child, finish, output := holdChild(t)
	p := Probe{}
	tests := map[string]struct{ pid int }{
		"success: this test process":       {pid: os.Getpid()},
		"success: a running fixture child": {pid: child},
	}
	tokens := make(map[int]StartToken, len(tests))
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			first, err := readStartToken(t, t.Context(), p, tt.pid)
			if err != nil || first == "" {
				t.Fatalf("first read of pid %d = %q, %v; want a token", tt.pid, first, err)
			}
			second, err := readStartToken(t, t.Context(), p, tt.pid)
			if err != nil {
				t.Fatalf("second read of pid %d: %v", tt.pid, err)
			}
			if diff := gocmp.Diff(first, second); diff != "" {
				t.Errorf("two reads of pid %d (-first +second): %s", tt.pid, diff)
			}
			id, err := p.Identity(t.Context(), tt.pid)
			if err != nil {
				t.Fatalf("identity of pid %d: %v", tt.pid, err)
			}
			if id.StartToken != first {
				t.Errorf("identity token %q; want %q", id.StartToken, first)
			}
			table, err := p.Table(t.Context())
			if err != nil {
				t.Fatalf("process table: %v", err)
			}
			if row := table[tt.pid]; row.StartToken != first {
				t.Errorf("table row token %q; want %q", row.StartToken, first)
			}
			tokens[tt.pid] = first
		})
	}
	if self, ok := tokens[os.Getpid()]; ok && self == tokens[child] {
		t.Errorf("test process and fixture child share token %q", self)
	}
	t.Run("error: an exited and reaped child", func(t *testing.T) {
		if err := finish(); err != nil {
			t.Fatalf("fixture child: %v; output: %s", err, output)
		}
		token, err := readStartToken(t, t.Context(), p, child)
		if token != "" || !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnknown) {
			t.Errorf("StartToken(%d) after exit = %q, %v; want an empty token and only %v", child, token, err, os.ErrNotExist)
		}
	})
}
