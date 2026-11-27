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
	// Processes supplies the live evidence; nil means the native probe with its default budget, one probe shared by
	// every such Authority.
	Processes Processes
	// Signal delivers sig to the process the identity sample names. Nil, which production uses, means the built-in
	// delivery: Stop opens a handle on the process before the second sample of each decision and signals through it.
	// On Linux that is a pidfd, which cannot reach a process that reused the number; where pidfd_open answers ENOSYS
	// (a kernel before 5.3, or a system-call filter such as a seccomp profile that refuses it on a newer kernel) and on
	// the other supported Unix platforms it is the number itself, whose reuse between the second sample and the signal
	// the samples only narrow. The built-in delivery acts only on the native probe's evidence: with a nil Signal,
	// Processes must be nil or a procinfo.Probe, and any other Processes is refused before anything is signalled.
	//
	// A non-nil Signal bypasses the handles by design: it receives the second sample and is the whole delivery, with
	// whatever guarantee it gives itself; tests use it to signal only processes they own. Signal is called while the
	// store's exclusion is held, so it must return promptly, must not wait for the process, and must not call the
	// Store; the exclusion is given up even if it panics.
	Signal func(context.Context, procinfo.Identity, os.Signal) error
	// Wait pauses for the duration or until the context ends; nil means a timer.
	Wait func(context.Context, time.Duration) error
	// Legacy reads a command line as a Node gateway's. It returns the install root the command line names, or an
	// empty root when it is not a Node gateway's; this plugin's install root, for the refusal text; and whether the
	// named root belongs to this plugin. NodeGateway returns the production predicate; nil reads no command line as
	// a Node gateway's.
	Legacy func(argv []string) (root, current string, ours bool)
}

// ErrNotOwned means the evidence did not establish authority to act on the process.
var ErrNotOwned = errors.New("process ownership not established")

// ErrStillRunning means a fresh sample after the kill confirmed the signalled process still running in the same
// lifetime. A stop whose evidence establishes neither that nor an exit fails with an error that matches neither
// ErrStillRunning nor ErrNotOwned.
var ErrStillRunning = errors.New("process exit not confirmed")

// Resolve classifies the holder of port against the record name in this state root. It never signals. It reads the
// record through Store.Read, which repairs a stale or missing decimal mirror under the store's exclusion; nothing
// else is written. The probe budget is the time to the context's deadline when Resolve starts, or the native probe's
// platform default when the context has none; evidence that arrives after the deadline proves nothing, and an
// inconclusive first inspection is retried once while budget is left. A request it cannot answer (an invalid state
// root or record name, port zero) is an error; a cancelled context is reported as an error beside an Unknown owner.
func (a Authority) Resolve(ctx context.Context, name string, port uint16) (Owner, error) {
	return a.resolve(ctx, name, port)
}

// Stop ends the process that the record name of this state root names. The stored record, not expected, is the
// authority; expected only has to agree with it on process number, generation and start token. Every action is
// decided on a fresh sample, the stored record and, for a supervisor or proxy record, the listener evidence of its
// port, and re-sampled immediately before the signal. Success means the recorded process is gone.
//
// The terminate is sent on two samples that show the recorded process; the kill only when it is still running
// three seconds later and two fresh samples taken then still show it. A sample showing another start token on the
// number means the recorded process has already exited: nothing is signalled and the record is retired. Any other
// gap or contradiction in the evidence, a record replaced before the signal, or a kill whose fresh samples no longer
// show the recorded process, is an error matching ErrNotOwned, and the record is kept. A process still running after
// the kill, confirmed by a sample of the same lifetime, is an error matching ErrStillRunning, and the record is kept;
// when the last sample establishes neither the exit nor that, the error matches neither sentinel and the record is
// kept. A process that is gone has its record removed, unless it has been replaced in the meantime. An ended context
// returns the context's error.
//
// Three steps take the store's exclusion, and a signal is sent only by the second. A wait for the lock is bounded by
// the caller's deadline or, when the caller has none, by the default probe budget. When the lock stays held to that
// bound, the record has not been touched, and the error says so and matches neither sentinel; when the caller's own
// context ended the wait, the error also matches the caller's error:
//   - reading the stored record at the start: the error says nothing was signalled;
//   - the check immediately before each signal, which never waits: the error names the signal that was not sent and
//     says which were attempted before it (for the kill, the terminate was attempted; its delivery is not
//     established, only an exit would be);
//   - removing the record of a process that is gone: the error says the process is gone and which signals were
//     attempted. An error of the removal itself, a partial one that removed the record but not its mirror included,
//     is reported as that failure and never as a record left intact.
func (a Authority) Stop(ctx context.Context, name string, expected procinfo.Record) error {
	return a.stop(ctx, name, expected)
}
