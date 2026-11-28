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

package launch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/launch"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) { venue.Require(); os.Exit(m.Run()) }

const (
	failureName = "shim-supervisor-failure.txt"
	// pollInterval is the pause between two polls of the source's readiness wait.
	pollInterval = 300 * time.Millisecond
	// quietLimit is the wait of a quiet start; ordinaryLimit is the ordinary one with the default drain timeout.
	quietLimit    = 12 * time.Second
	ordinaryLimit = 42 * time.Second
	// watchdog bounds a hung wait in virtual time; it only aborts the test.
	watchdog = time.Hour
	// callGuard is the call budget of one wait, shared by its four functions and far above the few hundred calls a
	// correct wait makes. A wait that never blocks does not let the bubble's clock or the watchdog move, so the call
	// past the budget panics with budgetSpent before it logs or answers anything, and awaitOn recovers that as a
	// failure. The guard is synchronous: it is recovered only when the call runs on the goroutine that called Await,
	// so a callback invoked from a goroutine of the wait's own still aborts the test binary. It does not bound code that
	// spins without calling any of the wait's functions.
	callGuard = 100000

	launchedPID = 4242
	generation  = "a6f1c0de-0000-4000-8000-000000000001"
	bindFailure = "model-gateway: shim supervisor cannot bind 127.0.0.1:18080: EADDRINUSE"

	// otherPID and opaqueGeneration make a second valid ticket: another positive pid and a generation that is not a
	// UUID, which the wait must treat as an opaque string.
	otherPID         = 31337
	opaqueGeneration = "restart 7/é"
)

var (
	errPanicked  = errors.New("the wait panicked")
	errLiveness  = errors.New("liveness could not be determined")
	errPause     = errors.New("the pause failed on its own")
	launchTicket = launch.Ticket{PID: launchedPID, Generation: generation}
	otherTicket  = launch.Ticket{PID: otherPID, Generation: opaqueGeneration}
)

func ticket() *launch.Ticket { t := launchTicket; return &t }

func opaqueTicket() *launch.Ticket { t := otherTicket; return &t }

// event is one call the wait made, stamped with the time elapsed since the wait began.
type event struct {
	At   time.Duration
	What string
}

func probeAt(n int) event { return event{time.Duration(n) * pollInterval, "probe"} }

func pauseAt(n int) event {
	return event{time.Duration(n) * pollInterval, "wait " + pollInterval.String()}
}

func aliveAt(at time.Duration) event { return aliveFor(launchedPID, at) }

func aliveFor(pid int, at time.Duration) event { return event{at, "alive " + strconv.Itoa(pid)} }

// polls is the log of n polls that did not decide the outcome: a probe and a 300 ms pause each.
func polls(n int) []event {
	var events []event
	for i := range n {
		events = append(events, probeAt(i), pauseAt(i))
	}
	return events
}

// slowPolls is the log of n unanswered polls that start every period, each pause asking for 300 ms and starting
// probeTakes after its probe.
func slowPolls(n int, period, probeTakes time.Duration) []event {
	var events []event
	for i := range n {
		at := time.Duration(i) * period
		events = append(events, event{at, "probe"}, event{at + probeTakes, "wait " + pollInterval.String()})
	}
	return events
}

// answeredAt is the log of a wait whose n-th probe (counted from one) decided the outcome.
func answeredAt(n int) []event { return append(polls(n-1), probeAt(n-1)) }

// only keeps the events whose description starts with prefix.
func only(events []event, prefix string) []event {
	var kept []event
	for _, e := range events {
		if strings.HasPrefix(e.What, prefix) {
			kept = append(kept, e)
		}
	}
	return kept
}

// harness supplies the wait's probe, liveness check, clock and pause, and logs every call. It runs inside a synctest
// bubble, so time.Now is the bubble's clock and the pause is a real timer of that clock.
type harness struct {
	t     *testing.T
	start time.Time
	files lifecycle.Files

	// probe answers the n-th probe, counted from one; nil never answers.
	probe func(ctx context.Context, n int) bool
	// alive answers the liveness check; nil fails the test when liveness is consulted.
	alive func(ctx context.Context, pid int) (bool, error)
	// pause replaces the n-th pause, counted from one, after it has been logged; nil sleeps on the bubble's clock.
	pause func(ctx context.Context, n int, d time.Duration) error
	// clock, when set, sees every reading of the clock, as time since the wait began, just before Now returns it.
	clock func(elapsed time.Duration)
	// forbid fails the test on any call of the wait's functions, the clock included.
	forbid atomic.Bool

	mu     sync.Mutex
	events []event
	probes int
	pauses int
	calls  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, start: time.Now(), files: lifecycle.Files{StateDir: t.TempDir()}}
}

func (h *harness) log(what string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event{time.Since(h.start), what})
}

func (h *harness) logged() []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// forbidden reports a call the case does not allow.
func (h *harness) forbidden(call string) {
	if h.forbid.Load() {
		h.t.Errorf("the wait called %s where the case allows no further call", call)
	}
}

// budgetSpent is the panic of the call past callGuard; awaitOn recovers it.
type budgetSpent struct{ call string }

// spend counts one call of any of the wait's functions against the shared budget and panics once it is spent.
func (h *harness) spend(call string) {
	h.mu.Lock()
	h.calls++
	spent := h.calls > callGuard
	h.mu.Unlock()
	if spent {
		panic(budgetSpent{call})
	}
}

// waitDone waits, on the bubble's clock, for a context the wait handed to a callback to end after the caller's
// cancellation. A context that never ends (one detached from the caller's, or a nil Done channel) fails the test
// after one second instead of hanging it.
func (h *harness) waitDone(ctx context.Context, call string) {
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		h.t.Errorf("the context given to %s did not end within one second of the caller's cancellation", call)
	}
}

