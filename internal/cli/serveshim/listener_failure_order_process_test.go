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
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// failListener makes the supervisor's public listener fail on its own, as a kernel error would: it finds the listening
// socket bound to public among this process's descriptors and shuts down its receiving side. Linux then ends the
// listener's pending accept with an error that is not temporary. Sockets the listener accepted share its local
// address, so only the one that is still listening is chosen.
//
// Each descriptor is duplicated before it is examined, and only the duplicate is examined and shut down. A descriptor
// number can be closed and reused by another socket at any moment; the duplicate keeps referring to the socket that
// was examined, so the shutdown can never reach a socket that was not validated. One socket can appear under several
// descriptor numbers, among them the duplicates another case makes while it examines this process's descriptors, so
// sockets are told apart by their inode rather than by their descriptor.
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
	// listening holds one duplicate per listening socket, keyed by the socket's inode.
	listening := make(map[uint64]int)
	defer func() {
		for _, dup := range listening {
			_ = unix.Close(dup)
		}
	}()
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			// The descriptor was closed after the directory was read.
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(dup, &st) != nil || !isListeningOn(dup, port) {
			_ = unix.Close(dup)
			continue
		}
		if _, seen := listening[st.Ino]; seen {
			_ = unix.Close(dup)
			continue
		}
		listening[st.Ino] = dup
	}
	if len(listening) != 1 {
		t.Fatalf("found %d listening sockets on %s among this process's descriptors; want exactly the supervisor's one", len(listening), public)
	}
	for _, dup := range listening {
		if err := unix.Shutdown(dup, unix.SHUT_RD); err != nil {
			t.Fatalf("shut down the public listener's socket: %v", err)
		}
	}
}

// isListeningOn reports whether fd is a listening TCP socket bound to 127.0.0.1:port.
func isListeningOn(fd, port int) bool {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return false
	}
	inet, ok := sa.(*unix.SockaddrInet4)
	if !ok || inet.Port != port || inet.Addr != [4]byte{127, 0, 0, 1} {
		return false
	}
	accepting, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	return err == nil && accepting == 1
}

// confirmAlive asks a lingering worker whether it still runs. The supervisor closes its public listener in order only
// after its worker has exited, so a worker that answers after the listener failed proves that the listener failed
// while the supervisor's stop was still in progress, before any orderly close.
func confirmAlive(t *testing.T, child *fixtureChild) {
	t.Helper()
	if _, err := io.WriteString(child.conn, "alive?\n"); err == nil && child.lines.Scan() && child.lines.Text() == "alive" {
		return
	}
	t.Fatalf("the worker did not confirm that it still runs (%v); the supervisor may have closed its listener in order before the listener failed, so the case's ordering does not hold", child.lines.Err())
}

// release lets a lingering worker exit, which ends the supervisor's wait for it.
func release(t *testing.T, child *fixtureChild) {
	t.Helper()
	if _, err := io.WriteString(child.conn, "release\n"); err != nil {
		t.Fatalf("release the worker: %v", err)
	}
}

// listenerFailureText is the supervisor's diagnostic for the failure failListener causes on public.
func listenerFailureText(public string) string {
	return fmt.Sprintf("model-gateway: the public listener stopped: accept tcp4 %s: accept4: invalid argument", public)
}

// TestListenerFailureOrdering checks how a failure of the public listener decides the supervisor's outcome against a
// worker failure and a stop request that came first. The failure that is selected first decides the exit status, and
// a listener failure that loses is printed once as a warning, never dropped and never turned into the exit status.
// The worker lingers after its supervisor disconnects it and ignores SIGTERM, so each stop stays in progress until the
// test releases the worker; the listener fails inside that window, and the worker confirms that it was still running
// afterwards.
func TestListenerFailureOrdering(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: listener failure with nothing else pending stops the supervisor with that failure": {func(t *testing.T) {
			// This case shows that failListener produces a listener failure the supervisor sees; the other cases rely on it.
			s := startOrderedShim(t, map[string]string{orderLingerKey: "1"})
			child := s.awaitReady(t)
			failListener(t, s.public)
			if diff := gocmp.Diff("parent gone", child.farewell(t)); diff != "" {
				t.Fatalf("worker's view of the listener failure (-want +got):\n%s", diff)
			}
			release(t, child)
			s.awaitStop(t)
			if s.result == nil || s.result.Error() != listenerFailureText(s.public) {
				t.Errorf("supervisor stopped with %v; want exactly %q", s.result, listenerFailureText(s.public))
			}
			if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		}},
		"error: listener failure after a worker failure is warned about": {func(t *testing.T) {
			// The configured worker port stays held by the test, so the supervisor can never choose it as its public
			// port; the child only names it and never binds it.
			port := startForeign(t, 0).port
			s := startOrderedShim(t, map[string]string{"CODEX_GATEWAY_WORKER_PORT": strconv.Itoa(port), orderMisreportKey: "1", orderLingerKey: "1"})
			child := s.awaitReady(t)
			// The child reported another port than the configured one; the supervisor stops for it and disconnects the
			// child before the listener fails.
			if diff := gocmp.Diff("parent gone", child.farewell(t)); diff != "" {
				t.Fatalf("worker's view of the worker failure (-want +got):\n%s", diff)
			}
			failListener(t, s.public)
			confirmAlive(t, child)
			release(t, child)
			s.awaitStop(t)
			want := fmt.Sprintf("model-gateway: the shim worker reported port %d instead of CODEX_GATEWAY_WORKER_PORT %d", port%65535+1, port)
			if s.result == nil || s.result.Error() != want {
				t.Errorf("supervisor stopped with %v; want the worker failure that came first, exactly %q", s.result, want)
			}
			if diff := gocmp.Diff(listenerFailureText(s.public)+"\n", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch: want the listener failure printed once as a warning (-want +got):\n%s", diff)
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
			confirmAlive(t, child)
			release(t, child)
			s.awaitStop(t)
			if s.result != nil {
				t.Errorf("supervisor stopped with %v; want exit status 0 for the stop request that came first", s.result)
			}
			if diff := gocmp.Diff(listenerFailureText(s.public)+"\n", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch: want the listener failure printed once as a warning (-want +got):\n%s", diff)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case waits for its own worker; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}
