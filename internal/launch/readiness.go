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
	"errors"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// pollInterval is the pause between two polls of the wait, as in the source's start command.
const pollInterval = 300 * time.Millisecond

var errInvalidTicket = errors.New("model-gateway: a launch ticket needs a positive pid and a startup generation")

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
//
// Its functions have no defaults and must all be supplied; only Alive may be left nil, and only by a caller that
// always waits with a nil ticket. The time limit bounds when a poll may start, not when Await returns: a poll that
// started before the limit is finished, its probe and its pause included.
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

// Await waits for the supervisor to answer, for a matching failure record, or for the time limit to expire.
// A zero or negative timeout starts no poll, even if Now moves backward. For a positive timeout, a poll starts
// only while Now reports a time before the deadline. Each poll probes first. If the probe does not answer and
// own is non-nil, Await looks for a record matching the launch PID and generation; a nonempty matching message
// ends the wait with that message. A poll still undecided pauses for 300 ms through Wait. Crossing the deadline
// during a poll does not cut that poll short.
//
// Await takes the PID and generation of a non-nil ticket at entry, before calling any Readiness function, and
// uses those values for every record lookup and liveness check.
// With a ticket (a launch this caller started), Alive decides once, after the limit has passed, whether the
// ticket's process is still starting. A ticket without a positive pid or without a generation is refused before any
// of r's functions is called. With a nil ticket (waiting for a service that already exists), failure records are
// ignored and liveness is not consulted.
// If ctx has ended when Now returns, including on the reading that sets the deadline, Await returns the zero
// ReadyResult and ctx.Err() without another clock reading, probe, record lookup, pause, or liveness check.
// The end of ctx ends the wait with its error, whatever a call in progress returns, its own error included; an
// error Wait returns on its own while ctx is live ends the wait too, and nothing is called after it. Every error
// comes with the zero ReadyResult.
func (r Readiness) Await(ctx context.Context, own *Ticket, timeout time.Duration) (ReadyResult, error) {
	if err := ctx.Err(); err != nil {
		return ReadyResult{}, err
	}
	// The ticket is copied before the first callback, so a callback that changes the caller's Ticket can neither
	// empty the generation (which the record reader treats as a wildcard) nor redirect the liveness check.
	ticketed := own != nil
	var pid int
	var gen string
	if ticketed {
		pid, gen = own.PID, own.Generation
		if pid <= 0 || gen == "" {
			return ReadyResult{}, errInvalidTicket
		}
	}
	// A zero or negative limit starts no poll whatever the clock says, so it is not read at all.
	if timeout > 0 {
		deadline := r.Now().Add(timeout)
		// The caller's context is consulted after every call, so its end wins over whatever that call returned,
		// including a clock reading that shows the limit has passed.
		if err := ctx.Err(); err != nil {
			return ReadyResult{}, err
		}
		for {
			now := r.Now()
			if err := ctx.Err(); err != nil {
				return ReadyResult{}, err
			}
			if !now.Before(deadline) {
				break
			}
			ready := r.Probe(ctx)
			if err := ctx.Err(); err != nil {
				return ReadyResult{}, err
			}
			if ready {
				return ReadyResult{Ready: true}, nil
			}
			if ticketed {
				reason, ok := r.Files.StartupFailure(pid, gen)
				if err := ctx.Err(); err != nil {
					return ReadyResult{}, err
				}
				if ok && reason != "" {
					return ReadyResult{Reason: reason}, nil
				}
			}
			err := r.Wait(ctx, pollInterval)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ReadyResult{}, ctxErr
			}
			if err != nil {
				return ReadyResult{}, err
			}
		}
	}
	if !ticketed {
		return ReadyResult{TimedOut: true}, nil
	}
	alive, err := r.Alive(ctx, pid)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ReadyResult{}, ctxErr
	}
	if err != nil {
		return ReadyResult{}, err
	}
	return ReadyResult{TimedOut: true, StillStarting: alive}, nil
}