func (h *harness) readiness() launch.Readiness {
	return launch.Readiness{
		Files: h.files,
		Probe: func(ctx context.Context) bool {
			h.spend("Probe")
			h.forbidden("Probe")
			h.log("probe")
			h.mu.Lock()
			h.probes++
			n := h.probes
			h.mu.Unlock()
			if h.probe == nil {
				return false
			}
			return h.probe(ctx, n)
		},
		Alive: func(ctx context.Context, pid int) (bool, error) {
			h.spend("Alive")
			h.forbidden("Alive")
			h.log("alive " + strconv.Itoa(pid))
			if h.alive == nil {
				h.t.Errorf("liveness of pid %d consulted, want no liveness check in this case", pid)
				return false, errors.New("unexpected liveness check")
			}
			return h.alive(ctx, pid)
		},
		Now: func() time.Time {
			h.spend("Now")
			h.forbidden("Now")
			now := time.Now()
			if h.clock != nil {
				h.clock(now.Sub(h.start))
			}
			return now
		},
		Wait: func(ctx context.Context, d time.Duration) error {
			h.spend("Wait")
			h.forbidden("Wait")
			h.log("wait " + d.String())
			h.mu.Lock()
			h.pauses++
			n := h.pauses
			h.mu.Unlock()
			if h.pause != nil {
				return h.pause(ctx, n, d)
			}
			return sleep(ctx, d)
		},
	}
}

// outcome is what one wait returned and when.
type outcome struct {
	result  launch.ReadyResult
	err     error
	elapsed time.Duration
}

func (h *harness) await(ctx context.Context, own *launch.Ticket, timeout time.Duration) outcome {
	h.t.Helper()
	return awaitOn(h.t, h.readiness(), ctx, own, timeout, h.start)
}

// awaitOn runs Await on its own goroutine inside the caller's synctest bubble. A panic is a test failure, not an
// aborted binary. The watchdog, in the bubble's time, cancels the wait, joins it for another watchdog period and
// aborts the test. A wait that never blocks does not let the bubble's clock reach the watchdog; a wait that keeps
// calling its functions on this goroutine is stopped instead by the call past callGuard, whose budgetSpent panic is
// recovered here (a panic on a goroutine the wait started itself is not).
func awaitOn(t *testing.T, r launch.Readiness, ctx context.Context, own *launch.Ticket, timeout time.Duration, start time.Time) outcome {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				if spent, ok := p.(budgetSpent); ok {
					t.Errorf("the wait made more than %d calls (the last one %s); it does not follow its clock or its context", callGuard, spent.call)
				} else {
					t.Errorf("Await panicked: %v", p)
				}
				done <- outcome{err: errPanicked}
			}
		}()
		result, err := r.Await(ctx, own, timeout)
		done <- outcome{result: result, err: err, elapsed: time.Since(start)}
	}()
	select {
	case o := <-done:
		if t.Failed() {
			t.FailNow()
		}
		return o
	case <-time.After(watchdog):
		cancel()
		select {
		case <-done:
			t.Fatalf("watchdog: Await did not return within %v of virtual time; it returned after the cancellation", watchdog)
		case <-time.After(watchdog):
			t.Fatalf("watchdog: Await did not return within %v of virtual time, nor after the cancellation", watchdog)
		}
		return outcome{}
	}
}

func equal[T any](t testing.TB, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// writeRecord writes a startup-failure record from a callback of the wait, where t.Fatal is not allowed.
func writeRecord(t *testing.T, files lifecycle.Files, pid int, gen *string, message string) {
	if err := files.WriteStartupFailure(pid, gen, message); err != nil {
		t.Errorf("WriteStartupFailure: %v", err)
	}
}

func matchingRecord(t *testing.T, files lifecycle.Files) {
	t.Helper()
	noError(t, files.WriteStartupFailure(launchedPID, new(generation), bindFailure))
}

// awaitCase is one wait: the ticket, the time limit, the state before it and the answers it gets.
type awaitCase struct {
	own     *launch.Ticket
	timeout time.Duration
	setup   func(t *testing.T, files lifecycle.Files)
	probe   func(t *testing.T, files lifecycle.Files) func(ctx context.Context, n int) bool
	alive   func(start time.Time) func(ctx context.Context, pid int) (bool, error)
	// pauseFor, when set, is how long every pause really lasts on the bubble's clock, whatever it asks for.
	pauseFor time.Duration
	// want is the result; wantEvents, when set, is the exact log of calls.
	want       launch.ReadyResult
	wantEvents []event
	elapsed    time.Duration
	// check, when set, makes further assertions on the outcome and the log.
	check func(t *testing.T, got outcome, events []event)
}

func runAwaitCases(t *testing.T, tests map[string]awaitCase) {
	t.Helper()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				if tt.setup != nil {
					tt.setup(t, h.files)
				}
				if tt.probe != nil {
					h.probe = tt.probe(t, h.files)
				}
				if tt.alive != nil {
					h.alive = tt.alive(h.start)
				}
				if tt.pauseFor != 0 {
					h.pause = func(ctx context.Context, _ int, _ time.Duration) error { return sleep(ctx, tt.pauseFor) }
				}
				got := h.await(t.Context(), tt.own, tt.timeout)
				noError(t, got.err)
				equal(t, tt.want, got.result)
				if tt.wantEvents != nil {
					equal(t, tt.wantEvents, h.logged())
				}
				equal(t, tt.elapsed, got.elapsed)
				if tt.check != nil {
					tt.check(t, got, h.logged())
				}
			})
		})
	}
}

// answersAt answers the n-th probe and every later one.
func answersAt(answer int) func(*testing.T, lifecycle.Files) func(context.Context, int) bool {
	return func(*testing.T, lifecycle.Files) func(context.Context, int) bool {
		return func(_ context.Context, n int) bool { return n >= answer }
	}
}

func alwaysAlive(time.Time) func(context.Context, int) (bool, error) {
	return func(context.Context, int) (bool, error) { return true, nil }
}

