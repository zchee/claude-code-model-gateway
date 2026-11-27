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

package ownership

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// unknownKind is why an Unknown holder could not be established; each kind has the source's own refusal text
// (lib/process-supervision.js:606-614).
type unknownKind int

const (
	unidentified unknownKind = iota
	timedOut
	hiddenCommand
)

// finding is the outcome of one inspection of the port.
type finding struct {
	class  Class
	pid    int
	image  string
	kind   unknownKind
	reason string
}

// resolver inspects one port within one deadline, fixed when Resolve starts: the caller's, or the probe's platform
// default budget from then when the caller has none. Every evidence call, the record's lock wait included, runs
// under a context that ends at that deadline.
type resolver struct {
	a        Authority
	ctx      context.Context
	name     string
	port     uint16
	deadline time.Time
	budget   time.Duration
}

func (a Authority) resolve(ctx context.Context, name string, port uint16) (Owner, error) {
	if port == 0 {
		return Owner{Class: Unknown}, fmt.Errorf("resolve the owner of port 0: %w", fs.ErrInvalid)
	}
	if err := a.Records.validate(name); err != nil {
		return Owner{Class: Unknown}, fmt.Errorf("resolve the owner of :%d against record %q: %w", port, name, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, procinfo.DefaultProbeTimeout(runtime.GOOS))
		defer cancel()
		deadline, _ = ctx.Deadline()
	}
	r := &resolver{a: a, ctx: ctx, name: name, port: port, deadline: deadline, budget: time.Until(deadline)}
	// One inspection, and one retry of an inconclusive answer while budget is left, as the source does
	// (lib/process-supervision.js:589-596). The retry's Ours counts only for the holder the first inspection saw.
	first := r.inspect()
	if first.class != Unknown || r.expired() {
		return r.owner(first)
	}
	retry := r.inspect()
	switch retry.class {
	case Unowned, Foreign, LegacyNode, PortOwned:
		return r.owner(retry)
	case Ours:
		if retry.pid == first.pid {
			return r.owner(retry)
		}
	}
	combined := finding{class: Unknown, pid: first.pid, image: first.image}
	if retry.pid != 0 {
		combined.pid, combined.image = retry.pid, retry.image
	}
	if retry.class == Unknown {
		combined.kind = retry.kind
	}
	return r.owner(combined)
}

// expired reports whether the caller's context has ended: evidence that arrives afterwards proves nothing.
func (r *resolver) expired() bool {
	return r.ctx.Err() != nil || !time.Now().Before(r.deadline)
}

func (r *resolver) unknown(pid int, kind unknownKind) finding {
	return finding{class: Unknown, pid: pid, kind: kind}
}

// inspect observes the port's holder once. Every evidence call is followed by a check of the budget, so that an
// answer that came after it ended is never taken for evidence, and no further call is made.
func (r *resolver) inspect() finding {
	if r.expired() {
		return r.unknown(0, timedOut)
	}
	p := r.a.processes()
	l, err := p.ListenerEvidence(r.ctx, r.port)
	if r.expired() {
		return r.unknown(0, timedOut)
	}
	if err != nil {
		return r.unknown(0, listenerKind(l, err))
	}
	switch l.State {
	case procinfo.ListenerAbsent:
		return finding{class: Unowned}
	case procinfo.ListenerUnresolved:
		return finding{class: PortOwned}
	case procinfo.ListenerIdentified:
		if l.PID > 0 {
			break
		}
		return r.unknown(0, unidentified)
	default:
		// Denied, ambiguous, timed out or inconclusive evidence is never absence.
		return r.unknown(0, listenerKind(l, nil))
	}
	pid := l.PID
	id, err := p.Identity(r.ctx, pid)
	if r.expired() {
		return r.unknown(pid, timedOut)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r.confirmAbsence(pid)
	case errors.Is(err, context.DeadlineExceeded):
		return r.unknown(pid, timedOut)
	case err != nil:
		return r.unreadable(pid)
	}
	// A classification that completes after the deadline, by a late record read or install comparison, is discarded:
	// only the holder it observed is kept.
	f := r.classify(id)
	if r.expired() {
		late := r.unknown(id.PID, timedOut)
		late.image = id.Image.Path
		return late
	}
	return f
}

func listenerKind(l procinfo.Listener, err error) unknownKind {
	if l.State == procinfo.ListenerTimedOut || errors.Is(err, context.DeadlineExceeded) {
		return timedOut
	}
	return unidentified
}

