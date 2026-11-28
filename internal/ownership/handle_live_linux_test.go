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

package ownership

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// handleTReferencePath is the import path of the package itself. A copy of the package made for a deliberately wrong
// version has another path, and its live guard never forwards.
const handleTReferencePath = "github.com/zchee/claude-code-model-gateway/internal/ownership"

// handleTIsReference reports whether this is the package itself rather than a copy of it.
func handleTIsReference() bool { return reflect.TypeFor[Handle]().PkgPath() == handleTReferencePath }

// handleTPidfdTarget returns the process number the pidfd fd names, from the kernel's own description of the
// descriptor, or -1 when fd is not a pidfd of a process that still holds its number or the description is unreadable.
func handleTPidfdTarget(fd int) int {
	f, err := os.Open(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return -1
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "Pid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return -1
			}
			return pid
		}
	}
	if scanner.Err() != nil {
		return -1
	}
	return -1
}

// handleTSameFile reports whether descriptors a and b of this process refer to one open file description.
func handleTSameFile(a, b int) bool {
	self := uintptr(os.Getpid())
	const kcmpFile = 0
	r, _, errno := unix.Syscall6(unix.SYS_KCMP, self, self, kcmpFile, uintptr(a), uintptr(b), 0)
	return errno == 0 && r == 0
}

// HandleTFixture asks the kernel, without the package, what it offers for the held child, a process the row started
// and has not reaped. It returns the backend a correct handle must choose ("pidfd", "ENOSYS" or "EPERM"), or an error
// naming the capability the live rows lack on this host, which is not a handle failure: a pidfd the descriptor
// description names, kcmp to compare descriptors, and a pidfd_send_signal of signal 0 (no signal is delivered); or,
// under ENOSYS, a kill of signal 0. In a copy of the package it refuses before making any call.
func HandleTFixture(held int) (string, error) {
	if !handleTIsReference() {
		return "", errors.New("not the package itself: the live rows run with the reference alone")
	}
	fd, err := unix.PidfdOpen(held, 0)
	for errors.Is(err, unix.EINTR) {
		fd, err = unix.PidfdOpen(held, 0)
	}
	switch {
	case errors.Is(err, unix.ENOSYS):
		if err := unix.Kill(held, 0); err != nil {
			return "", fmt.Errorf("kill of the held child with signal 0: %w", err)
		}
		return "ENOSYS", nil
	case errors.Is(err, unix.EPERM):
		return "EPERM", nil
	case err != nil:
		return "", fmt.Errorf("pidfd_open of the held child: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if target := handleTPidfdTarget(fd); target != held {
		return "", fmt.Errorf("the descriptor description of a pidfd of the held child %d names %d", held, target)
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("duplicate a pidfd: %w", err)
	}
	defer func() { _ = unix.Close(dup) }()
	if !handleTSameFile(fd, dup) {
		return "", errors.New("kcmp cannot tell that a duplicate is the same descriptor")
	}
	if err := unix.PidfdSendSignal(dup, 0, nil, 0); err != nil {
		return "", fmt.Errorf("pidfd_send_signal of signal 0 to the held child: %w", err)
	}
	return "pidfd", nil
}

// HandleTGuard confines the package's three system calls to the held child of one live row, a process the row
// started and has not reaped, from HandleTHoldChild until Stop. While it forwards it makes a real call only for that
// child: pidfd_open of its number with no flags; pidfd_send_signal of SIGTERM with no siginfo and no flags, through a
// duplicate the guard keeps of a descriptor it opened and checked, and only when the caller's descriptor is still that
// description; kill of its number with SIGTERM. Every other call, and every call after Stop, is recorded as refused
// and answered EPERM without being made. A copy of the package never forwards.
type HandleTGuard struct {
	held int

	mu         sync.Mutex
	forwarding bool
	stopped    bool
	trace      []string
	// issued has one entry per descriptor the guard handed out, in order, each with the private duplicate the guard
	// keeps of it. A number that is closed and handed out again gets an entry of its own; none is ever overwritten.
	issued []handleTIssue
}

// handleTIssue is one descriptor the guard handed out and the private duplicate it keeps of that descriptor.
type handleTIssue struct {
	fd, dup int
}

// current returns the private duplicate of the issuance that fd still is, if any: the latest one whose description
// fd still refers to.
func (g *HandleTGuard) current(fd int) (int, bool) {
	for _, e := range slices.Backward(g.issued) {
		if e.fd == fd && handleTSameFile(fd, e.dup) {
			return e.dup, true
		}
	}
	return -1, false
}

// HandleTHoldChild installs the guard for held until the row ends; the row calls Stop before it reaps the child.
func HandleTHoldChild(t *testing.T, held int) *HandleTGuard {
	t.Helper()
	g := &HandleTGuard{held: held, forwarding: handleTIsReference()}
	open, send, signal := pidfdOpen, pidfdSendSignal, kill
	pidfdOpen, pidfdSendSignal, kill = g.open, g.send, g.kill
	t.Cleanup(func() {
		g.Stop()
		pidfdOpen, pidfdSendSignal, kill = open, send, signal
	})
	return g
}

func (g *HandleTGuard) note(call string, err error) {
	if err != nil {
		call += ": " + err.Error()
	}
	g.trace = append(g.trace, call)
}

func (g *HandleTGuard) open(pid, flags int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.forwarding || pid != g.held || flags != 0 {
		g.note(fmt.Sprintf("refused pidfd_open(%s, %d)", g.who(pid), flags), nil)
		return -1, unix.EPERM
	}
	fd, err := unix.PidfdOpen(g.held, 0)
	if errors.Is(err, unix.EINTR) {
		// Leading interrupted attempts are the handle's to retry; only answered opens are traced.
		return -1, err
	}
	if err != nil {
		g.note("pidfd_open(held, 0)", err)
		return -1, err
	}
	if target := handleTPidfdTarget(fd); target != g.held {
		_ = unix.Close(fd)
		g.note("pidfd_open(held, 0)", fmt.Errorf("the descriptor names %d, not the held child", target))
		return -1, unix.EPERM
	}
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(fd)
		g.note("pidfd_open(held, 0)", err)
		return -1, err
	}
	g.issued = append(g.issued, handleTIssue{fd: fd, dup: dup})
	g.note("pidfd_open(held, 0)", nil)
	return fd, nil
}