func TestAwaitReady(t *testing.T) {
	runAwaitCases(t, map[string]awaitCase{
		"success: the first probe answers": {
			own:        ticket(),
			timeout:    quietLimit,
			probe:      answersAt(1),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(1),
		},
		"success: a later probe answers after three pauses": {
			own:        ticket(),
			timeout:    quietLimit,
			probe:      answersAt(4),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(4),
			elapsed:    3 * pollInterval,
		},
		"success: the last probe before the limit answers": {
			own:        ticket(),
			timeout:    quietLimit,
			probe:      answersAt(40),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(40),
			elapsed:    39 * pollInterval,
		},
		// The probe comes first: a supervisor that answers is ready, whatever record the state root holds.
		"success: an answer wins over a matching record present from the start": {
			own:        ticket(),
			timeout:    quietLimit,
			setup:      matchingRecord,
			probe:      answersAt(1),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(1),
		},
		"success: an answer wins over a matching record written during that probe": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 3 {
						writeRecord(t, files, launchedPID, new(generation), bindFailure)
						return true
					}
					return false
				}
			},
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(3),
			elapsed:    2 * pollInterval,
		},
		// As in the source (lib/commands.js:656-657), the limit is checked before a poll starts, so a probe started
		// before the limit that answers after it counts: the 40th probe starts at 11.7 s and answers at 12.2 s.
		"success: a probe started before the limit answers after it": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(*testing.T, lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n < 40 {
						return false
					}
					time.Sleep(500 * time.Millisecond)
					return true
				}
			},
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(40),
			elapsed:    quietLimit + 200*time.Millisecond,
		},
		"success: a wait without a ticket ends when the probe answers": {
			timeout:    quietLimit,
			probe:      answersAt(2),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(2),
			elapsed:    pollInterval,
		},
	})
}

func TestAwaitOwnFailure(t *testing.T) {
	runAwaitCases(t, map[string]awaitCase{
		// Liveness is not consulted (the harness fails the test if it is): the record decides, even while the
		// process lives.
		"success: a matching record present before the wait ends it after the first probe": {
			own:        ticket(),
			timeout:    quietLimit,
			setup:      matchingRecord,
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(1),
		},
		"success: a matching record written during a poll ends the wait at that poll": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 3 {
						writeRecord(t, files, launchedPID, new(generation), bindFailure)
					}
					return false
				}
			},
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(3),
			elapsed:    2 * pollInterval,
		},
		"success: a record of another generation replaced by a matching one ends the wait then": {
			own:     ticket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, new("an earlier launch"), "an old failure"))
			},
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 5 {
						writeRecord(t, files, launchedPID, new(generation), bindFailure)
					}
					return false
				}
			},
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(5),
			elapsed:    4 * pollInterval,
		},
		"success: the reason is the record's message byte for byte": {
			own:     ticket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, new(generation), "fatal: \"quoted\"\n\tsecond line é"))
			},
			want:       launch.ReadyResult{Reason: "fatal: \"quoted\"\n\tsecond line é"},
			wantEvents: answeredAt(1),
		},
		"success: a record matching another pid and an opaque generation ends that ticket's wait": {
			own:     opaqueTicket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(otherPID, new(opaqueGeneration), "the second ticket's failure"))
			},
			want:       launch.ReadyResult{Reason: "the second ticket's failure"},
			wantEvents: answeredAt(1),
		},
		"success: a reason that is only white space is returned byte for byte": {
			own:     ticket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, new(generation), " \t\n "))
			},
			want:       launch.ReadyResult{Reason: " \t\n "},
			wantEvents: answeredAt(1),
		},
		"success: a reason with white space at both ends is returned byte for byte": {
			own:     ticket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, new(generation), "\t padded reason \n"))
			},
			want:       launch.ReadyResult{Reason: "\t padded reason \n"},
			wantEvents: answeredAt(1),
		},
		// As in the source (lib/commands.js:656-660), a poll that has started is finished: the 40th probe starts at
		// 11.7 s and fails at 12.2 s, after the limit, and the record it then finds still ends the wait, with no pause.
		"success: a matching record found after a probe that crossed the limit ends the wait": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 40 {
						time.Sleep(500 * time.Millisecond)
						writeRecord(t, files, launchedPID, new(generation), bindFailure)
					}
					return false
				}
			},
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(40),
			elapsed:    quietLimit + 200*time.Millisecond,
		},
		"success: a matching record ends the wait before the ordinary limit": {
			own:     ticket(),
			timeout: ordinaryLimit,
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 100 {
						writeRecord(t, files, launchedPID, new(generation), bindFailure)
					}
					return false
				}
			},
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(100),
			elapsed:    99 * pollInterval,
		},
	})
}

// TestAwaitIgnoredFailure: a record that is not this launch's leaves the wait polling; the probe then answers.
func TestAwaitIgnoredFailure(t *testing.T) {
	record := func(pid int, gen *string, message string) func(*testing.T, lifecycle.Files) {
		return func(t *testing.T, files lifecycle.Files) {
			t.Helper()
			noError(t, files.WriteStartupFailure(pid, gen, message))
		}
	}
	raw := func(content string) func(*testing.T, lifecycle.Files) {
		return func(t *testing.T, files lifecycle.Files) {
			t.Helper()
			noError(t, os.WriteFile(filepath.Join(files.StateDir, failureName), []byte(content), 0o600))
		}
	}
	ignored := func(setup func(*testing.T, lifecycle.Files)) awaitCase {
		return awaitCase{
			own:        ticket(),
			timeout:    quietLimit,
			setup:      setup,
			probe:      answersAt(4),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(4),
			elapsed:    3 * pollInterval,
		}
	}
	runAwaitCases(t, map[string]awaitCase{
		"success: a record of another pid is ignored":                  ignored(record(launchedPID+1, new(generation), "another supervisor")),
		"success: a record of another generation is ignored":           ignored(record(launchedPID, new("an earlier launch"), "an old failure")),
		"success: a record of the pid without a generation is ignored": ignored(record(launchedPID, nil, "no generation recorded")),
		"success: a record with an empty generation is ignored":        ignored(raw(`{"supervisorPid":4242,"generation":"","message":"m"}`)),
		"success: a record of another pid and generation is ignored":   ignored(record(1, new("other"), "unrelated")),
		"success: a record of the opaque generation under another pid is ignored": func() awaitCase {
			c := ignored(record(launchedPID, new(opaqueGeneration), "another pid"))
			c.own = opaqueTicket()
			return c
		}(),
		"success: a malformed record is ignored":             ignored(raw(`{"supervisorPid":4242,"generation":"` + generation + `",`)),
		"success: a record whose pid is a string is ignored": ignored(raw(`{"supervisorPid":"4242","generation":"` + generation + `","message":"m"}`)),
		"success: a record without a message is ignored":     ignored(raw(`{"supervisorPid":4242,"generation":"` + generation + `"}`)),
		"success: a directory at the record's name is ignored": ignored(func(t *testing.T, files lifecycle.Files) {
			t.Helper()
			noError(t, os.Mkdir(filepath.Join(files.StateDir, failureName), 0o700))
		}),
		// As in the source, which ends the wait only on a truthy message, so an empty message
		// of a matching record does not end it.
		"success: a matching record with an empty message is ignored": ignored(record(launchedPID, new(generation), "")),
	})
}