// confirmAbsence asks the port once more after its holder vanished: a port still bound is held by someone this
// inspection could not identify. An inconclusive identity is never followed by this question.
func (r *resolver) confirmAbsence(pid int) finding {
	if r.expired() {
		return r.unknown(pid, timedOut)
	}
	l, err := r.a.processes().ListenerEvidence(r.ctx, r.port)
	switch {
	case r.expired():
		return r.unknown(pid, timedOut)
	case err == nil && l.State == procinfo.ListenerAbsent:
		return finding{class: Unowned, pid: pid}
	case err != nil || l.State == procinfo.ListenerTimedOut:
		return r.unknown(pid, listenerKind(l, err))
	}
	return r.unknown(pid, unidentified)
}

// unreadable tells a holder whose command line is hidden, as a protected process's is, from one whose sample was
// merely inconclusive: only the first has a process-table row without arguments.
func (r *resolver) unreadable(pid int) finding {
	if r.expired() {
		return r.unknown(pid, timedOut)
	}
	table, err := r.a.processes().Table(r.ctx)
	if r.expired() {
		return r.unknown(pid, timedOut)
	}
	if row, ok := table[pid]; err == nil && ok && len(row.Argv) == 0 {
		return r.unknown(pid, hiddenCommand)
	}
	return r.unknown(pid, unidentified)
}

// classify judges a readable holder. Only this root's record naming the holder's number can make it Ours, and a
// record that names it but contradicts the sample makes it Unknown: a reused number is not another install. Only a
// Node gateway's command line names an install root, so only it can be Foreign or this install's LegacyNode.
func (r *resolver) classify(id procinfo.Identity) finding {
	if len(id.Argv) == 0 {
		return finding{class: Unknown, pid: id.PID, image: id.Image.Path, kind: hiddenCommand}
	}
	unknown := finding{class: Unknown, pid: id.PID, image: id.Image.Path, kind: unidentified}
	record, err := r.a.Records.load(r.ctx, r.name)
	// Once the budget has expired nothing more is started: not the comparison, and not the Legacy reading, which has
	// no context and may itself read the file system.
	if r.expired() {
		return finding{class: Unknown, pid: id.PID, image: id.Image.Path, kind: timedOut}
	}
	if err == nil && record.PID == id.PID {
		if contradiction(record, id) == "" && record.Port == r.port {
			return finding{class: Ours, pid: id.PID, image: id.Image.Path}
		}
		return unknown
	}
	if r.a.Legacy == nil {
		return unknown
	}
	root, current, ours := r.a.Legacy(id.Argv)
	switch {
	case root == "":
		return unknown
	case ours:
		reason := fmt.Sprintf("refusing to stop PID %d on :%d; it is the Node model-gateway of this install, not this gateway; stop it with its own command; left the listener untouched", id.PID, r.port)
		return finding{class: LegacyNode, pid: id.PID, image: id.Image.Path, reason: reason}
	}
	reason := fmt.Sprintf("refusing to stop PID %d on :%d; it belongs to a different install root (%s), not %s", id.PID, r.port, root, current)
	return finding{class: Foreign, pid: id.PID, image: id.Image.Path, reason: reason}
}

// owner renders a finding. A cancelled context is reported as an error beside the refusal; a deadline is the
// budget's end and is reported only in the refusal text.
func (r *resolver) owner(f finding) (Owner, error) {
	o := Owner{Class: f.class, PID: f.pid, Image: f.image, Reason: f.reason}
	switch f.class {
	case Unknown:
		o.Reason = r.unknownReason(f)
	case PortOwned:
		o.Reason = r.unknownReason(finding{kind: unidentified})
	}
	if err := r.ctx.Err(); errors.Is(err, context.Canceled) {
		return o, err
	}
	return o, nil
}

func (r *resolver) unknownReason(f finding) string {
	observed := "unknown"
	if f.pid != 0 {
		observed = strconv.Itoa(f.pid)
	}
	prefix := fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); ", r.port, observed)
	switch f.kind {
	case timedOut:
		ms := int64((max(r.budget, 0) + time.Millisecond - 1) / time.Millisecond)
		return prefix + fmt.Sprintf("the ownership probe exhausted its %dms budget (set CODEX_GATEWAY_PROBE_TIMEOUT_MS to override); left the listener untouched", ms)
	case hiddenCommand:
		return prefix + "the owning process did not expose its command line. It may be elevated or otherwise protected, so run stop or setup from a session with the same privileges; left the listener untouched"
	}
	return unidentifiedRefusal(r.port, f.pid)
}
