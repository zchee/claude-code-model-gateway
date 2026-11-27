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

package serveworker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// controlDescriptors are the supervised worker's report and liveness pipes and the listener and drain channel its
// supervisor reserved for its generation.
type controlDescriptors struct {
	generation string
	reportFile *os.File
	live       *os.File
	listener   net.Listener
	drain      net.Conn
	closeOnce  sync.Once
}

// supervisedDescriptors validates and adopts the supervised handoff. It returns nil when the worker is not supervised:
// MODEL_GATEWAY_CONTROL_FDS alone selects the supervised mode, the listener and drain settings never do.
//
// Every setting is checked before any descriptor is adopted: the report and liveness numbers as before, then the
// generation, then the listener and drain numbers, which must be canonical decimal numbers above the standard
// streams, distinct from each other and from the report and liveness numbers, open, and four different endpoints
// rather than one endpoint under two numbers. A descriptor that fails these checks is never used on trust, as its
// number may since have been reused by an unrelated descriptor, and there is no fallback to a listener of the worker's
// own. Once all four are checked the worker owns them: a later failure, such as a listener of the wrong kind or at
// another port, closes every one of them, the ones already imported included.
func supervisedDescriptors(lookup config.LookupEnv) (*controlDescriptors, error) {
	raw, ok := lookup(controlFDsKey)
	if !ok {
		return nil, nil
	}
	first, second, found := strings.Cut(raw, ",")
	if !found {
		return nil, fmt.Errorf("model-gateway: %s must name the report and liveness descriptors as two numbers, such as 3,4", controlFDsKey)
	}
	report, err := descriptor(first, unix.O_WRONLY, "report")
	if err != nil {
		return nil, err
	}
	live, err := descriptor(second, unix.O_RDONLY, "liveness")
	if err != nil {
		return nil, err
	}
	if report == live {
		return nil, fmt.Errorf("model-gateway: %s names descriptor %d for both the report and liveness pipes", controlFDsKey, report)
	}
	generation, _ := lookup(generationKey)
	if generation == "" {
		return nil, fmt.Errorf("model-gateway: a supervised worker needs %s; refusing to start without its generation", generationKey)
	}
	if _, err := reportLine(0, generation); err != nil {
		return nil, err
	}
	listener, err := handoffNumber(lookup, workerlink.ListenerFDEnv, "listener")
	if err != nil {
		return nil, err
	}
	drain, err := handoffNumber(lookup, workerlink.DrainFDEnv, "drain channel")
	if err != nil {
		return nil, err
	}
	slots := []handoffSlot{{"report", report}, {"liveness", live}, {"listener", listener}, {"drain channel", drain}}
	if err := distinctSlots(slots); err != nil {
		return nil, err
	}
	port, err := workerPort(lookup)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		return nil, errors.New("model-gateway: a supervised worker needs CODEX_GATEWAY_WORKER_PORT, the port of the listener its supervisor reserved")
	}

	// From here on the four descriptors are the worker's: every path below closes each of them or hands it on. The
	// imports consume their file on success and on failure alike.
	c := &controlDescriptors{generation: generation}
	closeRaw := func(fds ...int) {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}
	if c.reportFile, err = openDescriptor(report, "port report"); err != nil {
		closeRaw(report, live, listener, drain)
		return nil, err
	}
	if c.live, err = openDescriptor(live, "parent liveness"); err != nil {
		c.close()
		closeRaw(live, listener, drain)
		return nil, err
	}
	if c.listener, err = workerlink.ImportListener(os.NewFile(uintptr(listener), "inherited listener"), uint16(port)); err != nil {
		c.close()
		closeRaw(drain)
		return nil, fmt.Errorf("model-gateway: the inherited listener descriptor %d is unusable: %w", listener, err)
	}
	if c.drain, err = workerlink.ImportDrainChannel(os.NewFile(uintptr(drain), "inherited drain channel")); err != nil {
		c.close()
		return nil, fmt.Errorf("model-gateway: the inherited drain channel descriptor %d is unusable: %w", drain, err)
	}
	return c, nil
}

// handoffSlot is one descriptor of the supervised handoff with the role a diagnostic names it by.
type handoffSlot struct {
	role string
	fd   int
}

