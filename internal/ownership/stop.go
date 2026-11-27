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
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	// terminateGrace is how long a terminated process has to exit before it is killed (lib/process-supervision.js:13,
	// :655).
	terminateGrace = 3 * time.Second
	// exitPoll is the pause between two checks for an exit (lib/process-supervision.js:906).
	exitPoll = 50 * time.Millisecond
	// killSettle is how long a killed process has to disappear before the stop reports it still running.
	killSettle = 2 * time.Second
)

// processHandle is the reference through which the built-in delivery signals one process.
type processHandle interface {
	signal(sig syscall.Signal) error
	close() error
}

// errProcessGone means the process a handle names no longer exists.
var errProcessGone = errors.New("process already gone")

// errReplaced means the stored record changed between the decision and the signal.
var errReplaced = errors.New("the stored record was replaced")

// errExitUnconfirmed means the process was signalled and neither its exit nor its survival could be established.
var errExitUnconfirmed = errors.New("neither the exit nor the survival of the signalled process could be established")

// errForeignEvidence refuses the built-in delivery on evidence that does not come from the native probe: a handle
// would act on a real process number that the supplied evidence may not describe.
var errForeignEvidence = errors.New("the built-in signal delivery acts only on the native probe's evidence; a supplied Processes needs a supplied Signal")

// verdict is what one decision sample shows about the recorded process.
type verdict int

const (
	// owned: the sample shows the recorded process and, where the role needs it, the listener agrees.
	owned verdict = iota
	// gone: the number no longer exists.
	gone
	// ended: the number holds another lifetime, so the recorded process has exited.
	ended
	// refused: the evidence is unreadable or contradicts the record.
	refused
)

// stopper ends the process one stored record names. The stored record, read once at the start, is the authority
// for every decision; it is read again under the store's exclusion immediately before each signal, and a record
// replaced in between cancels the signal.
type stopper struct {
	a      Authority
	ctx    context.Context
	name   string
	stored procinfo.Record
	// lastSignal is the most recent signal call's error, kept for the report of a process that stayed.
	lastSignal error
	// attempted lists the signals whose delivery was attempted, in order. Delivery itself is never established: only
	// the exit is.
	attempted []syscall.Signal
}

// attemptedText says which signals the stop attempted, for an error that reports what it had done.
func (s *stopper) attemptedText() string {
	switch len(s.attempted) {
	case 0:
		return "nothing was signalled"
	case 1:
		return "the terminate was attempted"
	}
	return "the terminate and the kill were attempted"
}

// signalName names a signal of the stop in an error.
func signalName(sig syscall.Signal) string {
	if sig == syscall.SIGKILL {
		return "kill"
	}
	return "terminate"
}

// acquisition returns the context for one wait on the record lock: the caller's when it has a deadline, otherwise
// the caller's bounded by the same default budget Resolve uses, so that no wait for the store is ever unbounded.
func (s *stopper) acquisition() (context.Context, context.CancelFunc) {
	if _, ok := s.ctx.Deadline(); ok {
		return s.ctx, func() {}
	}
	return context.WithTimeout(s.ctx, procinfo.DefaultProbeTimeout(runtime.GOOS))
}

// unavailable turns an error of a store operation of the stop into the stop's result when the operation could not
// take the record lock. A wait whose own bound expired, or a lock that was held when it must not be waited for,
// means another operation kept the lock: the result matches errRecordsBusy. A wait that the caller's context ended
// matches the caller's error. In both cases nothing was read or changed, and the text says so after what, which
// names the step and what the stop had done. Any other error, one that came after the lock was taken, is returned as
// nil here so that the caller reports it as the failure it is. No result matches ErrNotOwned or ErrStillRunning.
func (s *stopper) unavailable(err error, what string) error {
	switch {
	case !errors.Is(err, errLockWait) && !errors.Is(err, errRecordsBusy):
		return nil
	case s.ctx.Err() != nil:
		return fmt.Errorf("record %s: %s, but the context ended before the record lock was free, so the record was not touched and is kept as it was: %w", s.name, what, s.ctx.Err())
	}
	return fmt.Errorf("record %s: %s, but %w, so the record was not touched and is kept as it was", s.name, what, errRecordsBusy)
}