// TestAwaitWithoutTicket: a wait for a service that already exists ignores every startup-failure record, including
// one the reader's empty-generation wildcard would match, and never consults liveness (it has no process number).
func TestAwaitWithoutTicket(t *testing.T) {
	runAwaitCases(t, map[string]awaitCase{
		"success: a record without a generation is ignored": {
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, nil, "no generation recorded"))
			},
			probe:      answersAt(3),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(3),
			elapsed:    2 * pollInterval,
		},
		"success: a record with a generation is ignored": {
			timeout:    quietLimit,
			setup:      matchingRecord,
			probe:      answersAt(3),
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(3),
			elapsed:    2 * pollInterval,
		},
		"success: a record written during the wait is ignored": {
			timeout: quietLimit,
			probe: func(t *testing.T, files lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 2 {
						writeRecord(t, files, launchedPID, nil, "written during the wait")
					}
					return n >= 5
				}
			},
			want:       launch.ReadyResult{Ready: true},
			wantEvents: answeredAt(5),
			elapsed:    4 * pollInterval,
		},
		"success: the limit passes without an answer": {
			timeout:    quietLimit,
			setup:      matchingRecord,
			want:       launch.ReadyResult{TimedOut: true},
			wantEvents: polls(40),
			elapsed:    quietLimit,
		},
	})
}

func TestAwaitTimeLimit(t *testing.T) {
	// lastAliveAtLimit asserts the probes and pauses of n unanswered polls, and that the last liveness check, for
	// the ticket's pid, came at the limit, after the last poll. Whether liveness may be consulted earlier as well is
	// decided by its own case below.
	lastAliveAtLimit := func(n int, limit time.Duration) func(*testing.T, outcome, []event) {
		return func(t *testing.T, _ outcome, events []event) {
			t.Helper()
			var probesAndPauses []event
			for _, e := range events {
				if e.What == "probe" || e.What == "wait "+pollInterval.String() {
					probesAndPauses = append(probesAndPauses, e)
				}
			}
			equal(t, polls(n), probesAndPauses)
			alive := only(events, "alive")
			if len(alive) == 0 {
				t.Fatal("liveness was not consulted at the limit")
			}
			equal(t, aliveAt(limit), alive[len(alive)-1])
			equal(t, aliveAt(limit), events[len(events)-1])
		}
	}
	// goneAtLimit is a process that lives until the limit and is gone when it has passed.
	goneAtLimit := func(limit time.Duration) func(time.Time) func(context.Context, int) (bool, error) {
		return func(start time.Time) func(context.Context, int) (bool, error) {
			return func(context.Context, int) (bool, error) { return time.Since(start) < limit, nil }
		}
	}
	runAwaitCases(t, map[string]awaitCase{
		"success: at the quiet limit a live process is still starting": {
			own:     ticket(),
			timeout: quietLimit,
			alive:   alwaysAlive,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			elapsed: quietLimit,
			check:   lastAliveAtLimit(40, quietLimit),
		},
		"success: at the quiet limit a process that is gone has failed": {
			own:     ticket(),
			timeout: quietLimit,
			alive:   goneAtLimit(quietLimit),
			want:    launch.ReadyResult{TimedOut: true},
			elapsed: quietLimit,
			check:   lastAliveAtLimit(40, quietLimit),
		},
		"success: the ordinary limit of 42 seconds": {
			own:     ticket(),
			timeout: ordinaryLimit,
			alive:   alwaysAlive,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			elapsed: ordinaryLimit,
			check:   lastAliveAtLimit(140, ordinaryLimit),
		},
		"success: a record of another generation does not end the wait before the limit": {
			own:     ticket(),
			timeout: quietLimit,
			setup: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, new("an earlier launch"), "an old failure"))
			},
			alive:   alwaysAlive,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			elapsed: quietLimit,
			check:   lastAliveAtLimit(40, quietLimit),
		},
		"success: liveness is judged for the ticket's own pid": {
			own:        opaqueTicket(),
			timeout:    quietLimit,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(40), aliveFor(otherPID, quietLimit)),
			elapsed:    quietLimit,
		},
		// A probe that takes time uses up the limit: with 700 ms per failed probe a poll lasts one second, so the
		// wait makes 12 polls, not 40.
		"success: failed probes that take time leave fewer polls": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(*testing.T, lifecycle.Files) func(context.Context, int) bool {
				return func(context.Context, int) bool {
					time.Sleep(700 * time.Millisecond)
					return false
				}
			},
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(slowPolls(12, time.Second, 700*time.Millisecond), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
		// A pause that lasts longer than asked uses up the limit too; each pause still asks for 300 ms.
		"success: pauses that overrun leave fewer polls": {
			own:        ticket(),
			timeout:    quietLimit,
			pauseFor:   time.Second,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(slowPolls(12, time.Second, 0), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
		// With 700 ms pauses the 18th poll starts at 11.9 s and its pause ends at 12.6 s, where the limit is observed.
		"success: the limit is observed where the pause that crosses it ends": {
			own:        ticket(),
			timeout:    quietLimit,
			pauseFor:   700 * time.Millisecond,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(slowPolls(18, 700*time.Millisecond, 0), aliveAt(12600*time.Millisecond)),
			elapsed:    12600 * time.Millisecond,
		},
		// As in the source (lib/commands.js:656-660), a poll that has started is finished: the 40th probe starts at
		// 11.7 s and fails at 12.2 s, after the limit; its full pause follows, and liveness is judged once at 12.5 s.
		"success: a failed probe that crosses the limit is followed by its pause, then liveness": {
			own:     ticket(),
			timeout: quietLimit,
			probe: func(*testing.T, lifecycle.Files) func(context.Context, int) bool {
				return func(_ context.Context, n int) bool {
					if n == 40 {
						time.Sleep(500 * time.Millisecond)
					}
					return false
				}
			},
			alive: alwaysAlive,
			want:  launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(39),
				probeAt(39),
				event{quietLimit + 200*time.Millisecond, "wait " + pollInterval.String()},
				aliveAt(quietLimit+500*time.Millisecond)),
			elapsed: quietLimit + 500*time.Millisecond,
		},
		// As in the source, a negative limit behaves like a zero one: no poll starts, then liveness is judged.
		"success: a negative limit probes nothing and judges liveness": {
			own:        ticket(),
			timeout:    -time.Second,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: []event{aliveAt(0)},
		},
		// As in the source, which does not watch the process during the wait, so a process
		// that is gone without a record is noticed only at the limit.
		"success: a process gone before the limit does not end the wait early": {
			own:     ticket(),
			timeout: quietLimit,
			alive: func(time.Time) func(context.Context, int) (bool, error) {
				return func(context.Context, int) (bool, error) { return false, nil }
			},
			want:    launch.ReadyResult{TimedOut: true},
			elapsed: quietLimit,
			check:   lastAliveAtLimit(40, quietLimit),
		},
		// As in the source, liveness is consulted once, after the last poll.
		"success: liveness is consulted once, after the last poll": {
			own:        ticket(),
			timeout:    quietLimit,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(40), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
		// As in the source, a wait with no time at all probes nothing and then judges liveness.
		"success: a zero limit probes nothing and judges liveness": {
			own:        ticket(),
			timeout:    0,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: []event{aliveAt(0)},
		},
		// As in the source, every pause is a full interval, also the one that crosses the limit, so a
		// limit of one second ends after the fourth pause, at 1.2 s.
		"success: a limit that is not a multiple of the interval ends after the pause that crosses it": {
			own:        ticket(),
			timeout:    time.Second,
			alive:      alwaysAlive,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(4), aliveAt(4*pollInterval)),
			elapsed:    4 * pollInterval,
		},
	})
}

