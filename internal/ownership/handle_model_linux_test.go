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

//go:build linux

package ownership

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// handleTAnswer is one scripted answer of pidfd_open: issue hands out a fresh descriptor the test owns; otherwise err
// is the answer.
type handleTAnswer struct {
	issue bool
	err   error
}

// handleTDescriptor is a descriptor the model handed out: its number and the identity of the file behind it, so that
// a later look can tell it from another file that reused the number.
type handleTDescriptor struct {
	fd       int
	identity string
}

// handleTModel stands in for every system call through which a handle opens or signals a process. Opens are answered
// from a script, and deliveries are recorded and answered without being forwarded, so nothing a handle does under it
// reaches a real process. A descriptor it hands out is a temporary file the test opened; it is never a process's
// pidfd.
type handleTModel struct {
	// answers are the scripted pidfd_open answers, used in order; a call beyond them is answered EINVAL.
	answers []handleTAnswer
	// sendErr and killErr answer every recorded pidfd_send_signal and kill call.
	sendErr, killErr error

	t          *testing.T
	mu         sync.Mutex
	opens      []string
	deliveries []string
	issued     []handleTDescriptor
}

func (m *handleTModel) open(pid, flags int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens = append(m.opens, fmt.Sprintf("pidfd_open(%d, %d)", pid, flags))
	if len(m.answers) == 0 {
		return -1, fmt.Errorf("pidfd_open called more often than scripted: %w", syscall.EINVAL)
	}
	a := m.answers[0]
	m.answers = m.answers[1:]
	if !a.issue {
		return -1, a.err
	}
	d, err := handleTIssueDescriptor(m.t)
	if err != nil {
		return -1, err
	}
	m.issued = append(m.issued, d)
	return d.fd, nil
}

// label names a descriptor in a recorded call by the order the model issued it, so that rows compare literals.
func (m *handleTModel) label(fd int) string {
	for i, d := range m.issued {
		if d.fd == fd && handleTDescriptorIs(d) {
			return fmt.Sprintf("descriptor %d", i+1)
		}
	}
	return fmt.Sprintf("descriptor %d not issued by the model", fd)
}

// send records the descriptor, the signal and the call's scope: a process-directed send has no siginfo and no flags.
func (m *handleTModel) send(fd int, sig syscall.Signal, info *unix.Siginfo, flags int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries = append(m.deliveries, fmt.Sprintf("pidfd_send_signal(%s, %v, %s, flags=%d)", m.label(fd), sig, handleTInfo(info), flags))
	return m.sendErr
}

// handleTInfo names a pidfd_send_signal siginfo argument.
func handleTInfo(info *unix.Siginfo) string {
	if info == nil {
		return "info=nil"
	}
	return "info=set"
}

func (m *handleTModel) kill(pid int, sig syscall.Signal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries = append(m.deliveries, fmt.Sprintf("kill(%d, %v)", pid, sig))
	return m.killErr
}

// calls returns the opens and the deliveries recorded so far.
func (m *handleTModel) calls() (opens, deliveries []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.opens), slices.Clone(m.deliveries)
}

// open reports whether the n-th issued descriptor (from 1) is still open and still the file the model issued.
func (m *handleTModel) descriptorOpen(n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return n >= 1 && n <= len(m.issued) && handleTDescriptorIs(m.issued[n-1])
}

// handleTInstallModel puts m in place of every open and delivery system call of the package until the row ends, and
// closes in cleanup every descriptor it issued that is still the file it issued, whatever the handle did with it. A
// test that calls it must not run in parallel with any other test of the package.
func handleTInstallModel(t *testing.T, m *handleTModel) *handleTModel {
	t.Helper()
	m.t = t
	t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, d := range m.issued {
			handleTReleaseDescriptor(d)
		}
	})
	t.Cleanup(handleTInstallSeams(m))
	return m
}