// handoffNumber parses the descriptor number one handoff setting names: a canonical unsigned decimal number above the
// standard streams.
func handoffNumber(lookup config.LookupEnv, name, role string) (int, error) {
	spelling, ok := lookup(name)
	if !ok {
		return 0, fmt.Errorf("model-gateway: a supervised worker needs %s; the inherited %s descriptor is missing", name, role)
	}
	fd, err := strconv.Atoi(spelling)
	if err != nil || fd < 0 || strconv.Itoa(fd) != spelling {
		return 0, fmt.Errorf("model-gateway: %s has a malformed %s descriptor %q; want an unsigned decimal number", name, role, spelling)
	}
	if fd <= 2 {
		return 0, fmt.Errorf("model-gateway: %s names standard stream %d as the %s descriptor; the worker never uses stdin, stdout or stderr for it", name, fd, role)
	}
	return fd, nil
}

// distinctSlots refuses a handoff in which two roles share a descriptor number or two numbers refer to one endpoint,
// judged by the device and inode fstat reports. Linux gives every socket an inode of its own, and both ends of one
// pipe share theirs, so a duplicate under another number is found. macOS gives each pipe end and each local socket
// its own inode, but reports device and inode zero for every IP socket (xnu bsd/kern/sys_socket.c soo_stat,
// bsd/kern/uipc_socket2.c pru_sense_null): there two IP sockets always compare equal. A valid handoff has two pipes,
// one TCP listener and one local socket, which compare distinct on both systems; only a handoff that passes an IP
// socket in a second role is refused there as an alias, which it is unless it is a second TCP socket, itself invalid.
func distinctSlots(slots []handoffSlot) error {
	type identity struct {
		dev uint64
		ino uint64
	}
	ids := make([]identity, len(slots))
	for i, s := range slots {
		for _, other := range slots[:i] {
			if other.fd == s.fd {
				return fmt.Errorf("model-gateway: the %s and %s settings name the same descriptor %d", other.role, s.role, s.fd)
			}
		}
		var st unix.Stat_t
		if err := unix.Fstat(s.fd, &st); err != nil {
			return fmt.Errorf("model-gateway: the %s descriptor %d is unusable: %w", s.role, s.fd, err)
		}
		ids[i] = identity{uint64(st.Dev), uint64(st.Ino)} //nolint:gosec,unconvert // Stat_t.Dev is signed on some platforms.
	}
	for i, s := range slots {
		for j, other := range slots[:i] {
			if ids[j] == ids[i] {
				return fmt.Errorf("model-gateway: the %s descriptor %d and the %s descriptor %d are the same endpoint under two numbers", other.role, other.fd, s.role, s.fd)
			}
		}
	}
	return nil
}

// descriptor parses and checks one inherited descriptor. access is the direction the worker uses it in.
func descriptor(spelling string, access int, role string) (int, error) {
	fd, err := strconv.Atoi(spelling)
	if err != nil || fd < 0 || strconv.Itoa(fd) != spelling {
		return 0, fmt.Errorf("model-gateway: %s has a malformed %s descriptor %q", controlFDsKey, role, spelling)
	}
	if fd <= 2 {
		return 0, fmt.Errorf("model-gateway: %s names standard stream %d as the %s descriptor; the worker never uses stdin, stdout or stderr for it", controlFDsKey, fd, role)
	}
	if err := checkDescriptor(fd, access); err != nil {
		return 0, fmt.Errorf("model-gateway: %s %s descriptor %d is unusable: %w", controlFDsKey, role, fd, err)
	}
	return fd, nil
}

func checkDescriptor(fd, access int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if kind := st.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		return errors.New("not a pipe or socket")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if mode := flags & unix.O_ACCMODE; mode != access && mode != unix.O_RDWR {
		return errors.New("open in the wrong direction")
	}
	return nil
}

