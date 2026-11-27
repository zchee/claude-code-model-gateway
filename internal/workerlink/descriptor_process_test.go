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

package workerlink_test

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// The tests in this file start a child process through gatewaytest. A descriptor of this process that is not
// close-on-exec is inherited by that child, so the sockets the child finds open are the ones that would leak into
// the children of a supervisor or of a worker. The child also isolates checks that change process-wide state
// (the descriptor limit) or count every descriptor of the process.

// helperLinePrefix marks the lines a helper reports; the rest of its output is the test framework's.
const helperLinePrefix = "workerlink-helper: "

// inheritedSocket is one socket descriptor the child found open.
type inheritedSocket struct {
	FD        int    `json:"fd"`
	Family    string `json:"family,omitzero"`
	Type      int    `json:"type,omitzero"`
	Listening bool   `json:"listening,omitzero"`
	Port      int    `json:"port,omitzero"`
	// Pending is what is queued for reading on the socket, peeked without consuming it.
	Pending string `json:"pending,omitzero"`
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		"inherited-sockets":   inheritedSocketsHelper,
		"reserve-descriptors": reserveDescriptorsHelper,
		"descriptor-limit":    descriptorLimitHelper,
	})
}

// helperT returns the *testing.T DispatchHelper passes to a helper.
func helperT(tb testing.TB) *testing.T {
	t, ok := tb.(*testing.T)
	if !ok {
		tb.Fatalf("helper runs under %T, want *testing.T", tb)
	}
	return t
}

// report writes one helper line for the parent to read.
func report(t *testing.T, format string, args ...any) {
	t.Helper()
	if _, err := fmt.Fprintf(os.Stdout, helperLinePrefix+format+"\n", args...); err != nil {
		t.Fatalf("report: %v", err)
	}
}

// inheritedSocketsHelper reports every socket descriptor open in the child.
func inheritedSocketsHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("read /dev/fd: %v", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var st unix.Stat_t
		// The directory ReadDir read through is closed again; any descriptor that is gone is skipped.
		if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
			continue
		}
		s := inheritedSocket{FD: fd}
		s.Type, _ = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
		listening, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
		s.Listening = listening != 0
		switch sa, _ := unix.Getsockname(fd); sa := sa.(type) {
		case *unix.SockaddrInet4:
			s.Family, s.Port = "inet4", sa.Port
		case *unix.SockaddrInet6:
			s.Family, s.Port = "inet6", sa.Port
		case *unix.SockaddrUnix:
			s.Family = "unix"
		default:
			s.Family = "other"
		}
		if !s.Listening {
			buf := make([]byte, 256)
			if n, _, err := unix.Recvfrom(fd, buf, unix.MSG_PEEK|unix.MSG_DONTWAIT); err == nil && n > 0 {
				s.Pending = string(buf[:n])
			}
		}
		line, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		report(t, "%s", line)
	}
}

// descriptorBalance runs one reservation cycle and returns how many more descriptors are open after it than
// before it.
func descriptorBalance(t *testing.T, cycle func()) int {
	t.Helper()
	before, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	cycle()
	after, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return after - before
}

// reserveDescriptorsHelper reports the descriptor balance of reservation cycles that end with End: one that
// takes nothing, one that takes both transfer files and closes them, and one that is published and leased.
func reserveDescriptorsHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	reg := workerlink.NewRegistry(realListen, realDial)
	cycle := func(generation workerlink.Generation, take func(res *workerlink.Reservation)) func() {
		return func() {
			res, err := reg.Reserve(t.Context(), generation, 0)
			if err != nil {
				t.Fatalf("Reserve %s: %v", generation, err)
			}
			take(res)
			if err := res.End(); err != nil {
				t.Fatalf("End %s: %v", generation, err)
			}
		}
	}
	untaken := func(*workerlink.Reservation) {}
	taken := func(res *workerlink.Reservation) {
		f, err := res.File()
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		_ = f.Close()
		_, child, err := res.DrainChannel()
		if err != nil {
			t.Fatalf("DrainChannel: %v", err)
		}
		_ = child.Close()
	}
	leased := func(res *workerlink.Reservation) {
		taken(res)
		if err := res.Publish(4101); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		lease, err := reg.Acquire(t.Context(), "")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		_ = lease.Close()
		res.Retire()
	}
	// The first cycle opens whatever the runtime opens once, such as the network poller.
	cycle("warm-up", leased)()
	report(t, "untaken %d", descriptorBalance(t, cycle("untaken", untaken)))
	report(t, "taken %d", descriptorBalance(t, cycle("taken", taken)))
	report(t, "leased %d", descriptorBalance(t, cycle("leased", leased)))
}

