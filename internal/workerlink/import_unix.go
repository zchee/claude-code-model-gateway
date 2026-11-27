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

// importListener takes ownership of file and closes it on every path. It marks the descriptor close-on-exec first,
// then checks that it is a loopback IPv4 TCP stream socket bound to exactly 127.0.0.1 at expected, with no peer and,
// where the system can say so, listening, and returns a listener over its own duplicate.
func importListener(file *os.File, expected uint16) (net.Listener, error) {
	if err := adoptInherited(file); err != nil {
		return nil, err
	}
	err := inspect(file, func(fd int) error {
		if expected == 0 {
			return errors.New("the expected worker port is zero")
		}
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
		if err := tcpProtocol(fd); err != nil {
			return err
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
	var l net.Listener
	if err == nil {
		l, err = net.FileListener(file)
	}
	if closeErr := file.Close(); err == nil && closeErr != nil {
		_ = l.Close()
		err = fmt.Errorf("close the inherited descriptor: %w", closeErr)
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// importDrainChannel takes ownership of file and closes it on every path. It marks the descriptor close-on-exec
// first, then checks that it is a connected local stream socket and returns a connection over its own duplicate.
func importDrainChannel(file *os.File) (net.Conn, error) {
	if err := adoptInherited(file); err != nil {
		return nil, err
	}
	err := inspect(file, func(fd int) error {
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
	var c net.Conn
	if err == nil {
		c, err = net.FileConn(file)
	}
	if closeErr := file.Close(); err == nil && closeErr != nil {
		_ = c.Close()
		err = fmt.Errorf("close the inherited descriptor: %w", closeErr)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// adoptInherited marks the inherited descriptor close-on-exec before anything else is done with it. An inherited
// descriptor arrives with close-on-exec cleared, so the mark is made with forks locked out (syscall.ForkLock): no
// process this one starts from then on inherits it. When the mark fails the descriptor may still be inheritable, so
// forks stay locked out until it is closed; the close happens after RawConn.Control has returned, because closing
// waits for the descriptor reference that Control holds. On failure file is closed and the error returned.
// net.FileListener and net.FileConn later duplicate the marked descriptor with F_DUPFD_CLOEXEC.
func adoptInherited(file *os.File) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	err := inspect(file, markCloseOnExec)
	if err != nil {
		_ = file.Close()
	}
	return err
}

// inspect runs check on the descriptor behind file and returns the first failure.
func inspect(file *os.File, check func(fd int) error) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var checkErr error
	if err := raw.Control(func(fd uintptr) { checkErr = check(int(fd)) }); err != nil {
		return err
	}
	return checkErr
}

// markCloseOnExec adds FD_CLOEXEC to the descriptor's flags and keeps any other descriptor flag it has. The caller
// holds syscall.ForkLock.
func markCloseOnExec(fd int) error {
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
