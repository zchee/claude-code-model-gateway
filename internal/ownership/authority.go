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

// Package ownership decides which processes and listeners belong to one gateway instance and whether this
// instance may signal them. Authority comes only from a record in the instance's state root whose process number,
// birth, start token and command match a fresh sample of the live process; port and executable evidence can only
// narrow it. Inconclusive evidence never counts as absence.
package ownership

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// Class names what a port owner was found to be.
type Class string

// The classes of a port owner. Automatic paths act only on Ours. The class says what the holder is; Owner.Reason
// says why nothing may be done about it.
const (
	// Ours is a holder that a record in this state root names and that a fresh sample matches in every field.
	Ours Class = "ours"
	// Foreign is a holder with positive, independent evidence of another install: a Node gateway whose command line
	// names an install root that does not belong to this plugin.
	Foreign Class = "foreign"
	// LegacyNode is the Node gateway of this install, identified by its command line. Nothing acts on it automatically.
	LegacyNode Class = "legacy-node"
	// Unowned means no listener holds the port.
	Unowned Class = "unowned"
	// PortOwned means the port is bound but no holder could be resolved.
	PortOwned Class = "port-owned"
	// Unknown means the evidence did not establish the holder: unreadable, timed out, several holders, a readable
	// holder that no record of this root names, or a holder that contradicts the record naming its number (a
	// recycled number is not another install).
	Unknown Class = "unknown"
)

// Processes supplies live process and listener evidence; procinfo.Probe satisfies it.
type Processes interface {
	// Identity samples one live process, as procinfo.Probe.Identity does.
	Identity(context.Context, int) (procinfo.Identity, error)
	// Table samples every visible process, as procinfo.Probe.Table does.
	Table(context.Context) (map[int]procinfo.Identity, error)
	// Alive reports whether a process number exists, as procinfo.Probe.Alive does.
	Alive(context.Context, int) (bool, error)
	// ListenerEvidence observes the holders of a TCP listening port, as procinfo.Probe.ListenerEvidence does.
	ListenerEvidence(context.Context, uint16) (procinfo.Listener, error)
}

// Owner is the resolved holder of a port.
type Owner struct {
	// Class is what the holder was found to be.
	Class Class
	// PID is the last observed process number of the holder, or zero when none was observed.
	PID int
	// Reason is the refusal text to print when the class does not permit acting.
	Reason string
	// Image is the observed executable path of an identified holder, for diagnostics only.
	Image string
}

// Authority resolves port owners and stops recorded processes of one state root.
type Authority struct {
	// Records is this state root's record store.
	Records Store
	// Processes supplies the live evidence.
	Processes Processes
	// Signal delivers sig to the process the identity sample names.
	Signal func(context.Context, procinfo.Identity, os.Signal) error
	// Wait pauses for the duration or until the context ends.
	Wait func(context.Context, time.Duration) error
	// Legacy reads a command line as a Node gateway's. It returns the install root the command line names, or an
	// empty root when it is not a Node gateway's; this plugin's install root, for the refusal text; and whether the
	// named root belongs to this plugin.
	Legacy func(argv []string) (root, current string, ours bool)
}

// ErrNotOwned means the evidence did not establish authority to act on the process.
var ErrNotOwned = errors.New("process ownership not established")

// ErrStillRunning means the process was signalled and its exit could not be confirmed.
var ErrStillRunning = errors.New("process exit not confirmed")

// Resolve classifies the holder of port against the record name in this state root.
func (Authority) Resolve(ctx context.Context, name string, port uint16) (Owner, error) {
	panic("not implemented")
}

// Stop ends the process that the record name of this state root names. The stored record, not expected, is the
// authority; expected only has to agree with it on process number, generation and start token. Every action is
// decided on a fresh sample, the stored record and, for a supervisor or proxy record, the listener evidence of its
// port, and re-sampled immediately before the signal. Success means the recorded process is gone.
func (Authority) Stop(ctx context.Context, name string, expected procinfo.Record) error {
	panic("not implemented")
}
