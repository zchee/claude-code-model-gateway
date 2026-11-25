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
// An identified listener has one holder visible to this user. A process whose
// descriptors are unreadable may share its socket; ownership evidence alone
// is never authority to signal a process.
package procinfo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

type probeError string

func (e probeError) Error() string { return string(e) }

// ErrUnknown means that the inspection did not establish a process identity.
const ErrUnknown = probeError("process identity unknown")

// ErrGroupCleanup means the command's process group could not be cleaned up.
const ErrGroupCleanup = probeError("command process group could not be cleaned up")

// ErrUnsupported means the operating system has no native process probe.
const ErrUnsupported = probeError("native process inspection unsupported on this platform")

// Image identifies an executable by device and inode. On Linux Path is raw kernel
// link text, including the " (deleted)" suffix after unlink, not a relaunch path.
type Image struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Identity describes a running process. ParentPID zero denotes a proven root.
// Linux Birth adds start ticks to the boot wall clock; a clock step can change
// that computed birth for the same process. LegacyStart retains whole seconds.
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

// Probe reads native process facts. A zero-valued Probe uses the default budget.
// Its configuration never falls back to the process environment.
type Probe struct {
	listenerBusy *atomic.Bool
	lookup       config.LookupEnv
	inspect      func(context.Context, int) (Identity, error)
	existence    func(context.Context, int) error
	processes    func(context.Context) ([]Identity, error)
	sockets      func(context.Context) ([]socketEvidence, error)
}

// New returns a Probe whose budget uses lookup. A nil lookup selects the platform default.
// The probe and its copies share a single outstanding listener query slot.
func New(lookup config.LookupEnv) Probe { return Probe{lookup: lookup, listenerBusy: new(atomic.Bool)} }

// Identity returns the live identity, os.ErrNotExist for confirmed absence, or ErrUnknown for inconclusive inspection.
// A pending kernel read may not return when the context expires.
func (p Probe) Identity(ctx context.Context, pid int) (Identity, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return Identity{}, os.ErrNotExist
	}
	inspect := p.inspect
	if inspect == nil {
		inspect = nativeIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(runtime.GOOS))
	defer cancel()
	id, err := inspect(ctx, pid)
	if err == nil && ctx.Err() == nil && id.PID == pid && id.ParentPID >= 0 && !id.Birth.IsZero() && len(id.Argv) > 0 && id.Argv[0] != "" {
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
// On Darwin a row with nil argv lacks readable arguments; it does not prove exit.
// A kernel read already in progress cannot necessarily be canceled.
func (p Probe) Table(ctx context.Context) (map[int]Identity, error) {
	query := p.processes
	if query == nil {
		query = nativeTable
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(runtime.GOOS))
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
// Identified means the only holder visible to this user, not authority to signal.
// A deadline abandons this call's worker; it cannot interrupt a stuck kernel read.
// New probes refuse another query as unknown until that worker finishes. The zero
// value has no admission limit. On macOS the kernel count and visible holders are
// separate observations, so their combination is best-effort evidence.
func (p Probe) Listener(ctx context.Context, port uint16) Listener {
	if port == 0 {
		return Listener{State: ListenerAbsent}
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(runtime.GOOS))
	defer cancel()
	// Some kernel reads cannot be canceled. Let the caller leave at the deadline
	// without leaving the query waiting on a receiver when it eventually returns.
	if err := ctx.Err(); err != nil {
		return classifyListener(nil, err, port)
	}
	if p.listenerBusy != nil && !p.listenerBusy.CompareAndSwap(false, true) {
		return Listener{State: ListenerUnknown}
	}
	done := make(chan Listener, 1)
	go func() {
		var rows []socketEvidence
		var err error
		if p.sockets == nil {
			rows, err = nativeSockets(ctx, port)
		} else {
			rows, err = p.sockets(ctx)
		}
		result := classifyListener(rows, err, port)
		if p.listenerBusy != nil {
			p.listenerBusy.Store(false)
		}
		done <- result
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

// DefaultProbeTimeout returns the probe budget for the given GOOS without a configuration lookup.
func DefaultProbeTimeout(goos string) time.Duration { return (Probe{}).timeout(goos) }

func (p Probe) timeout(goos string) time.Duration {
	ms := config.ProbeTimeoutFor(p.lookup, goos)
	if ms >= float64(math.MaxInt64)/float64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// LegacyStart formats a birth time like POSIX ps lstart in the supplied time's location.
func LegacyStart(birth time.Time) string { return birth.Format(time.ANSIC) }

// Record is the authoritative on-disk process identity for one instance and role.
// StartedAt is legacy local wall time at whole-second precision. On Linux a
// clock step can change the boot wall clock from which it is computed.
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