// lowerDescriptorLimit sets the soft descriptor limit to the lowest free descriptor number, so that no further
// descriptor can be opened, and returns the function that restores the limit.
func lowerDescriptorLimit(t *testing.T) func() {
	t.Helper()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	lowest, err := unix.Dup(0)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	_ = unix.Close(lowest)
	lowered := limit
	lowered.Cur = uint64(lowest)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatalf("setrlimit: %v", err)
	}
	return func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatalf("restoring the descriptor limit: %v", err)
		}
	}
}

func probeSocketPair() error {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}
	return err
}

// descriptorLimitHelper runs with the descriptor limit lowered so that no socket pair can be created. With the
// argument "probe" it only reports that the limit does that and that restoring it undoes it; with "reserve" it
// reports what a Reserve whose listener needs no descriptor does when its socket pair cannot be created.
func descriptorLimitHelper(tb testing.TB, args []string) {
	t := helperT(tb)
	if len(args) != 1 || (args[0] != "probe" && args[0] != "reserve") {
		t.Fatalf("descriptor-limit arguments %q, want probe or reserve", args)
	}
	if args[0] == "probe" {
		restore := lowerDescriptorLimit(t)
		lowered := probeSocketPair()
		restore()
		report(t, "limit refuses a socket pair: %t", errors.Is(lowered, unix.EMFILE))
		report(t, "restored limit allows a socket pair: %t", probeSocketPair() == nil)
		return
	}
	listens := newFakeListens()
	reg := workerlink.NewRegistry(listens.listen, realDial)
	warm, err := reg.Reserve(t.Context(), "warm-up", 41001)
	if err != nil {
		t.Fatalf("Reserve warm-up: %v", err)
	}
	if err := warm.End(); err != nil {
		t.Fatalf("End warm-up: %v", err)
	}
	before, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	restore := lowerDescriptorLimit(t)
	refused := errors.Is(probeSocketPair(), unix.EMFILE)
	res, err := reg.Reserve(t.Context(), "gen-a", 41002)
	restore()
	report(t, "limit refuses a socket pair: %t", refused)
	report(t, "reservation returned: %t", res != nil)
	report(t, "reserve failed: %t", err != nil)
	if res != nil {
		_ = res.End()
	}
	listens.mu.Lock()
	l := listens.listeners[loopback(41002)]
	listens.mu.Unlock()
	report(t, "listener left open: %t", l != nil && !isClosed(l.closed))
	lease, err := reg.Acquire(t.Context(), "")
	report(t, "acquire unavailable: %t", lease == nil && errors.Is(err, workerlink.ErrUnavailable))
	if lease != nil {
		_ = lease.Close()
	}
	after, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	report(t, "descriptors left open: %d", after-before)
	retry, err := reg.Reserve(t.Context(), "gen-a", 41002)
	report(t, "reserve after the limit is restored: %t", err == nil && retry != nil)
	if retry != nil {
		_ = retry.End()
	}
}

// runHelper runs a helper in a child process and returns the lines it reported.
func runHelper(t *testing.T, name string, args ...string) []string {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	cmd, err := env.HelperCommand(t, name, args...)
	if err != nil {
		t.Fatalf("HelperCommand %s: %v", name, err)
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := env.RunFixtureWithin(cmd, 30*time.Second); err != nil {
		t.Fatalf("helper %s: %v\n%s", name, err, out.String())
	}
	var lines []string
	for line := range strings.Lines(out.String()) {
		if text, ok := strings.CutPrefix(line, helperLinePrefix); ok {
			lines = append(lines, strings.TrimSuffix(text, "\n"))
		}
	}
	return lines
}

// childSockets returns the sockets a child process started now inherits from this process.
func childSockets(t *testing.T) []inheritedSocket {
	t.Helper()
	var sockets []inheritedSocket
	for _, line := range runHelper(t, "inherited-sockets") {
		var s inheritedSocket
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("helper line %q: %v", line, err)
		}
		sockets = append(sockets, s)
	}
	return sockets
}

