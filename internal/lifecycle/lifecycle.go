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
	"encoding/json/jsontext"
	"errors"
	"io/fs"
	"time"
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
	return false
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
	return nil, errors.New("lifecycle lock not implemented")
}

// With runs operation under the lock, permitting reentry only during its active context.
func (l Lock) With(ctx context.Context, operation func(context.Context) error) error {
	return errors.New("lifecycle lock not implemented")
}

// Files locates lifecycle evidence under StateDir and optionally supplies the timestamp clock.
type Files struct {
	StateDir string
	Now      func() time.Time
	// AppendFile appends one complete journal record. Nil uses atomicfile.AppendFile.
	AppendFile func(path string, data []byte, mode fs.FileMode) error
}

// Stopped reports whether the intentional stop marker exists, without consuming it.
func (f Files) Stopped() bool { return false }

// Stop records an intentional stop.
func (f Files) Stop() error { return errors.New("stop marker not implemented") }

// Resume removes the marker if present.
func (f Files) Resume() error { return errors.New("stop marker not implemented") }

// Record appends an allowlisted lifecycle event on a best-effort basis. Below the
// trim threshold it passes the entire record, including its newline, to
// atomicfile.AppendFile(path, data, 0o666) without replacing the journal.
func (f Files) Record(event string, fields map[string]any) {}

// Records returns parseable lifecycle records in file order.
func (f Files) Records() []jsontext.Value { return nil }

// WriteStartupFailure records a supervisor failure, omitting an unspecified generation.
func (f Files) WriteStartupFailure(pid int, generation *string, message string) error {
	return errors.New("startup failure not implemented")
}

// StartupFailure returns the matching supervisor's message; an empty generation matches any.
func (f Files) StartupFailure(pid int, generation string) (string, bool) { return "", false }
