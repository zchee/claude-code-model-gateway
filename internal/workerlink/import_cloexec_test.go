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
	"os"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// heldInheritedListener is a listening loopback IPv4 TCP socket handed over as an inherited descriptor arrives:
// close-on-exec cleared. Its descriptor outlives the file's Close until release: the test keeps a reference to the
// file inside a RawConn.Control call that waits, and the os.File of a descriptor in blocking mode is not pollable, so
// its Close returns at once and leaves the descriptor open until that last reference is dropped (Go go1.27.4
// src/os/file_unix.go newFile, src/internal/poll/fd_unix.go:91-118 Close and :55-73 Init, src/internal/poll/fd_posix.go:56
// RawControl). An import that refuses the file and closes it therefore leaves the descriptor, with whatever flags the
// import gave it, for the test to inspect.
type heldInheritedListener struct {
	file    *os.File
	fd      int
	port    uint16
	release func()
}

// holdInheritedListener creates a heldInheritedListener; release runs at cleanup at the latest.
func holdInheritedListener(t *testing.T) heldInheritedListener {
	t.Helper()
	// Created and marked under the fork lock, so that no child this test process starts meanwhile inherits it before the
	// test clears the mark on purpose.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	f := os.NewFile(uintptr(fd), "inherited-listener")
	t.Cleanup(func() { _ = f.Close() })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	if status, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err != nil || status&unix.O_NONBLOCK != 0 {
		t.Fatalf("the descriptor's status flags are %#x (%v); the fixture needs blocking mode", status, err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		t.Fatalf("F_SETFD: %v", err)
	}
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	inside, done, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- raw.Control(func(uintptr) {
			close(inside)
			<-done
		})
	}()
	await(t, inside, "the reference held on the inherited listener")
	h := heldInheritedListener{file: f, fd: fd, port: uint16(sa.(*unix.SockaddrInet4).Port)}
	h.release = sync.OnceFunc(func() {
		close(done)
		if err := <-finished; err != nil {
			t.Errorf("the held Control: %v", err)
		}
	})
	t.Cleanup(h.release)
	return h
}

// descriptorFlags returns the descriptor flags of fd, or the error of F_GETFD.
func descriptorFlags(fd int) (int, error) {
	return unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
}

// TestFixtureHeldDescriptorOutlivesClose checks heldInheritedListener: after the file's Close returned, its
// descriptor is still open, still the listener at its port and still inheritable. Its number is not examined after
// release: once the last reference is dropped the number is free, and any open in the process may reuse it.
func TestFixtureHeldDescriptorOutlivesClose(t *testing.T) {
	h := holdInheritedListener(t)
	if err := h.file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flags, err := descriptorFlags(h.fd)
	if err != nil {
		t.Fatalf("F_GETFD of descriptor %d after the file's Close: %v; the held reference must keep it open", h.fd, err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Errorf("descriptor %d is close-on-exec; the fixture must hand it over inheritable", h.fd)
	}
	sa, err := unix.Getsockname(h.fd)
	if err != nil {
		t.Fatalf("getsockname of descriptor %d after the file's Close: %v", h.fd, err)
	}
	if in4, ok := sa.(*unix.SockaddrInet4); !ok || uint16(in4.Port) != h.port {
		t.Errorf("descriptor %d is bound to %#v after the file's Close, want the listener at port %d", h.fd, sa, h.port)
	}
	h.release()
}

// TestImportListenerRefusalMarksDescriptor checks that an import marks the inherited descriptor close-on-exec before
// it checks anything, so that a refused hand-off leaves no inheritable descriptor even in the moment before the file
// is closed: a process started then would inherit it. The test keeps the descriptor open past the import's Close
// (see heldInheritedListener) and reads its flags after the refusal. A zero expected port is refused like a wrong
// one, and neither may skip the mark.
func TestImportListenerRefusalMarksDescriptor(t *testing.T) {
	tests := map[string]struct {
		expected func(port uint16) uint16
	}{
		"error: expected port is zero":    {expected: func(uint16) uint16 { return 0 }},
		"error: listener on another port": {expected: otherPort},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := holdInheritedListener(t)
			l, err := workerlink.ImportListener(h.file, tt.expected(h.port))
			if l != nil {
				_ = l.Close()
				t.Errorf("ImportListener returned listener %v; a refused hand-off must return none", l.Addr())
			}
			if err == nil {
				t.Fatal("ImportListener accepted the descriptor")
			}
			checkConsumed(t, h.file)
			switch flags, err := descriptorFlags(h.fd); {
			case errors.Is(err, unix.EBADF):
				t.Logf("descriptor %d was closed outright by the import; nothing is left to inherit", h.fd)
			case err != nil:
				t.Fatalf("F_GETFD of descriptor %d: %v", h.fd, err)
			case flags&unix.FD_CLOEXEC == 0:
				t.Errorf("descriptor %d of the refused hand-off is still inheritable (flags %#x); the import must mark it close-on-exec before any check", h.fd, flags)
			}
		})
	}
}