// clearCloseOnExec gives f the flags of a descriptor a child inherits: close-on-exec cleared.
func clearCloseOnExec(t *testing.T, f *os.File) {
	t.Helper()
	control(t, f, func(fd int) {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			t.Fatalf("F_SETFD: %v", err)
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 {
			t.Fatalf("descriptor %d still close-on-exec (flags %#x, %v)", fd, flags, err)
		}
	})
}

// holdLowDescriptors keeps descriptors 0 to 3 open for the test. The child gets its standard streams and the
// launcher's directory at those numbers, which would hide a leaked descriptor that had one of them.
func holdLowDescriptors(t *testing.T) {
	t.Helper()
	for fd := range 4 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			continue
		}
		f, err := os.Open(os.DevNull)
		if f != nil {
			t.Cleanup(func() { _ = f.Close() })
		}
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if int(f.Fd()) != fd {
			t.Fatalf("placeholder for descriptor %d opened at %d", fd, f.Fd())
		}
	}
}

// leakedInto describes which of the test's marks the child found: a listening IPv4 socket on a port, or a socket
// with a tag queued for reading.
func leakedInto(sockets []inheritedSocket, port int, tag string) []string {
	var found []string
	for _, s := range sockets {
		if port != 0 && s.Listening && s.Family == "inet4" && s.Port == port {
			found = append(found, fmt.Sprintf("descriptor %d listening on port %d", s.FD, port))
		}
		if tag != "" && strings.Contains(s.Pending, tag) {
			found = append(found, fmt.Sprintf("descriptor %d with %q queued", s.FD, tag))
		}
	}
	return found
}

// TestFixtureChildSeesInheritedSockets checks the child's view against sockets whose flags the test sets: a
// socket pair end and a listener with close-on-exec cleared are found, the same with close-on-exec kept are not.
func TestFixtureChildSeesInheritedSockets(t *testing.T) {
	holdLowDescriptors(t)
	leakedEnd, leakedPeer := socketPair(t, unix.SOCK_STREAM)
	clearCloseOnExec(t, leakedEnd)
	writeAll(t, fileConn(t, leakedPeer), "leaked-pair")
	_, keptPeer := socketPair(t, unix.SOCK_STREAM)
	writeAll(t, fileConn(t, keptPeer), "kept-pair")
	leakedFile, leakedPort := tcpListenerFile("tcp4", "127.0.0.1:0")(t)
	clearCloseOnExec(t, leakedFile)
	keptPort := tcpPort(t, listenTCP(t, "tcp4", "127.0.0.1:0").Addr())

	sockets := childSockets(t)
	got := map[string]bool{
		"leaked pair end":     len(leakedInto(sockets, 0, "leaked-pair")) != 0,
		"kept pair end":       len(leakedInto(sockets, 0, "kept-pair")) != 0,
		"leaked listener":     len(leakedInto(sockets, int(leakedPort), "")) != 0,
		"kept listener":       len(leakedInto(sockets, int(keptPort), "")) != 0,
		"some socket is seen": len(sockets) != 0,
	}
	want := map[string]bool{
		"leaked pair end":     true,
		"kept pair end":       false,
		"leaked listener":     true,
		"kept listener":       false,
		"some socket is seen": true,
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("what the child inherited (-want +got):\n%s\nsockets: %+v", diff, sockets)
	}
}

// TestFixtureChildDescriptorLimit checks the lowered descriptor limit the rollback check relies on: it refuses a
// socket pair, and restoring it allows one again.
func TestFixtureChildDescriptorLimit(t *testing.T) {
	want := []string{"limit refuses a socket pair: true", "restored limit allows a socket pair: true"}
	if diff := gocmp.Diff(want, runHelper(t, "descriptor-limit", "probe")); diff != "" {
		t.Errorf("helper report (-want +got):\n%s", diff)
	}
}

