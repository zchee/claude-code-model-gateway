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

package proxyrecovery_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// TestNew decides which options make a monitor. A managed monitor needs every collaborator it may act through; an
// unmanaged one needs only the port and the probes, because it never starts, stops or adopts anything. The journal
// is optional: a monitor without it still runs its ticks. Making a monitor calls no collaborator.
func TestNew(t *testing.T) {
	tests := map[string]struct {
		managed bool
		change  func(*proxyrecovery.Options)
		wantErr bool
		// tick runs one Recover call on the monitor made, to show that what was left out is never called.
		tick bool
	}{
		"success: managed with every collaborator": {managed: true},
		"success: unmanaged with only the port and the probes": {
			change: func(o *proxyrecovery.Options) {
				o.BinaryExists, o.Resolve, o.Start, o.StopOwned, o.Recorded, o.WaitRelease, o.Record = nil, nil, nil, nil, nil, nil, nil
			},
			tick: true,
		},
		"success: managed without a journal": {
			managed: true,
			change:  func(o *proxyrecovery.Options) { o.Record = nil },
			tick:    true,
		},
		"error: managed with port zero":          {managed: true, change: func(o *proxyrecovery.Options) { o.Port = 0 }, wantErr: true},
		"error: unmanaged with port zero":        {change: func(o *proxyrecovery.Options) { o.Port = 0 }, wantErr: true},
		"error: managed without a probe":         {managed: true, change: func(o *proxyrecovery.Options) { o.Probe = nil }, wantErr: true},
		"error: unmanaged without a probe":       {change: func(o *proxyrecovery.Options) { o.Probe = nil }, wantErr: true},
		"error: managed without a listener read": {managed: true, change: func(o *proxyrecovery.Options) { o.Listening = nil }, wantErr: true},
		"error: managed without a binary check":  {managed: true, change: func(o *proxyrecovery.Options) { o.BinaryExists = nil }, wantErr: true},
		"error: managed without an owner lookup": {managed: true, change: func(o *proxyrecovery.Options) { o.Resolve = nil }, wantErr: true},
		"error: managed without a start":         {managed: true, change: func(o *proxyrecovery.Options) { o.Start = nil }, wantErr: true},
		"error: managed without a stop":          {managed: true, change: func(o *proxyrecovery.Options) { o.StopOwned = nil }, wantErr: true},
		"error: managed without a record read":   {managed: true, change: func(o *proxyrecovery.Options) { o.Recorded = nil }, wantErr: true},
		"error: managed without a release check": {managed: true, change: func(o *proxyrecovery.Options) { o.WaitRelease = nil }, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				o := w.options(tt.managed)
				if tt.change != nil {
					tt.change(&o)
				}
				m, err := construct(t, o)
				// Whether New accepts or refuses the options, it calls no collaborator and does not read the clock.
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("New called %q", got)
				}
				if n := w.reads(); n != 0 {
					t.Errorf("New read the clock %d times", n)
				}
				if tt.wantErr {
					if err == nil {
						t.Fatal("New accepted the options")
					}
					if m != nil {
						t.Errorf("New returned a monitor beside its error %v", err)
					}
					return
				}
				if err != nil || m == nil {
					t.Fatalf("New = %v, %v; want a monitor", m, err)
				}
				if tt.tick {
					res := tick(t, m)
					t.Logf("one tick: %+v; calls %q", res, w.sequence())
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestWithoutJournal decides that a monitor without Record behaves as one with it: the same results and the same
// calls of every other collaborator, tick for tick.
func TestWithoutJournal(t *testing.T) {
	tests := map[string]struct {
		setup func(w *world)
	}{
		"success: a holder this root's record names is replaced": {
			setup: func(w *world) {
				w.answers(false, false, true, false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
			},
		},
		"success: a holder that cannot be established is refused and retried": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(true)
			},
		},
		"success: a missing binary is retried on its schedule": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(false)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
		},
	}
	// run ticks a monitor at the same moments with and without the journal and returns its results, its calls
	// without the journal's, and the records it stopped.
	run := func(t *testing.T, setup func(w *world), journal bool) ([]proxyrecovery.Result, []string, []procinfo.Record) {
		w := newWorld(t)
		setup(w)
		o := w.options(true)
		if !journal {
			o.Record = nil
		}
		m := newMonitor(t, o)
		start := w.now()
		var results []proxyrecovery.Result
		for _, at := range []time.Duration{0, time.Second, 3 * time.Second, 40 * time.Second} {
			w.until(start.Add(at))
			res := tick(t, m)
			// Retry times are compared relative to the monitor's start: the two runs share no clock.
			if !res.RetryAt.IsZero() {
				res.RetryAt = time.Time{}.Add(res.RetryAt.Sub(start))
			}
			results = append(results, res)
		}
		if err := stop(t, m); err != nil {
			t.Errorf("Stop: %v", err)
		}
		calls := slices.DeleteFunc(w.sequence(), func(c string) bool { return strings.HasPrefix(c, "record:") })
		return results, calls, w.stops()
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				withResults, withCalls, withStops := run(t, tt.setup, true)
				results, calls, stops := run(t, tt.setup, false)
				if diff := gocmp.Diff(withResults, results); diff != "" {
					t.Errorf("results without the journal (-with +without):\n%s", diff)
				}
				if diff := gocmp.Diff(withCalls, calls); diff != "" {
					t.Errorf("calls without the journal (-with +without):\n%s", diff)
				}
				if diff := gocmp.Diff(withStops, stops); diff != "" {
					t.Errorf("records stopped without the journal (-with +without):\n%s", diff)
				}
			})
		})
	}
}
