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

//go:build linux

package launch_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/launch"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// recordOpens watches the state root with inotify and counts the opens of the startup-failure record. The kernel
// queues an event when the open happens, so a drain after a call returns has seen every open that call made. The
// watch also asks for the close of a file opened without writing: the kernel merges an event only into an identical
// one at the tail of the queue, so with a close between them two reads of the record stay two opens.
type recordOpens struct {
	t  *testing.T
	fd int
}

func watchRecordOpens(t *testing.T, stateDir string) *recordOpens {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	noError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	_, err = unix.InotifyAddWatch(fd, stateDir, unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
	noError(t, err)
	return &recordOpens{t: t, fd: fd}
}

// drain returns the number of opens of the record queued since the last drain. It may run on a callback's
// goroutine, so it reports a failure with Errorf.
func (w *recordOpens) drain() int {
	opens := 0
	buf := make([]byte, 64*1024)
	for {
		n, err := unix.Read(w.fd, buf)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EAGAIN):
			return opens
		case err != nil:
			w.t.Errorf("reading inotify events: %v", err)
			return opens
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			length := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := string(bytes.TrimRight(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+length], "\x00"))
			if mask&unix.IN_Q_OVERFLOW != 0 {
				w.t.Errorf("the inotify queue overflowed; opens were lost")
			}
			if mask&unix.IN_OPEN != 0 && name == failureName {
				opens++
			}
			off += unix.SizeofInotifyEvent + length
		}
	}
}

// readLog is the ordered log of the wait's calls and of the record's opens between them.
type readLog struct {
	watch   *recordOpens
	entries []string
}

// note drains the opens queued so far into the log, then appends the call (none when call is empty).
func (l *readLog) note(call string) {
	for range l.watch.drain() {
		l.entries = append(l.entries, "open")
	}
	if call != "" {
		l.entries = append(l.entries, call)
	}
}

// discard drops the opens queued so far: those of the test's own write of a record.
func (l *readLog) discard() { l.watch.drain() }

// decision says which call decides the outcome of a case.
type decision int

const (
	// noDecision: the wait must never open the record.
	noDecision decision = iota
	// byAnswer: the answering probe is the last entry; no open, pause or liveness check follows it.
	byAnswer
	// byRecord: the probe of the poll that finds the matching record is followed by exactly one open, then nothing.
	byRecord
	// byLimit: at least one poll ran, and one liveness check is the last entry, directly after the last pause.
	byLimit
	// bySkippedLoop: a limit that starts no poll; the log is exactly one liveness check.
	bySkippedLoop
	// byPause: the pause that failed on its own is the last entry; no open or call follows it.
	byPause
)

