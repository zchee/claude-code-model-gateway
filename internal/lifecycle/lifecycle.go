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

// Package lifecycle coordinates gateway operations and records lifecycle evidence.
// The caller supplies one explicit StateDir for both Lock and Files. Normally it is
// the fixed gateway home (no environment override); a shadow instance's root is
// the caller's decision.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// ProcessState distinguishes a confirmed absence from an inconclusive lookup.
type ProcessState uint8

const (
	// Unknown means the lookup failed and cannot authorize removal.
	Unknown ProcessState = iota
	// Alive includes a process that exists but cannot be signaled due to permissions.
	Alive
	// Absent means the lookup confirmed that no process has this PID.
	Absent
)

// ProcessSource reports liveness and the legacy process birth text (POSIX ps lstart in
// local time, including space-padded day), empty when unreadable. Lookup honors ctx.
type ProcessSource interface {
	Lookup(ctx context.Context, pid int) (ProcessState, string)
}

// Owner is the legacy directory owner's record. A nil birth identity was not a string.
type Owner struct {
	Token     string  `json:"token"`
	PID       int     `json:"pid"`
	StartedAt *string `json:"startedAt"`
	Host      string  `json:"host"`
}

// Stale reports whether a local owner's process is absent or its PID was reused.
func Stale(ctx context.Context, owner *Owner, host string, processes ProcessSource) bool {
	if processes == nil || owner == nil || owner.Host != host || owner.PID <= 0 || owner.PID > math.MaxInt32 || owner.StartedAt == nil {
		return false
	}
	state, birth := processes.Lookup(ctx, owner.PID)
	return staleBirth(*owner.StartedAt, state, birth)
}

func staleBirth(recorded string, state ProcessState, birth string) bool {
	return state == Absent || state == Alive && birth != "" && birth != recorded
}

type nativeProcesses struct{ probe procinfo.Probe }

func (p nativeProcesses) Lookup(ctx context.Context, pid int) (ProcessState, string) {
	if pid <= 0 || pid > math.MaxInt32 {
		return Unknown, ""
	}
	birth, err := p.probe.Birth(ctx, pid)
	switch {
	case err == nil:
		return Alive, procinfo.LegacyStart(birth)
	case errors.Is(err, fs.ErrNotExist):
		return Absent, ""
	default:
		return Unknown, ""
	}
}

// Lock describes the lifecycle.lock under StateDir. A nil Timeout uses fifteen seconds.
// Nil Now, Wait and NewToken select wall time, a cancellable timer and a random UUID.
type Lock struct {
	StateDir  string
	PID       int
	Host      string
	Processes ProcessSource
	Timeout   *time.Duration
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
	NewToken  func() string
}

type ownershipKey struct{}

type lease struct {
	stateDir string
	host     string
	pid      int
	mu       sync.Mutex
	closed   bool
	nested   sync.WaitGroup
}

func (l Lock) busy() error {
	return fmt.Errorf("gateway lifecycle is busy or ownership is unknown (%s); retry recover or run doctor", filepath.Join(l.StateDir, "lifecycle.lock"))
}

// With runs operation under the lock. The inherited context admits nested callbacks
// for this state directory and owner only until the outer callback returns. Later
// calls are refused, and release waits for every admitted callback to finish.
// A nested callback must not wait for the outer With to return.
func (l Lock) With(ctx context.Context, operation func(context.Context) error) error {
	if held, ok := ctx.Value(ownershipKey{}).(*lease); ok {
		if held.stateDir != l.StateDir || held.pid != l.PID || held.host != l.Host {
			return errors.New("lifecycle context belongs to a different lock owner or state directory")
		}
		held.mu.Lock()
		if held.closed {
			held.mu.Unlock()
			return l.busy()
		}
		held.nested.Add(1)
		held.mu.Unlock()
		defer held.nested.Done()
		return operation(ctx)
	}
	release, err := l.Acquire(ctx)
	if err != nil {
		return err
	}
	held := &lease{stateDir: l.StateDir, pid: l.PID, host: l.Host}
	defer func() {
		held.mu.Lock()
		held.closed = true
		held.mu.Unlock()
		held.nested.Wait()
		release()
	}()
	return operation(context.WithValue(ctx, ownershipKey{}, held))
}

func clock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}