// openDescriptor takes ownership of a checked descriptor. It is made close-on-exec and non-blocking, so that closing
// the returned file ends a read that is waiting on it.
func openDescriptor(fd int, name string) (*os.File, error) {
	// Marked with forks locked out, so that no process this one starts meanwhile inherits it.
	syscall.ForkLock.RLock()
	unix.CloseOnExec(fd)
	syscall.ForkLock.RUnlock()
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("model-gateway: could not prepare the %s descriptor %d: %w", name, fd, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// report writes the single listening line and closes the report pipe.
func (c *controlDescriptors) report(port int) error {
	line, err := reportLine(port, c.generation)
	if err == nil {
		_, err = c.reportFile.Write(line)
	}
	if closeErr := c.reportFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("model-gateway: could not report the worker port to the supervisor: %w", err)
	}
	return nil
}

// close closes every descriptor of the handoff that is still open; the listener is closed by its servers first.
func (c *controlDescriptors) close() {
	c.closeOnce.Do(func() {
		for _, f := range []*os.File{c.reportFile, c.live} {
			if f != nil {
				_ = f.Close()
			}
		}
		if c.listener != nil {
			_ = c.listener.Close()
		}
		if c.drain != nil {
			_ = c.drain.Close()
		}
	})
}

// fixtureChannel returns the test harness's channel descriptor when this worker must stop at its end. Outside the
// test guard the descriptor is closed unread and nil is returned.
func fixtureChannel(lookup config.LookupEnv) (*os.File, error) {
	raw, ok := lookup(nodeChannelKey)
	if !ok {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd <= 2 || strconv.Itoa(fd) != raw {
		if guarded(lookup) {
			return nil, fmt.Errorf("model-gateway: %s names an unusable descriptor %q", nodeChannelKey, raw)
		}
		return nil, nil
	}
	if err := checkDescriptor(fd, unix.O_RDONLY); err != nil {
		if guarded(lookup) {
			return nil, fmt.Errorf("model-gateway: %s descriptor %d is unusable: %w", nodeChannelKey, fd, err)
		}
		return nil, nil
	}
	file, err := openDescriptor(fd, "fixture channel")
	if err != nil {
		return nil, err
	}
	if !guarded(lookup) {
		_ = file.Close()
		return nil, nil
	}
	return file, nil
}

// errorCode returns the system error name of a failure, as the source prints error.code.
func errorCode(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return err.Error()
}

// acceptQueued accepts the connections already queued on ln through its descriptor, which is non-blocking, until the
// accept would block. It never waits for a connection, ignores the listener's deadline and changes nothing about the
// listening socket, which the supervisor's copy shares. An immediate stop (now closed) is checked before every accept;
// it ends the sweep and closes what the sweep took.
func acceptQueued(ln net.Listener, now <-chan struct{}) ([]net.Conn, error) {
	sc, ok := ln.(syscall.Conn)
	if !ok {
		return nil, errors.New("the worker's listener has no descriptor for its final sweep")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var conns []net.Conn
	var acceptErr error
	controlErr := raw.Control(func(fd uintptr) {
		for {
			select {
			case <-now:
				for _, conn := range conns {
					_ = conn.Close()
				}
				conns = nil
				return
			default:
			}
			// The accepted descriptor is marked close-on-exec with forks locked out, as no process this one starts may
			// inherit it.
			syscall.ForkLock.RLock()
			n, _, err := unix.Accept(int(fd))
			if err == nil {
				unix.CloseOnExec(n)
			}
			syscall.ForkLock.RUnlock()
			switch {
			case errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED):
				continue
			case errors.Is(err, unix.EAGAIN):
				return
			case err != nil:
				acceptErr = os.NewSyscallError("accept", err)
				return
			}
			f := os.NewFile(uintptr(n), "swept connection")
			conn, err := net.FileConn(f)
			_ = f.Close()
			if err != nil {
				acceptErr = err
				return
			}
			conns = append(conns, conn)
		}
	})
	return conns, errors.Join(controlErr, acceptErr)
}

// parentEnded reports, without waiting, whether the liveness pipe reached end of file. It reads the non-blocking
// descriptor directly until the read would block or ends: the supervisor writes nothing there that the worker uses,
// end of file stays end of file for a watcher reading the pipe at the same time, and the direct read does not wait
// for the watcher's read. A descriptor that cannot be read counts as ended.
func parentEnded(live *os.File) bool {
	raw, err := live.SyscallConn()
	if err != nil {
		return true
	}
	ended := true
	_ = raw.Control(func(fd uintptr) {
		var buf [512]byte
		for {
			n, err := unix.Read(int(fd), buf[:])
			switch {
			case errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.EAGAIN):
				ended = false
				return
			case err != nil || n == 0:
				return
			}
		}
	})
	return ended
}
