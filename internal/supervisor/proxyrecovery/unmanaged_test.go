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
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// TestUnmanaged decides the unmanaged mode that shadow mode uses: a tick probes and reports healthy or proxy-down, and
// whatever the probes say it only probes and samples the listener: it never checks the binary, looks an owner up,
// reads a record, stops, waits for a release or starts, and journals no recovery, neither in a tick nor in Stop.
// After Stop it calls nothing at all.
func TestUnmanaged(t *testing.T) {
	tests := map[string]struct {
		setup func(w *world)
		state proxyrecovery.State
	}{
		"success: a proxy that answers is healthy": {
			setup: func(w *world) { w.answers(true); w.listens(true) },
			state: proxyrecovery.Healthy,
		},
		"success: nothing listening is proxy-down": {
			setup: func(w *world) { w.answers(false); w.listens(false) },
			state: proxyrecovery.ProxyDown,
		},
		"success: an unhealthy listener this root's record names is left alone": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
			},
			state: proxyrecovery.ProxyDown,
		},
		"success: a proven foreign holder does not halt the probes": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Foreign, PID: 4242}, nil)
			},
			state: proxyrecovery.ProxyDown,
		},
	}
	permitted := []string{"probe", "listening"}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)
				m := newMonitor(t, w.options(false))
				// Every tick probes again, at once and past any grace or backoff the managed mode would apply.
				for _, wait := range []time.Duration{0, 0, time.Second, 31 * time.Second, time.Hour} {
					time.Sleep(wait)
					probes := w.count("probe")
					if res := tick(t, m); res.State != tt.state {
						t.Errorf("tick after %v = %+v, want %q", wait, res, tt.state)
					}
					if w.count("probe") == probes {
						t.Errorf("the tick after %v did not probe", wait)
					}
				}
				for _, call := range w.sequence() {
					if !slices.Contains(permitted, call) {
						t.Errorf("an unmanaged tick called %s", call)
					}
				}
				if got := w.journal(); len(got) != 0 {
					t.Errorf("the unmanaged monitor journaled %v", got)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("Stop of the unmanaged monitor called %q", got)
				}
				if res, err := goRecover(m, t.Context()).result(t, "Recover after Stop"); err != nil || res.State != proxyrecovery.Stopped {
					t.Errorf("Recover after Stop = %+v, %v; want stopped", res, err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("Recover after Stop called %q", got)
				}
				if got := w.journal(); len(got) != 0 {
					t.Errorf("Stop of the unmanaged monitor journaled %v", got)
				}
			})
		})
	}
}
