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

// Package procinfo supplies process and listener evidence to the supervisor.
package procinfo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

type probeError string

func (e probeError) Error() string { return string(e) }

// ErrUnknown means that the inspection did not establish a process identity.
const ErrUnknown = probeError("process identity unknown")

// ErrUnsupported means the operating system has no native process probe.
const ErrUnsupported = probeError("native process inspection unsupported on this platform")

// Image identifies an executable file independently of its current name.
type Image struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Identity describes a running process. ParentPID zero denotes a proven root.
type Identity struct {
	PID       int
	ParentPID int
	Argv      []string
	Birth     time.Time
	Image     Image
}

// socketEvidence represents one TCP socket, not one endpoint. All visible holders
// of its descriptors are retained; an empty list means no holder could be resolved.
type socketEvidence struct {
	port      uint16
	listening bool
	owners    []int
}

// Probe reads process facts from the operating system when zero-valued.
type Probe struct {
	inspect   func(context.Context, int) (Identity, error)
	existence func(context.Context, int) error
	processes func(context.Context) ([]Identity, error)
	sockets   func(context.Context) ([]socketEvidence, error)
}

// Identity returns the live identity, os.ErrNotExist for confirmed absence, or ErrUnknown for inconclusive inspection.
func (p Probe) Identity(ctx context.Context, pid int) (Identity, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return Identity{}, os.ErrNotExist
	}
	inspect := p.inspect
	if inspect == nil {
		inspect = nativeIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout(runtime.GOOS))
	defer cancel()
	id, err := inspect(ctx, pid)
	if err == nil && ctx.Err() == nil && id.PID == pid && id.ParentPID >= 0 && !id.Birth.IsZero() && len(id.Argv) > 0 {
		return id, nil
	}
	if errors.Is(err, ErrUnsupported) {
		return Identity{}, err
	}
	if alive, checkErr := p.Alive(ctx, pid); checkErr == nil && !alive {
		return Identity{}, os.ErrNotExist
	}
	return Identity{}, fmt.Errorf("%w: pid %d", ErrUnknown, pid)
}

// Table returns a snapshot keyed by pid, or no table when the query fails.
func (p Probe) Table(ctx context.Context) (map[int]Identity, error) {
	query := p.processes
	if query == nil {
		query = nativeTable
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout(runtime.GOOS))
	defer cancel()
	rows, err := query(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	table := make(map[int]Identity, len(rows))
	for _, row := range rows {
		if row.PID > 0 && row.ParentPID >= 0 {
			table[row.PID] = row
		}
	}
	return table, nil
}

// Alive reports whether the positive pid exists, including when inspection is denied.
func (p Probe) Alive(ctx context.Context, pid int) (bool, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	check := p.existence
	if check == nil {
		check = nativeExistence
	}
	err := check(ctx, pid)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

// Listener describes evidence for a TCP listening port; PID is nonzero only for a unique owner.
type Listener struct {
	State ListenerState
	PID   int
}

// ListenerState distinguishes evidence of absence from incomplete inspection.
type ListenerState string

// These states describe probe evidence, not authority to signal a process.
const (
	ListenerDenied     ListenerState = "denied"
	ListenerAmbiguous  ListenerState = "ambiguous"
	ListenerTimedOut   ListenerState = "timeout"
	ListenerAbsent     ListenerState = "absent"
	ListenerIdentified ListenerState = "identified"
	ListenerUnresolved ListenerState = "port-owned"
	ListenerUnknown    ListenerState = "unknown"
)

// Listener returns TCP listener evidence within the configured probe budget.
func (p Probe) Listener(ctx context.Context, port uint16) Listener {
	if port == 0 {
		return Listener{State: ListenerAbsent}
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout(runtime.GOOS))
	defer cancel()
	// Some kernel reads cannot be canceled. Let the caller leave at the deadline
	// without leaving the query waiting on a receiver when it eventually returns.
	done := make(chan Listener, 1)
	go func() {
		var rows []socketEvidence
		var err error
		if p.sockets == nil {
			rows, err = nativeSockets(ctx, port)
		} else {
			rows, err = p.sockets(ctx)
		}
		done <- classifyListener(rows, err, port)
	}()
	select {
	case result := <-done:
		if ctx.Err() == nil {
			return result
		}
	case <-ctx.Done():
	}
	return classifyListener(nil, ctx.Err(), port)
}

func classifyListener(rows []socketEvidence, err error, port uint16) Listener {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Listener{State: ListenerTimedOut}
	case errors.Is(err, os.ErrPermission):
		return Listener{State: ListenerDenied}
	case err != nil:
		return Listener{State: ListenerUnknown}
	}
	var owner int
	var unresolved bool
	for _, row := range rows {
		if !row.listening || row.port != port {
			continue
		}
		unresolved = unresolved || len(row.owners) == 0
		for _, pid := range row.owners {
			switch {
			case pid <= 0:
				unresolved = true
			case owner != 0 && owner != pid:
				return Listener{State: ListenerAmbiguous}
			default:
				owner = pid
			}
		}
	}
	switch {
	case unresolved && owner != 0:
		return Listener{State: ListenerAmbiguous}
	case unresolved:
		return Listener{State: ListenerUnresolved}
	case owner != 0:
		return Listener{State: ListenerIdentified, PID: owner}
	default:
		return Listener{State: ListenerAbsent}
	}
}

// ProbeTimeout reads CODEX_GATEWAY_PROBE_TIMEOUT_MS for the given GOOS.
func ProbeTimeout(goos string) time.Duration {
	ms := config.ProbeTimeoutFor(os.LookupEnv, goos)
	if ms >= float64(math.MaxInt64)/float64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// LegacyStart formats a birth time like POSIX ps lstart in the supplied time's location.
func LegacyStart(birth time.Time) string { return birth.Format(time.ANSIC) }

// Record is the authoritative on-disk process identity for one instance and role.
type Record struct {
	PID        int    `json:"pid"`
	Command    string `json:"command"`
	StartedAt  string `json:"startedAt"`
	Role       string `json:"role"`
	StateRoot  string `json:"stateRoot"`
	Generation string `json:"generation"`
	Port       uint16 `json:"port"`
	Version    string `json:"version"`
	Image      Image  `json:"image"`
}
