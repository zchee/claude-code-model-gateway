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

//go:build linux || darwin

package workerlink_test

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// fixtureFile is an inherited descriptor as a test builds it, with the port the import is told to expect.
type fixtureFile func(t *testing.T) (file *os.File, expected uint16)

func tcpListenerFile(network, address string) fixtureFile {
	return func(t *testing.T) (*os.File, uint16) {
		l := listenTCP(t, network, address)
		f, err := l.File()
		if f != nil {
			t.Cleanup(func() { _ = f.Close() })
		}
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		return f, tcpPort(t, l.Addr())
	}
}

// connectedTCPFile is the client end of an established loopback connection; the expected port is its own local
// port, so only the listening rule can refuse it.
func connectedTCPFile(t *testing.T) (*os.File, uint16) {
	l := listenTCP(t, "tcp4", "127.0.0.1:0")
	c, err := realDial(t.Context(), "tcp4", l.Addr().String())
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("dial returned %T, want TCP", c)
	}
	f, err := tcp.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	return f, tcpPort(t, c.LocalAddr())
}

// boundTCPFile is a loopback IPv4 stream socket bound to a port but never put into the listening state.
func boundTCPFile(t *testing.T) (*os.File, uint16) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	unix.CloseOnExec(fd)
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("bind: %v", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("getsockname: %v", err)
	}
	f := os.NewFile(uintptr(fd), "bound")
	t.Cleanup(func() { _ = f.Close() })
	return f, uint16(sa.(*unix.SockaddrInet4).Port)
}

func udpFile(t *testing.T) (*os.File, uint16) {
	pc, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if pc != nil {
		t.Cleanup(func() { _ = pc.Close() })
	}
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		t.Fatalf("listen udp returned %T", pc)
	}
	f, err := udp.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	return f, uint16(udp.LocalAddr().(*net.UDPAddr).Port)
}

func unixListenerFile(t *testing.T) (*os.File, uint16) {
	dir, err := os.MkdirTemp("", "wl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", filepath.Join(dir, "s"))
	if l != nil {
		t.Cleanup(func() { _ = l.Close() })
	}
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("listen unix returned %T", l)
	}
	f, err := ul.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	return f, 1
}

// unconnectedUnixStreamFile is a local stream socket that is neither connected nor listening: of the right family
// and type for the drain channel, but not connected to anything.
func unconnectedUnixStreamFile(t *testing.T) (*os.File, uint16) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "unconnected")
	t.Cleanup(func() { _ = f.Close() })
	return f, 1
}

func socketPairFile(typ int) fixtureFile {
	return func(t *testing.T) (*os.File, uint16) {
		a, _ := socketPair(t, typ)
		return a, 1
	}
}

func regularFile(t *testing.T) (*os.File, uint16) {
	f, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, 1
}

func pipeFile(t *testing.T) (*os.File, uint16) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, 1
}

func closedFile(t *testing.T) (*os.File, uint16) {
	f, port := tcpListenerFile("tcp4", "127.0.0.1:0")(t)
	_ = f.Close()
	return f, port
}

// acceptOutcome runs one Accept on any listener; the watchdog closes the listener to end a hang and then fails.
func acceptOutcome(t *testing.T, l net.Listener) (net.Conn, error) {
	t.Helper()
	type accepted struct {
		c   net.Conn
		err error
	}
	out := make(chan accepted, 1)
	go func() {
		c, err := l.Accept()
		out <- accepted{c, err}
	}()
	select {
	case a := <-out:
		return a.c, a.err
	case <-time.After(watchdog):
		_ = l.Close()
		if a := <-out; a.c != nil {
			_ = a.c.Close()
		}
		t.Fatalf("Accept did not return within %v", watchdog)
		return nil, nil
	}
}

// acceptWithin accepts one connection on any listener.
func acceptWithin(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	c, err := acceptOutcome(t, l)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return c
}

func noFile(*testing.T) (*os.File, uint16) { return nil, 1 }

func otherPort(port uint16) uint16 {
	if port == 65535 {
		return port - 1
	}
	return port + 1
}

// TestImportListenerAccepts checks the worker's side of a valid handoff: the inherited duplicate becomes an
// independent listener on the selected loopback port that accepts, and the inherited file is consumed. The
// listener may be any net.Listener; that it is not inherited by the worker's own children is decided by
// TestImportedDescriptorsNotInherited.
func TestImportListenerAccepts(t *testing.T) {
	tests := map[string]struct {
		network string
	}{
		"success: listener bound as tcp4":                     {network: "tcp4"},
		"success: listener bound as tcp on the IPv4 loopback": {network: "tcp"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, port := tcpListenerFile(tt.network, "127.0.0.1:0")(t)
			l, err := workerlink.ImportListener(f, port)
			if l != nil {
				t.Cleanup(func() { _ = l.Close() })
			}
			if err != nil {
				t.Fatalf("ImportListener: %v", err)
			}
			checkConsumed(t, f)
			if diff := gocmp.Diff(loopback(port), l.Addr().String()); diff != "" {
				t.Errorf("address (-want +got):\n%s", diff)
			}
			c, err := realDial(t.Context(), "tcp4", loopback(port))
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			if _, err := c.Write([]byte("hello")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := readExactly(t, acceptWithin(t, l), 5); got != "hello" {
				t.Errorf("accepted connection read %q, want %q", got, "hello")
			}
		})
	}
}