// TestAwaitLivenessError: a liveness check that fails is reported with the zero result, whatever it claimed, after
// exactly one check of the ticket's pid at the limit.
func TestAwaitLivenessError(t *testing.T) {
	tests := map[string]struct {
		alive bool
	}{
		"error: the check fails and claims nothing":           {alive: false},
		"error: the check fails and claims the process lives": {alive: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.alive = func(context.Context, int) (bool, error) { return tt.alive, errLiveness }
				got := h.await(t.Context(), ticket(), quietLimit)
				if !errors.Is(got.err, errLiveness) {
					t.Fatalf("Await() error = %v, want one that matches %v", got.err, errLiveness)
				}
				equal(t, launch.ReadyResult{}, got.result)
				equal(t, append(polls(40), aliveAt(quietLimit)), h.logged())
			})
		})
	}
}

// TestAwaitRefusesTicket: a launch ticket needs a positive pid and a generation. A refused ticket makes no call at
// all, not even of the clock, although the probe would answer and a record matches it through the reader's wildcard.
func TestAwaitRefusesTicket(t *testing.T) {
	tests := map[string]struct {
		own launch.Ticket
	}{
		"error: zero pid":                      {own: launch.Ticket{PID: 0, Generation: generation}},
		"error: negative pid":                  {own: launch.Ticket{PID: -1, Generation: generation}},
		"error: empty generation":              {own: launch.Ticket{PID: launchedPID, Generation: ""}},
		"error: zero pid and empty generation": {own: launch.Ticket{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				// The record names the ticket's own pid and generation, or no generation when the ticket has none,
				// so the reader would match it.
				var gen *string
				if tt.own.Generation != "" {
					gen = new(tt.own.Generation)
				}
				noError(t, h.files.WriteStartupFailure(tt.own.PID, gen, "matches the refused ticket"))
				h.probe = func(context.Context, int) bool { return true }
				h.alive = alwaysAlive(h.start)
				h.forbid.Store(true)
				own := tt.own
				got := h.await(t.Context(), &own, quietLimit)
				if got.err == nil {
					t.Fatalf("Await(%+v) = %+v, nil; want an error", own, got.result)
				}
				// An error comes with the zero result.
				equal(t, launch.ReadyResult{}, got.result)
				equal(t, []event(nil), h.logged())
				equal(t, time.Duration(0), got.elapsed)
			})
		})
	}
}

