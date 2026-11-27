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
	"net"
	"os"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// The tests in this file start a child process through gatewaytest. A descriptor of this process that is not
// close-on-exec is inherited by that child, so the sockets the child finds open are the ones that would leak into
// the children of a supervisor or of a worker. The child also isolates checks that read the set of every descriptor
// of the process.

// helperLinePrefix marks the lines a helper reports; the rest of its output is the test framework's.
const helperLinePrefix = "workerlink-helper: "

// inheritedSocket is one socket descriptor the child found open. Its listening state is not recorded: macOS does
// not answer that question (see listeningOf), so a listener is identified by its family and bound port.
type inheritedSocket struct {
	FD     int    `json:"fd"`
	Family string `json:"family,omitzero"`
	Type   int    `json:"type,omitzero"`
	Port   int    `json:"port,omitzero"`
	// Pending is what is queued for reading on the socket, peeked without consuming it.
	Pending string `json:"pending,omitzero"`
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		"inherited-sockets":   inheritedSocketsHelper,
		"descriptor-sets":     descriptorSetsHelper,
		"reserve-descriptors": reserveDescriptorsHelper,
		"reserve-rollback":    reserveRollbackHelper,
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

// nothingToRead reports whether a failed peek means only that the socket has nothing to read: EAGAIN for a socket
// with an empty queue; ENOTCONN for a TCP socket that is listening or not connected (Linux tcp(7) receive; macOS
// soreceive, xnu bsd/kern/uipc_socket.c:3470-3472); EINVAL for a local stream socket that is listening or not
// connected (Linux unix_stream_recvmsg).
func nothingToRead(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOTCONN) || errors.Is(err, unix.EINVAL)
}

// inheritedSocketsHelper reports every socket descriptor open in the child. Any inspection that fails for another
// reason than the ones nothingToRead names fails the helper, so that a socket cannot hide behind an error.
func inheritedSocketsHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	fds, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	for _, fd := range fds {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			t.Fatalf("fstat %d: %v", fd, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
			continue
		}
		s := inheritedSocket{FD: fd}
		if s.Type, err = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); err != nil {
			t.Fatalf("SO_TYPE of %d: %v", fd, err)
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatalf("getsockname of %d: %v", fd, err)
		}
		switch sa := sa.(type) {
		case *unix.SockaddrInet4:
			s.Family, s.Port = "inet4", sa.Port
		case *unix.SockaddrInet6:
			s.Family, s.Port = "inet6", sa.Port
		case *unix.SockaddrUnix:
			s.Family = "unix"
		default:
			s.Family = "other"
		}
		buf := make([]byte, 256)
		switch n, _, err := unix.Recvfrom(fd, buf, unix.MSG_PEEK|unix.MSG_DONTWAIT); {
		case err == nil:
			s.Pending = string(buf[:n])
		case !nothingToRead(err):
			t.Fatalf("peek of %d: %v", fd, err)
		}
		line, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		report(t, "%s", line)
	}
}

// descriptorChangeOf reads the open descriptors before and after fn.
func descriptorChangeOf(t *testing.T, fn func()) descriptorChange {
	t.Helper()
	before, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	fn()
	after, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	return changeBetween(before, after)
}

// descriptorSetsHelper reports what the child's descriptor sets show for nothing, for a file kept open, for the
// file closed again, and for a socket pair kept open.
func descriptorSetsHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	report(t, "nothing: %v", descriptorChangeOf(t, func() {}))
	var f *os.File
	opened := descriptorChangeOf(t, func() {
		var err error
		if f, err = os.Open(os.DevNull); err != nil {
			t.Fatalf("open: %v", err)
		}
	})
	report(t, "an open file adds its number: %t", gocmp.Equal(descriptorChange{Opened: []int{int(f.Fd())}, Closed: []int{}}, opened))
	closed := descriptorChangeOf(t, func() { _ = f.Close() })
	report(t, "closing it removes that number: %t", gocmp.Equal(descriptorChange{Opened: []int{}, Closed: opened.Opened}, closed))
	var a, b *os.File
	pair := descriptorChangeOf(t, func() { a, b = socketPair(t, unix.SOCK_STREAM) })
	report(t, "a socket pair adds its two numbers: %t", gocmp.Equal(descriptorChange{Opened: []int{int(a.Fd()), int(b.Fd())}, Closed: []int{}}, pair, cmpopts.SortSlices(func(x, y int) bool { return x < y })))
}

// reserveDescriptorsHelper reports how the descriptor set changes across reservation cycles that end with End:
// one that takes nothing, one that takes both transfer files and closes them, and one that is published and leased.
func reserveDescriptorsHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	reg := workerlink.NewRegistry(realListen, realDial)
	cycle := func(generation workerlink.Generation, take func(res *workerlink.Reservation)) func() {
		return func() {
			res, err := reg.Reserve(t.Context(), generation, 0)
			if res != nil {
				defer func() {
					if err := res.End(); err != nil {
						t.Fatalf("End %s: %v", generation, err)
					}
				}()
			}
			if err != nil {
				t.Fatalf("Reserve %s: %v", generation, err)
			}
			take(res)
		}
	}
	untaken := func(*workerlink.Reservation) {}
	taken := func(res *workerlink.Reservation) {
		f, err := res.File()
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		_, child, err := res.DrainChannel()
		if child != nil {
			_ = child.Close()
		}
		if err != nil {
			t.Fatalf("DrainChannel: %v", err)
		}
	}
	leased := func(res *workerlink.Reservation) {
		taken(res)
		if err := res.Publish(4101); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		lease, err := reg.Acquire(t.Context(), "")
		if lease != nil {
			_ = lease.Close()
		}
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		res.Retire()
	}
	// The first cycle opens whatever the runtime opens once, such as the network poller.
	cycle("warm-up", leased)()
	report(t, "untaken %v", descriptorChangeOf(t, cycle("untaken", untaken)))
	report(t, "taken %v", descriptorChangeOf(t, cycle("taken", taken)))
	report(t, "leased %v", descriptorChangeOf(t, cycle("leased", leased)))
}