func (a Authority) stop(ctx context.Context, name string, expected procinfo.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.Records.validate(name); err != nil {
		return err
	}
	if a.Signal == nil && !nativeEvidence(a.Processes) {
		return errForeignEvidence
	}
	if expected.StartToken == "" {
		return fmt.Errorf("%w: the expected record %s has no start token", ErrNotOwned, name)
	}
	s := &stopper{a: a, ctx: ctx, name: name}
	acquisition, cancel := s.acquisition()
	stored, err := a.Records.load(acquisition, name)
	cancel()
	if failed := s.unavailable(err, "its record was to be read and nothing was signalled"); failed != nil {
		return failed
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("%w: record %s: %w", ErrNotOwned, name, err)
	}
	if !sameRecord(stored, expected) {
		return fmt.Errorf("%w: record %s names another process, generation or start token than expected", ErrNotOwned, name)
	}
	s.stored = stored
	if done, err := s.escalate(syscall.SIGTERM); done || err != nil {
		return err
	}
	exited, err := s.awaitExit(time.Now().Add(terminateGrace))
	if err != nil {
		return err
	}
	if !exited {
		// The kill is its own decision, on samples taken when the grace has ended.
		if done, err := s.escalate(syscall.SIGKILL); done || err != nil {
			return err
		}
		if exited, err = s.awaitExit(time.Now().Add(killSettle)); err != nil {
			return err
		}
	}
	if exited {
		return s.retire()
	}
	return s.finalOutcome()
}

// finalOutcome judges a process that the exit polls did not see go, on one last sample: a confirmed exit (the number
// is gone or holds another lifetime) is a stop and retires the record; the same lifetime still running is
// ErrStillRunning; anything less is an error that claims neither, and the record is kept for the next attempt.
func (s *stopper) finalOutcome() error {
	id, err := s.a.processes().Identity(s.ctx, s.stored.PID)
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s.retire()
	case err != nil:
		return fmt.Errorf("process %d of record %s: %w: its final sample is unreadable", s.stored.PID, s.name, errExitUnconfirmed)
	case id.StartToken == "":
		return fmt.Errorf("process %d of record %s: %w: its final sample has no start token", s.stored.PID, s.name, errExitUnconfirmed)
	case id.StartToken != s.stored.StartToken:
		return s.retire()
	}
	if s.lastSignal != nil {
		// The signal call's error is text only, so the result never matches a sentinel the call might have wrapped.
		return fmt.Errorf("%w: process %d is still running after the kill (last signal call: %s)", ErrStillRunning, s.stored.PID, s.lastSignal.Error())
	}
	return fmt.Errorf("%w: process %d is still running after the kill", ErrStillRunning, s.stored.PID)
}

