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

// Package proxyrecovery keeps the configured proxy answering for one supervisor. Each Recover call is one tick of
// the source's recovery sequence (lib/process-supervision.js:1083-1199): a probe that answers resets the backoff, a
// proxy this monitor started is given its startup grace, an unexpired retry time defers, a live listener is probed
// once more on a fresh connection, and only then is an attempt recorded. A holder of the port is stopped only when a
// record of this state root establishes authority over it; a proven foreign holder halts recovery for good.
package proxyrecovery

import (
	"context"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// State names the outcome of one recovery tick.
type State string

// The outcomes of a recovery tick. The managed outcomes keep the source's spelling. A tick that makes an attempt
// journals its outcome in that spelling; healthy, backing-off and a tick inside the startup grace journal nothing,
// and neither does a new tick after Stop. A tick whose attempt was already journaled may still return stopped when
// Stop arrives during it. ProxyDown is the unmanaged mode's only failure.
const (
	// Healthy means a probe answered.
	Healthy State = "healthy"
	// Starting means a proxy this monitor started is still inside its startup grace, or was started by this tick and
	// did not answer yet.
	Starting State = "starting"
	// BackingOff means the retry time of the last attempt has not come yet.
	BackingOff State = "backing-off"
	// BinaryMissing means the proxy executable is not installed.
	BinaryMissing State = "binary-missing"
	// OwnerUnknown means the holder of the bound port could not be established, or nothing grants authority over it.
	OwnerUnknown State = "owner-unknown"
	// PortOwned means the port is bound and its holder could not be resolved.
	PortOwned State = "port-owned"
	// ForeignPortOwner means a proven foreign holder owns the port; recovery is halted for the monitor's lifetime.
	ForeignPortOwner State = "foreign-port-owner"
	// PortStuck means a stopped holder did not release the port.
	PortStuck State = "port-stuck"
	// Recovered means this tick started a proxy and it answered.
	Recovered State = "recovered"
	// Stopped means the monitor has been stopped.
	Stopped State = "stopped"
	// ProxyDown means the unmanaged mode's probe did not answer.
	ProxyDown State = "proxy-down"
)

// Result is the outcome of one recovery tick.
type Result struct {
	// State is the outcome.
	State State
	// PID is the process number of the proxy this monitor started, when the state concerns it.
	PID int
	// RetryAt is when the next attempt may begin, for an outcome that follows an attempt or defers to one; zero for
	// healthy, recovered, foreign-port-owner and stopped. A tick inside the startup grace may leave it zero or report
	// the last attempt's retry time.
	RetryAt time.Time
}

// Child is a proxy process this monitor started.
type Child struct {
	// Record is the process record published for the child in this state root.
	Record procinfo.Record
	// Done is closed when the child has exited.
	Done <-chan struct{}
}

// Options supplies the monitor's collaborators. Every effect on the system goes through one of them.
type Options struct {
	// Managed enables recovery. Without it the monitor only probes, and it never starts, signals, adopts or stops a
	// process.
	Managed bool
	// Port is the configured proxy port.
	Port uint16
	// Now reads the clock for grace and backoff.
	Now func() time.Time
	// Probe reports whether the proxy answers its models request with status 200 on a fresh connection.
	Probe func(context.Context) bool
	// Listening reports whether a listener holds Port.
	Listening func(context.Context) (bool, error)
	// BinaryExists reports whether the proxy executable is installed.
	BinaryExists func() bool
	// Resolve classifies the holder of Port against this state root's proxy record.
	Resolve func(context.Context) (ownership.Owner, error)
	// Start starts a proxy and publishes its record. Publishing the record is the collaborator's obligation; the monitor
	// does not check it.
	Start func(context.Context) (Child, error)
	// StopOwned stops the process that record names, on the authority of the stored record.
	StopOwned func(context.Context, procinfo.Record) error
	// Recorded returns this state root's proxy record naming the process number.
	Recorded func(context.Context, int) (procinfo.Record, error)
	// WaitRelease waits, within its own bound, for Port to be released and reports whether it was. The bound is the
	// collaborator's obligation.
	WaitRelease func(context.Context) bool
	// Record journals one lifecycle event with its fields.
	Record func(string, map[string]any)
}

// Monitor runs proxy recovery for one supervisor.
type Monitor struct{}

// New validates the options and returns a monitor that has not probed anything yet.
func New(o Options) (*Monitor, error) { panic("not implemented") }

// Recover runs one recovery tick, or joins the one in flight, and returns its result. The tick runs on the monitor's
// lifetime, not on a caller's: a caller's context bounds only that caller's wait, and when it ends Recover returns
// its error while the tick goes on for the other callers. Only Stop cancels the tick's work.
func (*Monitor) Recover(context.Context) (Result, error) { panic("not implemented") }

// Stop halts recovery for good, cancels its probes, joins a recovery in flight and then stops the proxy this monitor
// started, with a bounded check of the port's release whose failure is ignored. A foreign halt is not a stop. Callers
// that overlap all wait for the same work. A caller whose context ends while it waits to join the recovery in flight
// gets that context's error; when the context ends during the stop of the monitor's own proxy, Stop returns at once,
// with nil or the context's error.
func (*Monitor) Stop(context.Context) error { panic("not implemented") }