// reserveRollbackHelper reports what a Reserve does when the creation of its drain channel pair fails, after a
// warm-up cycle: the descriptor set across the failed Reserve, whether a listener it got was left open, whether
// anything became eligible, and whether a Reserve of the same generation then succeeds.
func reserveRollbackHelper(tb testing.TB, _ []string) {
	t := helperT(tb)
	listens := newRecordingListens()
	reg := workerlink.NewRegistry(listens.listen, realDial)
	pairs := newDrainPairs(t, reg, false, 2)
	warm, err := reg.Reserve(t.Context(), "warm-up", 0)
	if warm != nil {
		defer func() { _ = warm.End() }()
	}
	if err != nil {
		t.Fatalf("Reserve warm-up: %v", err)
	}
	if err := warm.End(); err != nil {
		t.Fatalf("End warm-up: %v", err)
	}
	var res *workerlink.Reservation
	change := descriptorChangeOf(t, func() { res, err = reg.Reserve(t.Context(), "gen-a", 0) })
	if res != nil {
		_ = res.End()
	}
	report(t, "the failing pair was asked for: %t", pairs.count() == 2)
	report(t, "reservation returned: %t", res != nil)
	report(t, "reserve failed: %t", err != nil)
	report(t, "descriptors %v", change)
	// A listener the failed Reserve bound must already be closed: closing it again fails.
	_, bound := listens.snapshot()
	left := false
	for _, l := range bound[1:] {
		if err := l.Close(); !errors.Is(err, net.ErrClosed) {
			left = true
		}
	}
	report(t, "listener left open: %t", left)
	lease, err := reg.Acquire(t.Context(), "")
	if lease != nil {
		_ = lease.Close()
	}
	report(t, "acquire unavailable: %t", lease == nil && errors.Is(err, workerlink.ErrUnavailable))
	retry, err := reg.Reserve(t.Context(), "gen-a", 0)
	if retry != nil {
		_ = retry.End()
	}
	report(t, "reserve after the failure: %t", err == nil && retry != nil)
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

// leakedInto describes which of the test's marks the child found: an IPv4 socket bound to the port of the test's
// isolated listener (identified by its port, in any state), or a socket with a tag queued for reading.
func leakedInto(sockets []inheritedSocket, port int, tag string) []string {
	var found []string
	for _, s := range sockets {
		if port != 0 && s.Family == "inet4" && s.Port == port {
			found = append(found, fmt.Sprintf("descriptor %d bound to port %d", s.FD, port))
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

// TestFixtureChildDescriptorSets checks the descriptor sets the balance checks of the child rely on: nothing done
// changes nothing, an open file adds exactly its number and closing it removes it, and a socket pair adds its two.
func TestFixtureChildDescriptorSets(t *testing.T) {
	want := []string{
		"nothing: {[] []}",
		"an open file adds its number: true",
		"closing it removes that number: true",
		"a socket pair adds its two numbers: true",
	}
	if diff := gocmp.Diff(want, runHelper(t, "descriptor-sets")); diff != "" {
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
// reservation ended with End leaves the set of open descriptors as it found it: not when nothing was taken (End
// closes the untransferred child end), not when both transfer files were taken and closed, and not after
// publication and a lease. The sets are compared, not counted, so that a leak cannot be offset by another closure.
func TestReserveReleasesEveryDescriptor(t *testing.T) {
	want := []string{"untaken {[] []}", "taken {[] []}", "leased {[] []}"}
	if diff := gocmp.Diff(want, runHelper(t, "reserve-descriptors")); diff != "" {
		t.Errorf("descriptor changes across End, per cycle (-want +got):\n%s", diff)
	}
}

// TestReserveRollsBackWhenPairFails checks a Reserve whose drain channel pair cannot be created, through the
// registry's pair factory made to fail for that one reservation, in a child process: the Reserve reaches the
// failing factory, fails, returns no reservation, leaves the descriptor set as it was (its listener, bound for real,
// is closed), leaves nothing eligible, and a Reserve of the same generation then succeeds. Either order of binding
// and pair creation passes. Which error it returns is not decided here.
func TestReserveRollsBackWhenPairFails(t *testing.T) {
	want := []string{
		"the failing pair was asked for: true",
		"reservation returned: false",
		"reserve failed: true",
		"descriptors {[] []}",
		"listener left open: false",
		"acquire unavailable: true",
		"reserve after the failure: true",
	}
	if diff := gocmp.Diff(want, runHelper(t, "reserve-rollback")); diff != "" {
		t.Errorf("helper report (-want +got):\n%s", diff)
	}
}