func (g *HandleTGuard) send(fd int, sig syscall.Signal, info *unix.Siginfo, flags int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	dup, ok := g.current(fd)
	label := "other"
	if ok {
		label = "issued"
	}
	call := fmt.Sprintf("pidfd_send_signal(%s, %v, %s, flags=%d)", label, sig, handleTInfo(info), flags)
	if !g.forwarding || label != "issued" || sig != syscall.SIGTERM || info != nil || flags != 0 || handleTPidfdTarget(dup) != g.held {
		g.note("refused "+call, nil)
		return unix.EPERM
	}
	err := unix.PidfdSendSignal(dup, syscall.SIGTERM, nil, 0)
	g.note(call, err)
	return err
}

func (g *HandleTGuard) kill(pid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	call := fmt.Sprintf("kill(%s, %v)", g.who(pid), sig)
	if !g.forwarding || pid != g.held || sig != syscall.SIGTERM {
		g.note("refused "+call, nil)
		return unix.EPERM
	}
	err := unix.Kill(g.held, syscall.SIGTERM)
	g.note(call, err)
	return err
}

func (g *HandleTGuard) who(pid int) string {
	if pid == g.held {
		return "held"
	}
	return "other"
}

// Stop ends the forwarding: from here on every call is refused. It runs while the held child is still unreaped, after
// the row's handle operations and the handle's Close: a descriptor the handle left open that is still the description
// the guard issued is closed now, and so is every duplicate the guard kept. It returns the calls traced, and is
// idempotent.
func (g *HandleTGuard) Stop() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.stopped {
		g.stopped, g.forwarding = true, false
		for _, e := range g.issued {
			if handleTSameFile(e.fd, e.dup) {
				_ = unix.Close(e.fd)
				g.note("leaked descriptor closed", nil)
			}
			_ = unix.Close(e.dup)
		}
		g.issued = nil
	}
	return slices.Clone(g.trace)
}
