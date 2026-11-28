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

//go:build unix && !linux

package ownership

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// handleTModel stands in for kill, the only system call through which a handle of this platform signals a process:
// every call is recorded and answered with killErr without being made, so nothing a handle does under it reaches a
// real process. This platform opens nothing.
type handleTModel struct {
	killErr error

	mu         sync.Mutex
	deliveries []string
}

func (m *handleTModel) kill(pid int, sig syscall.Signal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries = append(m.deliveries, fmt.Sprintf("kill(%d, %v)", pid, sig))
	return m.killErr
}

// calls returns the opens, of which this platform has none, and the deliveries recorded so far.
func (m *handleTModel) calls() (opens, deliveries []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, slices.Clone(m.deliveries)
}

// handleTInstallModel puts m in place of kill until the row ends. A test that calls it must not run in parallel with
// any other test of the package.
func handleTInstallModel(t *testing.T, m *handleTModel) *handleTModel {
	t.Helper()
	signal := kill
	kill = m.kill
	t.Cleanup(func() { kill = signal })
	return m
}

// handleTRefused records every call the test binary's refusing default received, for diagnostics.
var handleTRefused struct {
	mu    sync.Mutex
	calls []string
}

// init makes kill refuse in this test binary: it records the call and answers EPERM without making it. A row that
// needs it installs the model, or the live guard for its held child; whatever is "restored" after a row is this
// refusal, never the real call.
func init() { //nolint:gochecknoinits // The refusal must be in place before any test of the binary runs.
	kill = func(pid int, sig syscall.Signal) error {
		handleTRefused.mu.Lock()
		defer handleTRefused.mu.Unlock()
		handleTRefused.calls = append(handleTRefused.calls, fmt.Sprintf("kill(%d, %v)", pid, sig))
		return unix.EPERM
	}
}

// TestHandleSeamsRefuseByDefault decides that in this test binary kill refuses unless a row installs something else.
// The number names no process a kernel could reach.
func TestHandleSeamsRefuseByDefault(t *testing.T) {
	tests := map[string]struct {
		want string
	}{
		"error: kill refuses by default": {want: "kill(1073741824, terminated)"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTRefused.mu.Lock()
			before := len(handleTRefused.calls)
			handleTRefused.mu.Unlock()
			err := kill(handleTVirtualPID, syscall.SIGTERM)
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
