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

//go:build process && linux

package serveshim_test

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// failListener makes the supervisor's public listener fail on its own, as a kernel error would: it finds the listening
// socket bound to public among this process's descriptors and shuts down its receiving side. Linux then ends the
// listener's pending accept with an error that is not temporary. Sockets the listener accepted share its local
// address, so only the one that is still listening is chosen.
func failListener(t *testing.T, public string) {
	t.Helper()
	host, portText, err := net.SplitHostPort(public)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("unexpected public listener address %q", public)
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var listening []int
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		inet, ok := sa.(*unix.SockaddrInet4)
		if !ok || inet.Port != port || inet.Addr != [4]byte{127, 0, 0, 1} {
			continue
		}
		if accepting, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err == nil && accepting == 1 {
			listening = append(listening, fd)
		}
	}
	if len(listening) != 1 {
		t.Fatalf("found %d listening sockets on %s among this process's descriptors; want exactly the supervisor's one", len(listening), public)
	}
	if err := unix.Shutdown(listening[0], unix.SHUT_RD); err != nil {
		t.Fatalf("shut down the public listener's socket: %v", err)
	}
}

// TestListenerFailureOrdering checks how a failure of the public listener decides the supervisor's outcome against a
// worker failure and a stop request that came first. The failure that is selected first decides the exit status, and
// a listener failure that loses is printed once as a warning, never dropped and never turned into the exit status.
// The worker lingers after its supervisor disconnects it, so each stop stays in progress for the few seconds the
// supervisor waits before it signals the worker, and the listener fails inside that window.
func TestListenerFailureOrdering(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: listener failure with nothing else pending stops the supervisor with that failure": {func(t *testing.T) {
			// This case shows that failListener produces a listener failure the supervisor sees; the other cases rely on it.
			s := startOrderedShim(t, map[string]string{orderLingerKey: "1"})
			s.awaitReady(t)
			failListener(t, s.public)
			s.awaitStop(t)
			if s.result == nil || !strings.Contains(s.result.Error(), s.public) {
				t.Errorf("supervisor stopped with %v; want the failure of its public listener on %s", s.result, s.public)
			}
			if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		}},
		"error: listener failure after a worker failure is warned about": {func(t *testing.T) {
			port := freePort(t)
			s := startOrderedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port), orderMisreportKey: "1", orderLingerKey: "1"})
			child := s.awaitReady(t)
			// The child reported another port than the configured one; the supervisor stops for it and disconnects the
			// child before the listener fails.
			if diff := gocmp.Diff("parent gone", child.farewell(t)); diff != "" {
				t.Fatalf("worker's view of the worker failure (-want +got):\n%s", diff)
			}
			failListener(t, s.public)
			s.awaitStop(t)
			if s.result == nil || strings.Contains(s.result.Error(), s.public) {
				t.Errorf("supervisor stopped with %v; want the worker failure that came first", s.result)
			}
			lines := diagnosticLines(t, s.stderr.String())
			if len(lines) != 1 || !strings.Contains(lines[0], s.public) {
				t.Errorf("stderr holds %d line(s); want exactly one warning for the failure of the public listener on %s:\n%s", len(lines), s.public, s.stderr)
			}
		}},
		"success: listener failure after a stop request keeps exit status 0": {func(t *testing.T) {
			s := startOrderedShim(t, map[string]string{orderLingerKey: "1"})
			child := s.awaitReady(t)
			s.stop()
			if diff := gocmp.Diff("parent gone", child.farewell(t)); diff != "" {
				t.Fatalf("worker's view of the stop (-want +got):\n%s", diff)
			}
			failListener(t, s.public)
			s.awaitStop(t)
			if s.result != nil {
				t.Errorf("supervisor stopped with %v; want exit status 0 for the stop request that came first", s.result)
			}
			lines := diagnosticLines(t, s.stderr.String())
			if len(lines) != 1 || !strings.Contains(lines[0], s.public) {
				t.Errorf("stderr holds %d line(s); want exactly one warning for the failure of the public listener on %s:\n%s", len(lines), s.public, s.stderr)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case waits out its own lingering worker; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}