// TestReservationHandlesNotInherited checks that what a reservation keeps for itself, its listener and the
// parent's end of the drain channel, is close-on-exec: a child the supervisor starts inherits neither.
func TestReservationHandlesNotInherited(t *testing.T) {
	holdLowDescriptors(t)
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	f, err := res.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// The caller closes the transfer files after the start, as the worker manager does.
	if err := f.Close(); err != nil {
		t.Fatalf("closing the duplicate: %v", err)
	}
	_, child := drainChannel(t, res)
	const tag = "queued-for-the-parent-end"
	writeAll(t, child, tag)
	if found := leakedInto(childSockets(t), int(res.Port()), tag); len(found) != 0 {
		t.Errorf("a child inherited the reservation's own handles: %v", found)
	}
}

// TestImportedDescriptorsNotInherited checks that what the worker imports is close-on-exec, starting from the
// flags an inherited descriptor has (close-on-exec cleared): a child the worker starts inherits neither the
// imported listener nor the imported drain channel.
func TestImportedDescriptorsNotInherited(t *testing.T) {
	const tag = "queued-for-the-imported-end"
	tests := map[string]struct {
		// imp imports an inherited-style descriptor and returns what marks it: its port or the tag queued on it.
		imp func(t *testing.T) (port int, tag string)
	}{
		"success: imported listener": {imp: func(t *testing.T) (int, string) {
			f, port := tcpListenerFile("tcp4", "127.0.0.1:0")(t)
			clearCloseOnExec(t, f)
			l, err := workerlink.ImportListener(f, port)
			if l != nil {
				t.Cleanup(func() { _ = l.Close() })
			}
			if err != nil {
				t.Fatalf("ImportListener: %v", err)
			}
			checkConsumed(t, f)
			return int(port), ""
		}},
		"success: imported drain channel": {imp: func(t *testing.T) (int, string) {
			end, peer := socketPair(t, unix.SOCK_STREAM)
			clearCloseOnExec(t, end)
			c, err := workerlink.ImportDrainChannel(end)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if err != nil {
				t.Fatalf("ImportDrainChannel: %v", err)
			}
			checkConsumed(t, end)
			writeAll(t, fileConn(t, peer), tag)
			return 0, tag
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			holdLowDescriptors(t)
			port, mark := tt.imp(t)
			if found := leakedInto(childSockets(t), port, mark); len(found) != 0 {
				t.Errorf("a child inherited the imported descriptor: %v", found)
			}
		})
	}
}

// TestReserveReleasesEveryDescriptor checks, in a child process where nothing else opens descriptors, that a
// reservation ended with End leaves no descriptor open: not when nothing was taken (End closes the untransferred
// child end), not when both transfer files were taken and closed, and not after publication and a lease.
func TestReserveReleasesEveryDescriptor(t *testing.T) {
	want := []string{"untaken 0", "taken 0", "leased 0"}
	if diff := gocmp.Diff(want, runHelper(t, "reserve-descriptors")); diff != "" {
		t.Errorf("descriptors left open after End, per cycle (-want +got):\n%s", diff)
	}
}

// TestReserveRollsBackWhenPairFails checks a Reserve whose drain channel cannot be created, because the child's
// descriptor limit leaves no room for a socket pair: it fails, returns no reservation, closes a listener it got,
// leaves no descriptor open and nothing eligible, and a Reserve of the same generation succeeds once the limit is
// restored. Which error it returns is not decided here.
func TestReserveRollsBackWhenPairFails(t *testing.T) {
	want := []string{
		"limit refuses a socket pair: true",
		"reservation returned: false",
		"reserve failed: true",
		"listener left open: false",
		"acquire unavailable: true",
		"descriptors left open: 0",
		"reserve after the limit is restored: true",
	}
	if diff := gocmp.Diff(want, runHelper(t, "descriptor-limit", "reserve")); diff != "" {
		t.Errorf("helper report (-want +got):\n%s", diff)
	}
}