// TestAwaitCancellation: the end of the caller's context ends the wait in every step with that context's error and the
// zero result, whatever the call in progress returns (an answer, a matching record, a live process), and no probe or
// liveness check follows it.
// Some mid-wait cancellation rows do not count a pause that returns immediately on the ended context. That
// allowance does not apply to a clock reading that ends the context: no callback or record lookup may follow it.
// An already-ended context allows no call at all.
func TestAwaitCancellation(t *testing.T) {
	type control struct {
		ctx    context.Context
		cancel context.CancelFunc
	}
	tests := map[string]struct {
		// prepare makes the caller's context and installs the answers that end it.
		prepare func(t *testing.T, h *harness) control
		wantErr error
		// probes and alive are the probe and liveness events expected in total.
		probes  []event
		alive   []event
		elapsed time.Duration
	}{
		// No call of any kind: not the probe, the pause, the clock or the liveness check, although the probe would
		// answer and a record matches the ticket.
		"error: a context that already ended": {
			prepare: func(t *testing.T, h *harness) control {
				matchingRecord(t, h.files)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				h.forbid.Store(true)
				h.probe = func(context.Context, int) bool { return true }
				h.alive = alwaysAlive(h.start)
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
		},
		"error: cancelled during the first probe": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.probe = func(probeCtx context.Context, n int) bool {
					cancel()
					// The probe's context carries the caller's cancellation.
					h.waitDone(probeCtx, "Probe")
					return false
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0)},
		},
		"error: cancelled during a probe that then answers": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.probe = func(probeCtx context.Context, n int) bool {
					if n < 2 {
						return false
					}
					cancel()
					h.waitDone(probeCtx, "Probe")
					return true
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0), probeAt(1)},
			elapsed: pollInterval,
		},
		"error: cancelled during a probe while a matching record is present": {
			prepare: func(t *testing.T, h *harness) control {
				matchingRecord(t, h.files)
				ctx, cancel := context.WithCancel(t.Context())
				h.probe = func(probeCtx context.Context, n int) bool {
					cancel()
					h.waitDone(probeCtx, "Probe")
					return false
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0)},
		},
		"error: cancelled during a later probe": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.probe = func(probeCtx context.Context, n int) bool {
					if n < 3 {
						return false
					}
					cancel()
					h.waitDone(probeCtx, "Probe")
					return false
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0), probeAt(1), probeAt(2)},
			elapsed: 2 * pollInterval,
		},
		"error: cancelled during a pause": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.pause = func(pauseCtx context.Context, n int, d time.Duration) error {
					if n == 2 {
						time.AfterFunc(100*time.Millisecond, cancel)
					}
					return sleep(pauseCtx, d)
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0), probeAt(1)},
			elapsed: pollInterval + 100*time.Millisecond,
		},
		"error: cancelled during a pause that returns its own error": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.pause = func(pauseCtx context.Context, n int, d time.Duration) error {
					if n == 2 {
						cancel()
						h.waitDone(pauseCtx, "Wait")
						return errPause
					}
					return sleep(pauseCtx, d)
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0), probeAt(1)},
			elapsed: pollInterval,
		},
		// The second pause ends normally and returns nil after the context ended: no probe may follow it.
		"error: the context ends in a pause before the last, which returns nil": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.pause = func(pauseCtx context.Context, n int, d time.Duration) error {
					if err := sleep(pauseCtx, d); err != nil {
						return err
					}
					if n == 2 {
						cancel()
					}
					return nil
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  []event{probeAt(0), probeAt(1)},
			elapsed: 2 * pollInterval,
		},
		"error: cancelled during a liveness check that returns its own error": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.alive = func(aliveCtx context.Context, _ int) (bool, error) {
					cancel()
					h.waitDone(aliveCtx, "Alive")
					return false, errLiveness
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  only(polls(40), "probe"),
			alive:   []event{aliveAt(quietLimit)},
			elapsed: quietLimit,
		},
		"error: cancelled during the liveness check": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.alive = func(aliveCtx context.Context, _ int) (bool, error) {
					cancel()
					h.waitDone(aliveCtx, "Alive")
					return false, aliveCtx.Err()
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  only(polls(40), "probe"),
			alive:   []event{aliveAt(quietLimit)},
			elapsed: quietLimit,
		},
		"error: cancelled during a liveness check that then reports the process alive": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.alive = func(aliveCtx context.Context, _ int) (bool, error) {
					cancel()
					h.waitDone(aliveCtx, "Alive")
					return true, nil
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  only(polls(40), "probe"),
			alive:   []event{aliveAt(quietLimit)},
			elapsed: quietLimit,
		},
		// The last pause reaches the limit and returns nil while the context ends: the liveness check must not run
		// (the harness fails the test if it does).
		"error: the context ends in the final pause, which returns nil": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.pause = func(pauseCtx context.Context, n int, d time.Duration) error {
					if err := sleep(pauseCtx, d); err != nil {
						return err
					}
					if n == 40 {
						cancel()
					}
					return nil
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  only(polls(40), "probe"),
			elapsed: quietLimit,
		},
		// The last pause ends with the caller live; the caller's context ends while the clock reading that shows the
		// limit has passed is returned. The liveness check must not run (the harness fails the test if it does).
		"error: the context ends as the clock shows the limit has passed": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithCancel(t.Context())
				h.clock = func(elapsed time.Duration) {
					if elapsed >= quietLimit {
						cancel()
					}
				}
				return control{ctx, cancel}
			},
			wantErr: context.Canceled,
			probes:  only(polls(40), "probe"),
			elapsed: quietLimit,
		},
		"error: the caller's deadline comes before the limit": {
			prepare: func(t *testing.T, h *harness) control {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				return control{ctx, cancel}
			},
			wantErr: context.DeadlineExceeded,
			probes:  []event{probeAt(0), probeAt(1), probeAt(2), probeAt(3)},
			elapsed: time.Second,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				c := tt.prepare(t, h)
				defer c.cancel()
				got := h.await(c.ctx, ticket(), quietLimit)
				if !errors.Is(got.err, tt.wantErr) {
					t.Fatalf("Await() error = %v, want one that matches %v", got.err, tt.wantErr)
				}
				equal(t, launch.ReadyResult{}, got.result)
				events := h.logged()
				equal(t, tt.probes, only(events, "probe"))
				equal(t, tt.alive, only(events, "alive"))
				equal(t, tt.elapsed, got.elapsed)
			})
		})
	}
}

// TestAwaitPauseError: an error the pause returns on its own (not the context's) ends the wait at once with that error
// and the zero result; nothing is called after it, not the probe, the pause, the clock or the liveness check.
func TestAwaitPauseError(t *testing.T) {
	tests := map[string]struct {
		own  *launch.Ticket
		fail int
	}{
		"error: the first pause fails":                    {own: ticket(), fail: 1},
		"error: a later pause fails":                      {own: ticket(), fail: 3},
		"error: a pause fails in a wait without a ticket": {fail: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.pause = func(ctx context.Context, n int, d time.Duration) error {
					if n == tt.fail {
						h.forbid.Store(true)
						return errPause
					}
					return sleep(ctx, d)
				}
				got := h.await(t.Context(), tt.own, quietLimit)
				if !errors.Is(got.err, errPause) {
					t.Fatalf("Await() error = %v, want one that matches %v", got.err, errPause)
				}
				equal(t, launch.ReadyResult{}, got.result)
				equal(t, polls(tt.fail), h.logged())
				equal(t, time.Duration(tt.fail-1)*pollInterval, got.elapsed)
			})
		})
	}
}