// escalate decides on two fresh samples whether sig may be sent, then sends it. It reports done when the recorded
// process turned out to be gone, in which case the record has been retired.
//
// With the built-in delivery, the process handle is opened before the second sample and the signal goes through it.
// On Linux that is a pidfd, so the signal reaches only the process the second sample described, unless pidfd_open
// answers ENOSYS and the number is used instead (see openProcess); elsewhere it is the number, and the two samples
// only narrow the window in which it could be reused.
func (s *stopper) escalate(sig syscall.Signal) (done bool, err error) {
	if v, why, _, err := s.decide(); err != nil || v != owned {
		return s.conclude(v, why, err)
	}
	var handle processHandle
	if s.a.Signal == nil {
		handle, err = openProcess(s.stored.PID)
		if errors.Is(err, errProcessGone) {
			return true, s.retire()
		}
		if err != nil {
			return false, err
		}
		defer func() { _ = handle.close() }()
	}
	v, why, sample, err := s.decide()
	if err != nil || v != owned {
		return s.conclude(v, why, err)
	}
	// The decision is only as fresh as the samples just taken, so the final check never waits: a record lock held by
	// another operation refuses the signal instead of letting it fire on a decision that has gone stale meanwhile.
	// The check reads only the authoritative file and repairs nothing. The signal call runs under the lock.
	err = s.a.Records.locked(s.ctx, false, func(dir *atomicfile.Dir) error {
		if !stillStored(dir, s.name, s.stored) {
			return errReplaced
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		// Success is judged by the exit, not by the signal call: a call that fails because the process has just gone
		// is a stop, and a call that succeeds while the process stays is not.
		s.attempted = append(s.attempted, sig)
		if s.a.Signal != nil {
			s.lastSignal = s.a.Signal(s.ctx, sample, sig)
		} else {
			s.lastSignal = handle.signal(sig)
		}
		return nil
	})
	if errors.Is(err, errReplaced) {
		return false, fmt.Errorf("%w: record %s was replaced before the %v", ErrNotOwned, s.name, sig)
	}
	if failed := s.unavailable(err, fmt.Sprintf("the %s was due and was not sent; %s", signalName(sig), s.attemptedText())); failed != nil {
		return false, failed
	}
	return false, err
}

// nativeEvidence reports whether p is the native probe, the only evidence the built-in delivery may act on.
func nativeEvidence(p Processes) bool {
	switch p.(type) {
	case nil, procinfo.Probe:
		return true
	}
	return false
}

// conclude turns a decision other than owned into the stop's result.
func (s *stopper) conclude(v verdict, why string, err error) (bool, error) {
	switch {
	case err != nil:
		return false, err
	case v == gone || v == ended:
		return true, s.retire()
	}
	return false, fmt.Errorf("%w: process %d of record %s: %s", ErrNotOwned, s.stored.PID, s.name, why)
}

// decide takes one identity sample of the recorded number and judges it against the stored record; for a supervisor
// or proxy record it also asks the record's port, which must be held by that process alone. It returns the sample
// so that the signal is handed exactly the identity it was decided on.
func (s *stopper) decide() (verdict, string, procinfo.Identity, error) {
	p := s.a.processes()
	id, err := p.Identity(s.ctx, s.stored.PID)
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return refused, "", procinfo.Identity{}, ctxErr
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return gone, "", id, nil
	case err != nil:
		return refused, fmt.Sprintf("its sample is unreadable: %v", err), id, nil
	case id.StartToken == "":
		return refused, "the live process has no readable start token", id, nil
	case id.StartToken != s.stored.StartToken:
		return ended, "", id, nil
	}
	if why := contradiction(s.stored, id); why != "" {
		return refused, why, id, nil
	}
	if !listenerDecides(s.stored.Role) {
		return owned, "", id, nil
	}
	l, err := p.ListenerEvidence(s.ctx, s.stored.Port)
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return refused, "", procinfo.Identity{}, ctxErr
	}
	if err != nil || l.State != procinfo.ListenerIdentified || l.PID != s.stored.PID {
		return refused, fmt.Sprintf("its port :%d is not held by it alone (%s)", s.stored.Port, l.State), id, nil
	}
	return owned, "", id, nil
}

// awaitExit checks for the exit until deadline, pausing between checks; it reports whether the number is gone. A
// number that is still alive may already hold another process: the next decision tells.
func (s *stopper) awaitExit(deadline time.Time) (bool, error) {
	for {
		alive, err := s.a.processes().Alive(s.ctx, s.stored.PID)
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if err == nil && !alive {
			return true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		if err := s.a.wait(s.ctx, min(exitPoll, remaining)); err != nil {
			return false, err
		}
	}
}

// retire removes the stored record of a process that is gone, unless it has been replaced since.
//
// The wait for the record lock is bounded as every wait of the stop is. Every error says that the process is gone and
// which signals were attempted. A lock still held at the bound, or a caller's context that ended first, left the
// record untouched, and the error says that it is kept (matching errRecordsBusy, or the caller's error). An error from
// the removal itself, a partial one included, is carried as such. No error matches ErrNotOwned or ErrStillRunning.
func (s *stopper) retire() error {
	acquisition, cancel := s.acquisition()
	defer cancel()
	_, err := s.a.Records.removeIf(acquisition, s.name, s.stored)
	if err == nil {
		return nil
	}
	what := fmt.Sprintf("process %d is gone and %s", s.stored.PID, s.attemptedText())
	if failed := s.unavailable(err, what); failed != nil {
		return failed
	}
	// The lock was held, so the error came from the removal itself: a record whose mirror could not be removed after
	// its authoritative file was is reported as that partial removal, never as a record left intact.
	return fmt.Errorf("record %s: %s, but removing its record did not complete: %w", s.name, what, err)
}
