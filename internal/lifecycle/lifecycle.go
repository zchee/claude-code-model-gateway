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
	"io/fs"
	"sync/atomic"
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
	if owner == nil || owner.Host != host || owner.PID <= 0 || owner.StartedAt == nil {
		return false
	}
	state, birth := processes.Lookup(ctx, owner.PID)
	return state == Absent || state == Alive && birth != "" && birth != *owner.StartedAt
}

type nativeProcesses struct{ probe procinfo.Probe }

func (p nativeProcesses) Lookup(ctx context.Context, pid int) (ProcessState, string) {
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

// Acquire atomically installs a prepared directory and returns a token-specific release.
func (l Lock) Acquire(ctx context.Context) (func(), error) {
	if l.Processes == nil {
		l.Processes = nativeProcesses{procinfo.New(nil)}
	}
	state, birth := l.Processes.Lookup(ctx, l.PID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state != Alive || birth == "" {
		return nil, errors.New("could not establish lifecycle lock process identity; run doctor")
	}
	return l.acquire(ctx, birth)
}

type ownershipKey struct{ stateDir string }

// With runs operation under the lock, permitting reentry only during its active context.
func (l Lock) With(ctx context.Context, operation func(context.Context) error) error {
	key := ownershipKey{l.StateDir}
	if active, ok := ctx.Value(key).(*atomic.Bool); ok && active.Load() {
		return operation(ctx)
	}
	release, err := l.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	return operation(context.WithValue(ctx, key, &active))
}

func clock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}
