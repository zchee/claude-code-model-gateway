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

package workerlink_test

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// sctpListenerFile returns a listening one-to-one SCTP socket bound to 127.0.0.1 at a port the system selects, and
// that port: an IPv4 stream socket that listens and has no peer, like a TCP listener, but whose transport protocol is
// not TCP (socket(AF_INET, SOCK_STREAM, IPPROTO_SCTP), sctp(7)). It returns false, after logging why, when this kernel
// cannot create, bind or listen on such a socket, for example because SCTP is not built or its module is not
// available. A socket that is created but is not what the case needs fails the test.
func sctpListenerFile(t *testing.T) (*os.File, uint16, bool) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_SCTP)
	if err != nil {
		t.Logf("a one-to-one SCTP socket could not be created on this kernel (%v); the case was not constructed and asserts nothing", err)
		return nil, 0, false
	}
	f := os.NewFile(uintptr(fd), "sctp-listener")
	t.Cleanup(func() { _ = f.Close() })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Logf("the SCTP socket could not be bound to 127.0.0.1 (%v); the case was not constructed and asserts nothing", err)
		return nil, 0, false
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Logf("the SCTP socket could not listen (%v); the case was not constructed and asserts nothing", err)
		return nil, 0, false
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname of the SCTP socket: %v", err)
	}
	in4, ok := sa.(*unix.SockaddrInet4)
	if !ok || in4.Addr != [4]byte{127, 0, 0, 1} || in4.Port == 0 {
		t.Fatalf("the SCTP socket is bound to %#v, want 127.0.0.1 at a selected port", sa)
	}
	for _, opt := range []struct {
		name       string
		level, opt int
		want       int
	}{
		{"SO_TYPE", unix.SOL_SOCKET, unix.SO_TYPE, unix.SOCK_STREAM},
		{"SO_PROTOCOL", unix.SOL_SOCKET, unix.SO_PROTOCOL, unix.IPPROTO_SCTP},
		{"SO_ACCEPTCONN", unix.SOL_SOCKET, unix.SO_ACCEPTCONN, 1},
	} {
		got, err := unix.GetsockoptInt(fd, opt.level, opt.opt)
		if err != nil {
			t.Fatalf("%s of the SCTP socket: %v", opt.name, err)
		}
		if got != opt.want {
			t.Fatalf("%s of the SCTP socket = %d, want %d", opt.name, got, opt.want)
		}
	}
	if _, err := unix.Getpeername(fd); err == nil {
		t.Fatal("the listening SCTP socket has a peer")
	}
	t.Logf("constructed a listening one-to-one SCTP socket at 127.0.0.1:%d", in4.Port)
	return f, uint16(in4.Port), true
}

// TestImportListenerRefusesStreamThatIsNotTCP checks, on Linux, an inherited listener that passes every check of a
// TCP listener's kind, an IPv4 stream socket bound to exactly 127.0.0.1 at the expected port, listening, without a
// peer, and yet is not TCP: a one-to-one SCTP socket. The worker's relay speaks TCP to it, so the import must refuse
// it by its transport protocol, return no listener, and consume the file. Where the kernel cannot create such a
// socket the case logs that it was not constructed and asserts nothing.
func TestImportListenerRefusesStreamThatIsNotTCP(t *testing.T) {
	tests := map[string]struct {
		fixture func(t *testing.T) (*os.File, uint16, bool)
	}{
		"error: listening one-to-one SCTP socket on the IPv4 loopback": {fixture: sctpListenerFile},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, port, ok := tt.fixture(t)
			if !ok {
				return
			}
			l, err := workerlink.ImportListener(f, port)
			if l != nil {
				t.Cleanup(func() { _ = l.Close() })
				t.Errorf("ImportListener returned listener %v (%T); a stream socket whose protocol is not TCP must be refused", l.Addr(), l)
			}
			if err == nil {
				t.Fatal("ImportListener accepted a listening SCTP socket")
			}
			checkConsumed(t, f)
		})
	}
}