// TestAwaitUsesItsClock drives the wait with an injected clock that only the test's callbacks move, as the source's
// hook budget test does. The bubble's own clock never moves here, so the wait sees elapsed time only through Now: a
// failed probe or a pause that takes longer than asked must leave fewer polls, and liveness is judged where the clock
// stands when the limit is observed. The four callbacks share one call budget, so a wait that ignores the clock is
// aborted instead of spinning.
func TestAwaitUsesItsClock(t *testing.T) {
	tests := map[string]struct {
		timeout time.Duration
		// probeCost moves the clock in every probe that does not answer; pauseCost in every pause.
		probeCost, pauseCost time.Duration
		// answer is the probe, counted from one, that answers after moving the clock by answerCost; zero never.
		answer     int
		answerCost time.Duration
		want       launch.ReadyResult
		probes     int
		pauses     int
		end        time.Duration
		alive      []time.Duration
	}{
		"success: the quiet limit": {
			timeout: quietLimit, pauseCost: pollInterval,
			want: launch.ReadyResult{TimedOut: true, StillStarting: true}, probes: 40, pauses: 40, end: quietLimit,
			alive: []time.Duration{quietLimit},
		},
		"success: the ordinary limit": {
			timeout: ordinaryLimit, pauseCost: pollInterval,
			want: launch.ReadyResult{TimedOut: true, StillStarting: true}, probes: 140, pauses: 140, end: ordinaryLimit,
			alive: []time.Duration{ordinaryLimit},
		},
		"success: failed probes that take 700 ms leave 12 polls": {
			timeout: quietLimit, probeCost: 700 * time.Millisecond, pauseCost: pollInterval,
			want: launch.ReadyResult{TimedOut: true, StillStarting: true}, probes: 12, pauses: 12, end: quietLimit,
			alive: []time.Duration{quietLimit},
		},
		"success: pauses that take one second leave 12 polls": {
			timeout: quietLimit, pauseCost: time.Second,
			want: launch.ReadyResult{TimedOut: true, StillStarting: true}, probes: 12, pauses: 12, end: quietLimit,
			alive: []time.Duration{quietLimit},
		},
		"success: the limit is observed at 12.6 s after 700 ms pauses": {
			timeout: quietLimit, pauseCost: 700 * time.Millisecond,
			want: launch.ReadyResult{TimedOut: true, StillStarting: true}, probes: 18, pauses: 18, end: 12600 * time.Millisecond,
			alive: []time.Duration{12600 * time.Millisecond},
		},
		// As in the source (lib/commands.js:656-657): the 40th probe starts at 11.7 s and answers at 12.2 s.
		"success: a probe started before the limit answers after it": {
			timeout: quietLimit, pauseCost: pollInterval, answer: 40, answerCost: 500 * time.Millisecond,
			want: launch.ReadyResult{Ready: true}, probes: 40, pauses: 39, end: quietLimit + 200*time.Millisecond,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := time.Date(2026, 11, 28, 5, 0, 0, 0, time.UTC)
				var (
					mu     sync.Mutex
					now    = base
					calls  int
					probes int
					pauses []time.Duration
					alive  []time.Duration
				)
				// spend counts a call of any of the four functions, under mu, and panics once the budget is spent.
				spend := func(call string) {
					calls++
					if calls > callGuard {
						panic(budgetSpent{call})
					}
				}
				r := launch.Readiness{
					Files: lifecycle.Files{StateDir: t.TempDir()},
					Probe: func(context.Context) bool {
						mu.Lock()
						defer mu.Unlock()
						spend("Probe")
						probes++
						if tt.answer != 0 && probes == tt.answer {
							now = now.Add(tt.answerCost)
							return true
						}
						now = now.Add(tt.probeCost)
						return false
					},
					Alive: func(_ context.Context, pid int) (bool, error) {
						mu.Lock()
						defer mu.Unlock()
						spend("Alive")
						if pid != launchedPID {
							t.Errorf("liveness of pid %d, want %d", pid, launchedPID)
						}
						alive = append(alive, now.Sub(base))
						return true, nil
					},
					Now: func() time.Time {
						mu.Lock()
						defer mu.Unlock()
						spend("Now")
						return now
					},
					Wait: func(_ context.Context, d time.Duration) error {
						mu.Lock()
						defer mu.Unlock()
						spend("Wait")
						pauses = append(pauses, d)
						now = now.Add(tt.pauseCost)
						return nil
					},
				}
				got := awaitOn(t, r, t.Context(), ticket(), tt.timeout, time.Now())
				noError(t, got.err)
				equal(t, tt.want, got.result)
				mu.Lock()
				defer mu.Unlock()
				equal(t, tt.probes, probes)
				want := make([]time.Duration, tt.pauses)
				for i := range want {
					want[i] = pollInterval
				}
				equal(t, want, pauses)
				equal(t, tt.end, now.Sub(base))
				equal(t, tt.alive, alive)
			})
		})
	}
}

// steppingBack makes every reading of r's clock after the first an hour earlier than the one before it, so a wait
// that compared two readings against a limit of zero or less would see that limit still ahead.
func steppingBack(r launch.Readiness) launch.Readiness {
	inner := r.Now
	var mu sync.Mutex
	readings := 0
	r.Now = func() time.Time {
		now := inner()
		mu.Lock()
		defer mu.Unlock()
		back := time.Duration(readings) * time.Hour
		readings++
		return now.Add(-back)
	}
	return r
}

// cancelAtFirstReading makes the first reading of the harness's clock end the caller's context before it returns,
// and forbids every call after that reading, the clock included. It returns the count of readings.
func (h *harness) cancelAtFirstReading(cancel context.CancelFunc) *atomic.Int32 {
	var readings atomic.Int32
	h.clock = func(time.Duration) {
		if readings.Add(1) == 1 {
			cancel()
			h.forbid.Store(true)
		}
	}
	return &readings
}

// TestAwaitClockCancels: the clock reading that sets the deadline ends the caller's context before it returns. The
// wait returns that context's error with the zero result after exactly that one reading: no probe, pause, second
// reading or liveness check follows, although the probe would answer, a record matches the ticket and the process
// lives.
func TestAwaitClockCancels(t *testing.T) {
	tests := map[string]struct {
		own *launch.Ticket
	}{
		"error: a wait with a ticket":    {own: ticket()},
		"error: a wait without a ticket": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				matchingRecord(t, h.files)
				h.probe = func(context.Context, int) bool { return true }
				h.alive = alwaysAlive(h.start)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				readings := h.cancelAtFirstReading(cancel)
				got := h.await(ctx, tt.own, quietLimit)
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("Await() error = %v, want one that matches %v", got.err, context.Canceled)
				}
				equal(t, launch.ReadyResult{}, got.result)
				equal(t, int32(1), readings.Load())
				equal(t, []event(nil), h.logged())
				equal(t, time.Duration(0), got.elapsed)
			})
		})
	}
}