// TestAwaitRecordReads proves with the kernel's own record of opens when the wait reads the startup-failure record:
// never without a ticket, for a refused ticket, for an already-ended context or for a limit that starts no poll; only
// after a probe of the same poll, never after a pause or before the first probe; and never after the call that
// decided the outcome, which happens exactly once.
func TestAwaitRecordReads(t *testing.T) {
	otherGeneration := func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		noError(t, files.WriteStartupFailure(launchedPID, new("an earlier launch"), "an old failure"))
	}
	tests := map[string]struct {
		own     *launch.Ticket
		timeout time.Duration
		// record is written before the watch starts.
		record func(t *testing.T, files lifecycle.Files)
		// ended runs the wait with a context that has already ended.
		ended bool
		// answer is the first probe that answers, counted from one; zero never answers.
		answer int
		// writeAtProbe writes a matching record inside that probe, which then does not answer.
		writeAtProbe int
		// publishInPause writes a matching record at the end of that pause, which then returns nil; the case fails when
		// the wait never reaches that pause.
		publishInPause int
		// failPause makes that pause fail on its own.
		failPause int
		want      launch.ReadyResult
		wantErr   bool
		decided   decision
		// deciding is the probe that decides a byAnswer or byRecord case, or the pause that fails in a byPause case,
		// counted from one.
		deciding int
	}{
		// The record appears in the second pause; the third probe answers before any read could see it.
		"success: a record published during an earlier pause does not beat the next probe's answer": {
			own:            ticket(),
			timeout:        quietLimit,
			record:         otherGeneration,
			publishInPause: 2,
			answer:         3,
			want:           launch.ReadyResult{Ready: true},
			decided:        byAnswer,
			deciding:       3,
		},
		"success: a pause that fails on its own is followed by no open": {
			own:       ticket(),
			timeout:   quietLimit,
			record:    otherGeneration,
			failPause: 2,
			wantErr:   true,
			decided:   byPause,
			deciding:  2,
		},
		"success: a zero limit leaves a matching record unopened": {
			own:     ticket(),
			timeout: 0,
			record:  matchingRecord,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			decided: bySkippedLoop,
		},
		"success: a negative limit leaves a matching record unopened": {
			own:     ticket(),
			timeout: -time.Second,
			record:  matchingRecord,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			decided: bySkippedLoop,
		},
		"success: a wait without a ticket never opens a record a launch would match": {
			timeout: quietLimit,
			record:  matchingRecord,
			answer:  4,
			want:    launch.ReadyResult{Ready: true},
			decided: noDecision,
		},
		"success: a wait without a ticket that reaches the limit never opens the record": {
			timeout: quietLimit,
			record: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, nil, "no generation recorded"))
			},
			want:    launch.ReadyResult{TimedOut: true},
			decided: noDecision,
		},
		"success: an answering first probe leaves a matching record unopened": {
			own:      ticket(),
			timeout:  quietLimit,
			record:   matchingRecord,
			answer:   1,
			want:     launch.ReadyResult{Ready: true},
			decided:  byAnswer,
			deciding: 1,
		},
		"success: no open after a later probe answered": {
			own:      ticket(),
			timeout:  quietLimit,
			record:   otherGeneration,
			answer:   4,
			want:     launch.ReadyResult{Ready: true},
			decided:  byAnswer,
			deciding: 4,
		},
		"success: the matching record is read once and nothing follows": {
			own:          ticket(),
			timeout:      quietLimit,
			record:       otherGeneration,
			writeAtProbe: 3,
			want:         launch.ReadyResult{Reason: bindFailure},
			decided:      byRecord,
			deciding:     3,
		},
		"success: no open after the liveness check at the limit": {
			own:     ticket(),
			timeout: quietLimit,
			record:  otherGeneration,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			decided: byLimit,
		},
		// The loop ends with that pause, so the record is not read again before the liveness check.
		"success: a record published during the final pause is not read before the liveness check": {
			own:            ticket(),
			timeout:        quietLimit,
			record:         otherGeneration,
			publishInPause: 40,
			want:           launch.ReadyResult{TimedOut: true, StillStarting: true},
			decided:        byLimit,
		},
		"success: a refused ticket opens nothing": {
			own:     &launch.Ticket{PID: launchedPID},
			timeout: quietLimit,
			record: func(t *testing.T, files lifecycle.Files) {
				noError(t, files.WriteStartupFailure(launchedPID, nil, "matches through the wildcard"))
			},
			wantErr: true,
			decided: noDecision,
		},
		"success: an already-ended context opens nothing": {
			own:     ticket(),
			timeout: quietLimit,
			record:  matchingRecord,
			ended:   true,
			wantErr: true,
			decided: noDecision,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				tt.record(t, h.files)
				watch := watchRecordOpens(t, h.files.StateDir)
				// The watch must see an open of the record: the test opens it once itself before the wait.
				control, err := os.Open(filepath.Join(h.files.StateDir, failureName))
				noError(t, err)
				noError(t, control.Close())
				if got := watch.drain(); got != 1 {
					t.Fatalf("control: the watch counted %d opens of the record after the test opened it once, want 1", got)
				}
				l := &readLog{watch: watch}
				published := false
				h.probe = func(_ context.Context, n int) bool {
					l.note("probe " + strconv.Itoa(n))
					if n == tt.writeAtProbe {
						writeRecord(t, h.files, launchedPID, new(generation), bindFailure)
						l.discard()
						return false
					}
					return tt.answer != 0 && n >= tt.answer
				}
				h.pause = func(ctx context.Context, n int, d time.Duration) error {
					l.note("wait " + strconv.Itoa(n))
					if n == tt.failPause {
						h.forbid.Store(true)
						return errPause
					}
					if err := sleep(ctx, d); err != nil {
						return err
					}
					if n == tt.publishInPause {
						writeRecord(t, h.files, launchedPID, new(generation), bindFailure)
						l.discard()
						published = true
					}
					return nil
				}
				// A wait without a valid ticket keeps the harness's liveness check, which fails the test.
				if tt.own != nil && tt.own.PID > 0 && tt.own.Generation != "" {
					h.alive = func(context.Context, int) (bool, error) {
						l.note("alive")
						return true, nil
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.ended {
					cancel()
					h.forbid.Store(true)
				}
				got := h.await(ctx, tt.own, tt.timeout)
				l.note("")
				if tt.wantErr {
					if got.err == nil {
						t.Fatalf("Await() = %+v, nil error; want an error", got.result)
					}
				} else {
					noError(t, got.err)
				}
				equal(t, tt.want, got.result)
				if tt.publishInPause != 0 && !published {
					t.Fatalf("the wait never completed pause %d, where the record is published; log %q", tt.publishInPause, l.entries)
				}
				checkReads(t, tt.decided, tt.deciding, l.entries)
			})
		})
	}
}