// handleTIdentity names the file behind fd by its device and inode, or "" when fd is not open.
func handleTIdentity(fd int) string {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// handleTIssueDescriptor opens a fresh temporary file of the row and returns its descriptor, whose number the kernel
// chose, with the file's identity.
// A descriptor whose file cannot be identified is closed at once and not handed out: cleanup could not tell it from
// a reused number later.
func handleTIssueDescriptor(t *testing.T) (handleTDescriptor, error) {
	path := filepath.Join(t.TempDir(), "issued")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Error(err)
		return handleTDescriptor{fd: -1}, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Error(err)
		return handleTDescriptor{fd: -1}, err
	}
	identity := handleTIdentity(fd)
	if identity == "" {
		_ = unix.Close(fd)
		err := fmt.Errorf("the issued descriptor %d could not be identified: %w", fd, unix.EIO)
		t.Error(err)
		return handleTDescriptor{fd: -1}, err
	}
	return handleTDescriptor{fd: fd, identity: identity}, nil
}

// handleTDescriptorIs reports whether d's number still names the file the model issued.
func handleTDescriptorIs(d handleTDescriptor) bool {
	return d.fd >= 0 && d.identity != "" && handleTIdentity(d.fd) == d.identity
}

// handleTReleaseDescriptor closes d if its number still names the issued file, and leaves a reused number alone.
func handleTReleaseDescriptor(d handleTDescriptor) {
	if handleTDescriptorIs(d) {
		_ = unix.Close(d.fd)
	}
}

// handleTInstallSeams replaces pidfd_open, pidfd_send_signal and kill with m and returns the restore.
func handleTInstallSeams(m *handleTModel) (restore func()) {
	open, send, signal := pidfdOpen, pidfdSendSignal, kill
	pidfdOpen = m.open
	pidfdSendSignal = m.send
	kill = m.kill
	return func() { pidfdOpen, pidfdSendSignal, kill = open, send, signal }
}

// handleTRefused records every call the test binary's refusing defaults received, for diagnostics.
var handleTRefused struct {
	mu    sync.Mutex
	calls []string
}

func handleTRefuse(call string) error {
	handleTRefused.mu.Lock()
	defer handleTRefused.mu.Unlock()
	handleTRefused.calls = append(handleTRefused.calls, call)
	return unix.EPERM
}

// init makes the package's three system calls refuse in this test binary: each records the call and answers EPERM
// without making it. A row that needs them installs the model, or the live guard for its held child; whatever is
// "restored" after a row is this refusal, never the real call, so a goroutine that outlives its row can reach nothing.
func init() { //nolint:gochecknoinits // The refusal must be in place before any test of the binary runs.
	pidfdOpen = func(pid, flags int) (int, error) {
		return -1, handleTRefuse(fmt.Sprintf("pidfd_open(%d, %d)", pid, flags))
	}
	pidfdSendSignal = func(fd int, sig syscall.Signal, info *unix.Siginfo, flags int) error {
		return handleTRefuse(fmt.Sprintf("pidfd_send_signal(%d, %v, %s, flags=%d)", fd, sig, handleTInfo(info), flags))
	}
	kill = func(pid int, sig syscall.Signal) error { return handleTRefuse(fmt.Sprintf("kill(%d, %v)", pid, sig)) }
}

// TestHandleSeamsRefuseByDefault decides that in this test binary the three system calls refuse unless a row
// installs something else: each answers EPERM and is recorded. The arguments name nothing a kernel could reach.
func TestHandleSeamsRefuseByDefault(t *testing.T) {
	tests := map[string]struct {
		call func() error
		want string
	}{
		"error: pidfd_open refuses by default": {
			call: func() error { _, err := pidfdOpen(handleTVirtualPID, 0); return err },
			want: "pidfd_open(1073741824, 0)",
		},
		"error: pidfd_send_signal refuses by default": {
			call: func() error { return pidfdSendSignal(-1, syscall.SIGTERM, nil, 0) },
			want: "pidfd_send_signal(-1, terminated, info=nil, flags=0)",
		},
		"error: kill refuses by default": {
			call: func() error { return kill(handleTVirtualPID, syscall.SIGTERM) },
			want: "kill(1073741824, terminated)",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTRefused.mu.Lock()
			before := len(handleTRefused.calls)
			handleTRefused.mu.Unlock()
			err := tt.call()
			handleTRefused.mu.Lock()
			got := slices.Clone(handleTRefused.calls[before:])
			handleTRefused.mu.Unlock()
			if !errors.Is(err, unix.EPERM) {
				t.Errorf("the default answered %v; want EPERM", err)
			}
			if diff := gocmp.Diff([]string{tt.want}, got); diff != "" {
				t.Errorf("refused calls (-want +got):\n%s", diff)
			}
		})
	}
}
