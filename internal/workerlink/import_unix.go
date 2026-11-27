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

//go:build unix

package workerlink

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// importListener checks that file is a loopback IPv4 stream socket bound to exactly 127.0.0.1 at expected, with no
// peer and, where the system can say so, listening, and returns a listener over its own duplicate. The caller closes
// file.
func importListener(file *os.File, expected uint16) (net.Listener, error) {
	if expected == 0 {
		return nil, errors.New("the expected worker port is zero")
	}
	err := inspectInherited(file, func(fd int) error {
		if err := streamSocket(fd); err != nil {
			return err
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			return os.NewSyscallError("getsockname", err)
		}
		in4, ok := sa.(*unix.SockaddrInet4)
		if !ok {
			return errors.New("not an IPv4 socket")
		}
		if _, err := loopbackPort(&net.TCPAddr{IP: net.IP(in4.Addr[:]), Port: in4.Port}, expected); err != nil {
			return err
		}
		switch _, err := unix.Getpeername(fd); {
		case err == nil:
			return errors.New("a connected stream, not a listener")
		case !errors.Is(err, unix.ENOTCONN):
			return os.NewSyscallError("getpeername", err)
		}
		listening, known, err := listeningState(fd)
		if err != nil {
			return err
		}
		if known && !listening {
			return errors.New("a socket that is not listening")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	l, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// importDrainChannel checks that file is a connected local stream socket and returns a connection over its own
// duplicate. The caller closes file.
func importDrainChannel(file *os.File) (net.Conn, error) {
	err := inspectInherited(file, func(fd int) error {
		if err := streamSocket(fd); err != nil {
			return err
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			return os.NewSyscallError("getsockname", err)
		}
		if _, ok := sa.(*unix.SockaddrUnix); !ok {
			return errors.New("not a local socket")
		}
		// A listening or unconnected local socket has no peer: ENOTCONN on Linux and on macOS (xnu
		// bsd/kern/uipc_syscalls.c:3239-3243). A peer that only shut down its writing side still answers there; only a
		// socket shut down in both directions is refused with EINVAL (:3232-3237), and it is of no use as a channel.
		if _, err := unix.Getpeername(fd); err != nil {
			if errors.Is(err, unix.ENOTCONN) {
				return errors.New("a local stream socket that is not connected")
			}
			return os.NewSyscallError("getpeername", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// inspectInherited marks the inherited descriptor close-on-exec, then runs check on it. An inherited descriptor
// arrives with close-on-exec cleared; it is marked under syscall.ForkLock, so that no process this one starts from
// then on inherits it, before anything else is done with it. net.FileListener and net.FileConn then duplicate it
// with F_DUPFD_CLOEXEC, so the imported handle is close-on-exec too.
func inspectInherited(file *os.File, check func(fd int) error) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var checkErr error
	if err := raw.Control(func(fd uintptr) {
		if checkErr = markCloseOnExec(int(fd)); checkErr == nil {
			checkErr = check(int(fd))
		}
	}); err != nil {
		return err
	}
	return checkErr
}

// markCloseOnExec adds FD_CLOEXEC to the descriptor's flags and keeps any other descriptor flag it has.
func markCloseOnExec(fd int) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return os.NewSyscallError("fcntl", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return os.NewSyscallError("fcntl", err)
	}
	return nil
}

// streamSocket refuses a descriptor that is not a socket, or a socket that is not of the stream type.
func streamSocket(fd int) error {
	typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return fmt.Errorf("not a usable socket: %w", os.NewSyscallError("getsockopt", err))
	}
	if typ != unix.SOCK_STREAM {
		return fmt.Errorf("a socket of type %d, not a stream", typ)
	}
	return nil
}