// checkReads applies a case's rule to the ordered log. In every case an open may only follow the probe of its own
// poll (or another open after it): never the start of the wait, a pause or a liveness check.
func checkReads(t *testing.T, decided decision, deciding int, entries []string) {
	t.Helper()
	opens := 0
	last := "the start of the wait"
	for _, e := range entries {
		if e != "open" {
			last = e
			continue
		}
		opens++
		if !strings.HasPrefix(last, "probe ") {
			t.Fatalf("an open of the record follows %q, want it only after a probe of the same poll; log %q", last, entries)
		}
	}
	switch decided {
	case noDecision:
		if opens != 0 {
			t.Fatalf("the record was opened %d times, want never; log %q", opens, entries)
		}
	case byAnswer:
		if at := slices.Index(entries, "probe "+strconv.Itoa(deciding)); at < 0 || at != len(entries)-1 {
			t.Fatalf("want the answering probe %d as the last entry; log %q", deciding, entries)
		}
	case byRecord:
		at := slices.Index(entries, "probe "+strconv.Itoa(deciding))
		if at < 0 {
			t.Fatalf("probe %d is missing; log %q", deciding, entries)
		}
		if diff := gocmp.Diff([]string{"open"}, entries[at+1:]); diff != "" {
			t.Fatalf("after probe %d want exactly one open of the record and nothing else (-want +got):\n%s\nlog %q", deciding, diff, entries)
		}
	case byLimit:
		n := len(entries)
		if n < 2 || entries[n-1] != "alive" || slices.Index(entries, "alive") != n-1 {
			t.Fatalf("want at least one poll and exactly one liveness check, as the last entry; log %q", entries)
		}
		if !strings.HasPrefix(entries[n-2], "wait ") {
			t.Fatalf("want the last pause directly before the liveness check, with no open between them; log %q", entries)
		}
	case bySkippedLoop:
		if diff := gocmp.Diff([]string{"alive"}, entries); diff != "" {
			t.Fatalf("a limit that starts no poll: want exactly one liveness check and nothing else (-want +got):\n%s", diff)
		}
	case byPause:
		if at := slices.Index(entries, "wait "+strconv.Itoa(deciding)); at < 0 || at != len(entries)-1 {
			t.Fatalf("want the failing pause %d as the last entry, with no open or call after it; log %q", deciding, entries)
		}
	}
}

// TestAwaitClockRecordReads proves with the kernel's record of opens that a matching record stays unopened when the
// clock leaves no poll to run: a zero or negative limit under a clock whose every reading after the first is earlier
// than the one before, and a first reading that ends the caller's context before it returns. The probe would answer
// in every case. A wait without a ticket makes no call and opens nothing in each of these cases.
func TestAwaitClockRecordReads(t *testing.T) {
	tests := map[string]struct {
		own     *launch.Ticket
		timeout time.Duration
		// cancelAtClock ends the caller's context in the first reading of the clock, which otherwise steps back.
		cancelAtClock bool
		want          launch.ReadyResult
		wantErr       bool
		wantLog       []string
	}{
		"success: a zero limit under a clock that steps back leaves a matching record unopened": {
			own:     ticket(),
			timeout: 0,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantLog: []string{"alive"},
		},
		"success: a negative limit under a clock that steps back leaves a matching record unopened": {
			own:     ticket(),
			timeout: -time.Second,
			want:    launch.ReadyResult{TimedOut: true, StillStarting: true},
			wantLog: []string{"alive"},
		},
		"success: a clock reading that ends the caller's context is followed by no open": {
			own:           ticket(),
			timeout:       quietLimit,
			cancelAtClock: true,
			wantErr:       true,
		},
		"success: a wait without a ticket at a zero limit under a clock that steps back opens nothing": {
			timeout: 0,
			want:    launch.ReadyResult{TimedOut: true},
		},
		"success: a wait without a ticket at a negative limit under a clock that steps back opens nothing": {
			timeout: -time.Second,
			want:    launch.ReadyResult{TimedOut: true},
		},
		"success: a wait without a ticket whose clock reading ends the caller's context opens nothing": {
			timeout:       quietLimit,
			cancelAtClock: true,
			wantErr:       true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				matchingRecord(t, h.files)
				watch := watchRecordOpens(t, h.files.StateDir)
				// The watch must see an open of the record: the test opens it once itself before the wait.
				control, err := os.Open(filepath.Join(h.files.StateDir, failureName))
				noError(t, err)
				noError(t, control.Close())
				if got := watch.drain(); got != 1 {
					t.Fatalf("control: the watch counted %d opens of the record after the test opened it once, want 1", got)
				}
				l := &readLog{watch: watch}
				h.probe = func(_ context.Context, n int) bool {
					l.note("probe " + strconv.Itoa(n))
					return true
				}
				h.pause = func(_ context.Context, n int, _ time.Duration) error {
					l.note("wait " + strconv.Itoa(n))
					return nil
				}
				// A wait without a ticket keeps the harness's liveness check, which fails the test.
				if tt.own != nil {
					h.alive = func(context.Context, int) (bool, error) {
						l.note("alive")
						return true, nil
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := h.readiness()
				if tt.cancelAtClock {
					h.cancelAtFirstReading(cancel)
				} else {
					r = steppingBack(r)
				}
				got := awaitOn(t, r, ctx, tt.own, tt.timeout, h.start)
				l.note("")
				if tt.wantErr {
					if !errors.Is(got.err, context.Canceled) {
						t.Fatalf("Await() error = %v, want one that matches %v", got.err, context.Canceled)
					}
				} else {
					noError(t, got.err)
				}
				equal(t, tt.want, got.result)
				if diff := gocmp.Diff(tt.wantLog, l.entries); diff != "" {
					t.Fatalf("calls and opens of the record (-want +got):\n%s", diff)
				}
			})
		})
	}
}
