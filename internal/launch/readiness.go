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

// Package launch starts the detached shim supervisor and waits until it answers.
package launch

import (
	"context"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// Ticket names the supervisor launch this caller started: the process number it got back and the startup generation
// it handed to that process.
type Ticket struct {
	// PID is the launched supervisor's process number; it must be positive.
	PID int
	// Generation is the startup generation given to the launched supervisor; it must not be empty.
	Generation string
}

// ReadyResult is the outcome of one readiness wait. At most one of Ready, TimedOut and a nonempty Reason holds.
type ReadyResult struct {
	// Ready reports that a health probe started before the time limit answered, also when it answered after it.
	Ready bool
	// TimedOut reports that the wait observed the time limit to have passed while no poll it started had seen an
	// answering probe or a failure record of the ticket's launch with a nonempty message.
	TimedOut bool
	// StillStarting reports, together with TimedOut, that the ticket's process was alive when the wait observed that
	// the limit had passed.
	StillStarting bool
	// Reason is the nonempty message of the failure record the ticket's own supervisor wrote, which ended the wait early.
	Reason string
}

// Readiness polls a supervisor's health and its startup-failure record until one of them decides the outcome.
type Readiness struct {
	// Files locates the startup-failure record.
	Files lifecycle.Files
	// Probe reports whether the supervisor answers its health check.
	Probe func(context.Context) bool
	// Alive reports whether the process with the given number still exists.
	Alive func(context.Context, int) (bool, error)
	// Now is the clock that bounds the wait.
	Now func() time.Time
	// Wait pauses between two polls; it returns early with the context's error.
	Wait func(context.Context, time.Duration) error
}

// Await polls the supervisor until a probe started before the time limit answers, a failure record of the ticket's
// launch ends the wait, or the wait observes that the limit has passed. A poll starts only while Now is before the
// limit, and runs in this order: it probes; when the probe does not answer and the wait has a ticket, it looks for a
// failure record whose pid and generation both match the ticket, and such a record with a nonempty message ends the
// wait with that message; only a poll still undecided then pauses for 300 ms through Wait. A poll that has started
// is finished in that order even when the limit passes during it. A zero or negative timeout starts no poll.
//
// With a ticket (a launch this caller started), Alive decides once, after the limit has passed, whether the
// ticket's process is still starting. A ticket without a positive pid or without a generation is refused before any
// of r's functions is called. With a nil ticket (waiting for a service that already exists), failure records are
// ignored and liveness is not consulted. The end of ctx ends the wait with its error, whatever a call in progress
// returns, its own error included; an error Wait returns on its own while ctx is live ends the wait too, and nothing
// is called after it. Every error comes with the zero ReadyResult.
func (r Readiness) Await(ctx context.Context, own *Ticket, timeout time.Duration) (ReadyResult, error) {
	panic("not implemented")
}