// TestAwaitNoTimeWhateverTheClock: a zero or negative limit starts no poll even when every reading of the clock after
// the first is earlier than the one before, the probe would answer and a record matches the ticket. With a ticket one
// liveness check decides the result; without one the wait times out and liveness is not consulted.
func TestAwaitNoTimeWhateverTheClock(t *testing.T) {
	tests := map[string]struct {
		own        *launch.Ticket
		timeout    time.Duration
		alive      bool
		want       launch.ReadyResult
		wantEvents []event
	}{
		"success: a zero limit finds a live process still starting": {
			own:        ticket(),
			timeout:    0,
			alive:      true,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: []event{aliveAt(0)},
		},
		"success: a negative limit finds a live process still starting": {
			own:        ticket(),
			timeout:    -time.Second,
			alive:      true,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: []event{aliveAt(0)},
		},
		"success: a zero limit finds a process that is gone": {
			own:        ticket(),
			timeout:    0,
			want:       launch.ReadyResult{TimedOut: true},
			wantEvents: []event{aliveAt(0)},
		},
		"success: a zero limit without a ticket times out without a liveness check": {
			timeout: 0,
			want:    launch.ReadyResult{TimedOut: true},
		},
		"success: a negative limit without a ticket times out without a liveness check": {
			timeout: -time.Second,
			want:    launch.ReadyResult{TimedOut: true},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				matchingRecord(t, h.files)
				h.probe = func(context.Context, int) bool { return true }
				// A wait without a ticket keeps the harness's liveness check, which fails the test.
				if tt.own != nil {
					h.alive = func(context.Context, int) (bool, error) { return tt.alive, nil }
				}
				got := awaitOn(t, steppingBack(h.readiness()), t.Context(), tt.own, tt.timeout, h.start)
				noError(t, got.err)
				equal(t, tt.want, got.result)
				equal(t, tt.wantEvents, h.logged())
				equal(t, time.Duration(0), got.elapsed)
			})
		})
	}
}

// TestAwaitTicketTakenAtEntry: the wait uses the ticket as it was when Await was called. A callback that changes the
// caller's Ticket while the wait runs changes neither the record the wait looks for nor the process whose liveness it
// judges. The reader matches any generation when asked with an empty one, so a wait that looked up a cleared
// generation would end at once with the message of another launch.
func TestAwaitTicketTakenAtEntry(t *testing.T) {
	otherGeneration := func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		noError(t, files.WriteStartupFailure(launchedPID, new("an earlier launch"), "an old failure"))
	}
	// changedPID is a record that a lookup with the changed pid and the original generation would match.
	changedPID := func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		noError(t, files.WriteStartupFailure(otherPID, new(generation), "a record of the changed pid"))
	}
	clearGeneration := func(own *launch.Ticket) { own.Generation = "" }
	changePID := func(own *launch.Ticket) { own.PID = otherPID }
	tests := map[string]struct {
		// record is written before the wait.
		record func(t *testing.T, files lifecycle.Files)
		// change alters the caller's ticket in the first probe, or in the first pause when inPause is set.
		change  func(own *launch.Ticket)
		inPause bool
		// writeAtProbe writes a record of the original pid and generation inside that probe, counted from one; zero
		// never.
		writeAtProbe int
		want         launch.ReadyResult
		wantEvents   []event
		elapsed      time.Duration
	}{
		"success: a probe that clears the generation leaves the lookup on the original generation": {
			record:       otherGeneration,
			change:       clearGeneration,
			writeAtProbe: 3,
			want:         launch.ReadyResult{Reason: bindFailure},
			wantEvents:   answeredAt(3),
			elapsed:      2 * pollInterval,
		},
		"success: a pause that clears the generation leaves the lookup on the original generation": {
			record:       otherGeneration,
			change:       clearGeneration,
			inPause:      true,
			writeAtProbe: 3,
			want:         launch.ReadyResult{Reason: bindFailure},
			wantEvents:   answeredAt(3),
			elapsed:      2 * pollInterval,
		},
		"success: a probe that changes the pid leaves the lookup and the liveness check on the original pid": {
			record:     changedPID,
			change:     changePID,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(40), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
		"success: a pause that changes the pid leaves the lookup and the liveness check on the original pid": {
			record:     changedPID,
			change:     changePID,
			inPause:    true,
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(40), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				tt.record(t, h.files)
				own := ticket()
				h.probe = func(_ context.Context, n int) bool {
					if n == 1 && !tt.inPause {
						tt.change(own)
					}
					if n == tt.writeAtProbe {
						writeRecord(t, h.files, launchedPID, new(generation), bindFailure)
					}
					return false
				}
				h.pause = func(ctx context.Context, n int, d time.Duration) error {
					if n == 1 && tt.inPause {
						tt.change(own)
					}
					return sleep(ctx, d)
				}
				h.alive = alwaysAlive(h.start)
				got := h.await(t.Context(), own, quietLimit)
				noError(t, got.err)
				equal(t, tt.want, got.result)
				equal(t, tt.wantEvents, h.logged())
				equal(t, tt.elapsed, got.elapsed)
			})
		})
	}
}

// TestAwaitTicketTakenBeforeTheClock: the first reading of the clock replaces both the pid and the generation of the
// caller's Ticket with another valid pair. The wait still uses the pair it was called with: a record of the original
// pair ends it with that record's reason, and a wait that reaches the limit judges the original pid. Each row also
// stages what the replacement pair would decide, and neither prescribes how often the clock is read.
func TestAwaitTicketTakenBeforeTheClock(t *testing.T) {
	tests := map[string]struct {
		// record is written before the wait.
		record     func(t *testing.T, files lifecycle.Files)
		want       launch.ReadyResult
		wantEvents []event
		elapsed    time.Duration
	}{
		"success: a record of the original pair ends the wait with its reason": {
			record:     matchingRecord,
			want:       launch.ReadyResult{Reason: bindFailure},
			wantEvents: answeredAt(1),
		},
		"success: at the limit liveness is judged for the original pid": {
			record: func(t *testing.T, files lifecycle.Files) {
				t.Helper()
				noError(t, files.WriteStartupFailure(otherPID, new(opaqueGeneration), "a record of the replacement pair"))
			},
			want:       launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantEvents: append(polls(40), aliveAt(quietLimit)),
			elapsed:    quietLimit,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				tt.record(t, h.files)
				own := ticket()
				var readings atomic.Int32
				h.clock = func(time.Duration) {
					if readings.Add(1) == 1 {
						*own = otherTicket
					}
				}
				h.alive = alwaysAlive(h.start)
				got := h.await(t.Context(), own, quietLimit)
				noError(t, got.err)
				equal(t, tt.want, got.result)
				equal(t, tt.wantEvents, h.logged())
				equal(t, tt.elapsed, got.elapsed)
			})
		})
	}
}