// TestImportListenerRefuses checks every refusal of the inherited listener. A refusal returns no listener (there
// is no fallback bind) and still consumes the inherited file. A bound TCP socket that is not listening is refused
// by the import where the system reports the listening state (Linux). On macOS, which does not (see listeningOf),
// it may pass the import, and then its first Accept fails at once; the worker command treats that as a failed start.
// A listener on another loopback address than exactly 127.0.0.1 is refused; that case runs where 127.0.0.2 can be
// bound without configuration (Linux, see otherLoopbackBindable).
func TestImportListenerRefuses(t *testing.T) {
	type refusal struct {
		fixture  fixtureFile
		expected func(port uint16) uint16
		// orFirstAccept also accepts a listener whose first Accept fails.
		orFirstAccept bool
	}
	tests := map[string]refusal{
		"error: expected port is zero":                  {fixture: tcpListenerFile("tcp4", "127.0.0.1:0"), expected: func(uint16) uint16 { return 0 }},
		"error: listener on another port":               {fixture: tcpListenerFile("tcp4", "127.0.0.1:0"), expected: otherPort},
		"error: listener on the wildcard address":       {fixture: tcpListenerFile("tcp4", "0.0.0.0:0")},
		"error: connected TCP stream":                   {fixture: connectedTCPFile},
		"error: bound TCP socket that is not listening": {fixture: boundTCPFile, orFirstAccept: !listeningObservable},
		"error: UDP socket":                             {fixture: udpFile},
		"error: Unix stream listener":                   {fixture: unixListenerFile},
		"error: drain channel given as the listener":    {fixture: socketPairFile(unix.SOCK_STREAM)},
		"error: regular file":                           {fixture: regularFile},
		"error: pipe":                                   {fixture: pipeFile},
		"error: closed file":                            {fixture: closedFile},
		"error: no file":                                {fixture: noFile},
	}
	if otherLoopbackBindable {
		tests["error: listener on another loopback address"] = refusal{fixture: tcpListenerFile("tcp4", "127.0.0.2:0")}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, port := tt.fixture(t)
			if tt.expected != nil {
				port = tt.expected(port)
			}
			l, err := workerlink.ImportListener(f, port)
			if l != nil {
				t.Cleanup(func() { _ = l.Close() })
			}
			if l != nil && err == nil && tt.orFirstAccept {
				checkConsumed(t, f)
				checkAcceptFails(t, l)
				return
			}
			if l != nil {
				t.Errorf("ImportListener returned listener %v; a refused handoff must return none", l.Addr())
			}
			if err == nil {
				t.Fatal("ImportListener accepted the descriptor")
			}
			checkConsumed(t, f)
		})
	}
}

// checkAcceptFails asserts that the first Accept on l fails; the watchdog of acceptOutcome only aborts a hang.
func checkAcceptFails(t *testing.T, l net.Listener) {
	t.Helper()
	c, err := acceptOutcome(t, l)
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("the imported socket that is not listening accepted a connection")
	}
}

// TestImportDrainChannelAccepts checks the worker's side of the drain channel: a connected local stream socket
// becomes an independent duplex connection, and the inherited file is consumed. The import checks the kind of
// socket only, so a channel whose parent has already shut down its writing side is accepted too, and the direction
// that remains works. The connection may be any net.Conn; that it is not inherited by the worker's own children is
// decided by TestImportedDescriptorsNotInherited.
func TestImportDrainChannelAccepts(t *testing.T) {
	tests := map[string]struct {
		// parentShutWrite shuts down the parent's writing side before the import.
		parentShutWrite bool
	}{
		"success: both directions open":                      {},
		"success: the parent has shut down its writing side": {parentShutWrite: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			child, parent := socketPair(t, unix.SOCK_STREAM)
			if tt.parentShutWrite {
				control(t, parent, func(fd int) {
					if err := unix.Shutdown(fd, unix.SHUT_WR); err != nil {
						t.Fatalf("shutdown: %v", err)
					}
				})
			}
			c, err := workerlink.ImportDrainChannel(child)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if err != nil {
				t.Fatalf("ImportDrainChannel: %v", err)
			}
			checkConsumed(t, child)
			peer := fileConn(t, parent)
			if tt.parentShutWrite {
				_ = c.SetReadDeadline(time.Now().Add(watchdog))
				if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
					t.Errorf("child read after the parent shut down its writing side = %d, %v; want end of file", n, err)
				}
			} else {
				writeAll(t, peer, "to-child\n")
				if got := readExactly(t, c, 9); got != "to-child\n" {
					t.Errorf("child read %q", got)
				}
			}
			writeAll(t, c, "to-parent\n")
			if got := readExactly(t, peer, 10); got != "to-parent\n" {
				t.Errorf("parent read %q", got)
			}
		})
	}
}

// TestImportDrainChannelRefuses checks every refusal of the inherited drain channel, including a descriptor that
// aliases the listener and a local stream socket that is not connected. A refusal returns no connection and still
// consumes the inherited file.
func TestImportDrainChannelRefuses(t *testing.T) {
	tests := map[string]fixtureFile{
		"error: listener given as the drain channel": tcpListenerFile("tcp4", "127.0.0.1:0"),
		"error: connected TCP stream":                connectedTCPFile,
		"error: UDP socket":                          udpFile,
		"error: Unix stream listener":                unixListenerFile,
		"error: Unix datagram socket pair":           socketPairFile(unix.SOCK_DGRAM),
		"error: unconnected Unix stream socket":      unconnectedUnixStreamFile,
		"error: regular file":                        regularFile,
		"error: pipe":                                pipeFile,
		"error: closed file":                         closedFile,
		"error: no file":                             noFile,
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			f, _ := fixture(t)
			c, err := workerlink.ImportDrainChannel(f)
			if c != nil {
				_ = c.Close()
				t.Error("ImportDrainChannel returned a connection for a refused descriptor")
			}
			if err == nil {
				t.Fatal("ImportDrainChannel accepted the descriptor")
			}
			checkConsumed(t, f)
		})
	}
}
