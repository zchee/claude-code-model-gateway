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
	"time"

	gnet "github.com/shirou/gopsutil/v4/net"
)

var errNotImplemented = errors.New("procinfo: not implemented")

// ErrUnknown means that the inspection did not establish a process identity.
var ErrUnknown = errors.New("process identity unknown")

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

// Probe reads process facts from the operating system when zero-valued.
type Probe struct {
	inspect     func(context.Context, int) (Identity, error)
	existence   func(context.Context, int) error
	processes   func(context.Context) ([]Identity, error)
	connections func(context.Context, string) ([]gnet.ConnectionStat, error)
}

// Identity returns the live identity, os.ErrNotExist for confirmed absence, or ErrUnknown for inconclusive inspection.
func (Probe) Identity(ctx context.Context, pid int) (Identity, error) {
	return Identity{}, errNotImplemented
}

// Table returns a snapshot keyed by pid, or no table when the query fails.
func (Probe) Table(ctx context.Context) (map[int]Identity, error) { return nil, errNotImplemented }

// Alive reports whether the positive pid exists, including when inspection is denied.
func (Probe) Alive(ctx context.Context, pid int) (bool, error) { return false, errNotImplemented }

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
func (Probe) Listener(ctx context.Context, port uint16) Listener { return Listener{} }

// ProbeTimeout reads CODEX_GATEWAY_PROBE_TIMEOUT_MS for the given GOOS.
func ProbeTimeout(goos string) time.Duration { return 0 }

// LegacyStart formats a birth time like POSIX ps lstart in the supplied time's location.
func LegacyStart(birth time.Time) string { return "" }

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

// WriteRecord atomically publishes name.pid.json before updating its decimal mirror.
func WriteRecord(root, name string, record Record) error { return errNotImplemented }

// ReadRecord reads the authoritative JSON and repairs a stale or missing mirror.
func ReadRecord(root, name string) (Record, error) { return Record{}, errNotImplemented }
