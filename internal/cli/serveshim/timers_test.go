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

package serveshim

import (
	"math"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// timerTLongest is the longest timer the source accepts, 2147483647 ms.
const timerTLongest = 2147483647 * time.Millisecond

// timerTPost is the drain request's bound and whether the request is sent at all.
type timerTPost struct {
	Budget time.Duration
	Sent   bool
}

// timerTDrain is everything a restart derives from one drain timeout.
type timerTDrain struct {
	Timer   time.Duration
	Post    timerTPost
	Seconds string
}

// timerTRecovery is the proxy recovery cadence and whether the configured value was replaced by the default.
type timerTRecovery struct {
	Interval time.Duration
	Replaced bool
}

// timerTCall runs one conversion and turns a panic into a failure of the calling row only.
func timerTCall[T any](t *testing.T, name string, f func() T) (got T, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", name, r)
			ok = false
		}
	}()
	return f(), true
}

// timerTCheckDrain compares each of the three drain conversions of ms with want, separately.
func timerTCheckDrain(t *testing.T, ms float64, want timerTDrain) {
	t.Helper()
	if got, ok := timerTCall(t, "drainTimer", func() time.Duration { return drainTimer(ms) }); ok {
		if diff := gocmp.Diff(want.Timer, got); diff != "" {
			t.Errorf("drainTimer(%v) mismatch (-want +got):\n%s", ms, diff)
		}
	}
	if got, ok := timerTCall(t, "drainPostBudget", func() timerTPost {
		budget, sent := drainPostBudget(ms)
		return timerTPost{Budget: budget, Sent: sent}
	}); ok {
		if diff := gocmp.Diff(want.Post, got); diff != "" {
			t.Errorf("drainPostBudget(%v) mismatch (-want +got):\n%s", ms, diff)
		}
	}
	if got, ok := timerTCall(t, "drainSeconds", func() string { return drainSeconds(ms) }); ok {
		if diff := gocmp.Diff(want.Seconds, got); diff != "" {
			t.Errorf("drainSeconds(%v) mismatch (-want +got):\n%s", ms, diff)
		}
	}
}

// timerTCheckRecovery compares the recovery conversion of ms with want.
func timerTCheckRecovery(t *testing.T, ms float64, want timerTRecovery) {
	t.Helper()
	if got, ok := timerTCall(t, "recoveryInterval", func() timerTRecovery {
		interval, replaced := recoveryInterval(ms)
		return timerTRecovery{Interval: interval, Replaced: replaced}
	}); ok {
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Errorf("recoveryInterval(%v) mismatch (-want +got):\n%s", ms, diff)
		}
	}
}

// timerTLookup is an environment holding exactly one variable.
func timerTLookup(variable, value string) config.LookupEnv {
	return func(key string) (string, bool) {
		if key == variable {
			return value, true
		}
		return "", false
	}
}

func TestRestartDrainTimers(t *testing.T) {
	t.Parallel()

	t.Run("drain timeout", func(t *testing.T) {
		t.Parallel()

		tests := map[string]struct {
			ms   float64
			want timerTDrain
		}{
			"success: a timeout below minus one second sends no request": {
				ms:   -1001,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "-1"},
			},
			"success: a request budget below zero sends no request although it truncates to zero": {
				ms:   -1000.5,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "-1"},
			},
			"success: a timeout of minus one second bounds the request at one second although the source sets no bound": {
				ms:   -1000,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "-1"},
			},
			"success: a request budget between zero and one millisecond becomes one millisecond": {
				ms:   -999.5,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Millisecond, Sent: true}, Seconds: "0"},
			},
			"success: a negative fraction is added to one second before the budget is truncated": {
				ms:   -1.5,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: 998 * time.Millisecond, Sent: true}, Seconds: "0"},
			},
			"success: minus one millisecond prints its seconds as zero": {
				ms:   -1,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: 999 * time.Millisecond, Sent: true}, Seconds: "0"},
			},
			"success: negative zero after configuration prints as zero": {
				ms:   math.Copysign(0, -1),
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "0"},
			},
			"success: zero": {
				ms:   0,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "0"},
			},
			"success: the smallest positive timeout prints its seconds as zero": {
				ms:   math.SmallestNonzeroFloat64,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "0"},
			},
			"success: a fraction below one millisecond gives the shortest timer": {
				ms:   0.5,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "1"},
			},
			"success: just below one millisecond gives the shortest timer": {
				ms:   0.999,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Second, Sent: true}, Seconds: "1"},
			},
			"success: one millisecond": {
				ms:   1,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: 1001 * time.Millisecond, Sent: true}, Seconds: "1"},
			},
			"success: a fraction is truncated, never rounded": {
				ms:   1.9,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: 1001 * time.Millisecond, Sent: true}, Seconds: "1"},
			},
			"success: the printed seconds round up the timeout itself, not the truncated timer": {
				ms:   1000.5,
				want: timerTDrain{Timer: time.Second, Post: timerTPost{Budget: 2000 * time.Millisecond, Sent: true}, Seconds: "2"},
			},
			"success: just above one second rounds the printed seconds up": {
				ms:   1001,
				want: timerTDrain{Timer: 1001 * time.Millisecond, Post: timerTPost{Budget: 2001 * time.Millisecond, Sent: true}, Seconds: "2"},
			},
			"success: not a number sends no request": {
				ms:   math.NaN(),
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "NaN"},
			},
			"success: positive infinity sends no request": {
				ms:   math.Inf(1),
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "Infinity"},
			},
			"success: negative infinity sends no request": {
				ms:   math.Inf(-1),
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "-Infinity"},
			},
			"success: the configuration's fallback": {
				ms:   30000,
				want: timerTDrain{Timer: 30 * time.Second, Post: timerTPost{Budget: 31000 * time.Millisecond, Sent: true}, Seconds: "30"},
			},
			"success: a request budget one millisecond below the longest timer is kept": {
				ms:   2147482646,
				want: timerTDrain{Timer: 2147482646 * time.Millisecond, Post: timerTPost{Budget: 2147483646 * time.Millisecond, Sent: true}, Seconds: "2147483"},
			},
			"success: a request budget exactly at the longest timer is kept": {
				ms:   2147482647,
				want: timerTDrain{Timer: 2147482647 * time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147483"},
			},
			"success: a request budget half a millisecond above the longest timer is capped": {
				ms:   2147482647.5,
				want: timerTDrain{Timer: 2147482647 * time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147483"},
			},
			"success: a request budget one millisecond above the longest timer is capped": {
				ms:   2147482648,
				want: timerTDrain{Timer: 2147482648 * time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147483"},
			},
			"success: a fraction just below the longest timer is truncated": {
				ms:   2147483646.9,
				want: timerTDrain{Timer: 2147483646 * time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147484"},
			},
			"success: the longest timer": {
				ms:   2147483647,
				want: timerTDrain{Timer: timerTLongest, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147484"},
			},
			"success: half a millisecond above the longest timer gives the shortest timer": {
				ms:   2147483647.5,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147484"},
			},
			"success: one millisecond above the longest timer gives the shortest timer": {
				ms:   2147483648,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "2147484"},
			},
			"success: a timeout of 1e21 milliseconds prints its seconds as plain digits": {
				ms:   1e21,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1000000000000000000"},
			},
			"success: seconds below 1e21 print their shortest decimal, not their exact binary value": {
				ms:   1.0000000000000001e21,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1000000000000000100"},
			},
			"success: seconds just below 1e21 print every digit": {
				ms:   9.99e23,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "999000000000000000000"},
			},
			"success: seconds of exactly 1e21 print in exponent form": {
				ms:   1e24,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1e+21"},
			},
			"success: a large timeout prints its seconds in exponent form": {
				ms:   1e25,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1e+22"},
			},
			"success: a large timeout with a fraction in its exponent form": {
				ms:   1.2345e25,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1.2345e+22"},
			},
			"success: the largest finite timeout": {
				ms:   math.MaxFloat64,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1.7976931348623156e+305"},
			},
			"success: a large negative timeout sends no request and prints in exponent form": {
				ms:   -1e24,
				want: timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "-1e+21"},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				timerTCheckDrain(t, tt.ms, tt.want)
			})
		}
	})

	t.Run("drain timeout from the environment", func(t *testing.T) {
		t.Parallel()

		const variable = "CODEX_GATEWAY_DRAIN_TIMEOUT_MS"
		fallback := timerTDrain{Timer: 30 * time.Second, Post: timerTPost{Budget: 31000 * time.Millisecond, Sent: true}, Seconds: "30"}
		tests := map[string]struct {
			lookup config.LookupEnv
			ms     float64
			want   timerTDrain
		}{
			"success: unset gives the fallback": {
				lookup: nil,
				ms:     30000,
				want:   fallback,
			},
			"success: empty gives the fallback": {
				lookup: timerTLookup(variable, ""),
				ms:     30000,
				want:   fallback,
			},
			"success: zero gives the fallback": {
				lookup: timerTLookup(variable, "0"),
				ms:     30000,
				want:   fallback,
			},
			"success: negative zero before configuration gives the fallback": {
				lookup: timerTLookup(variable, "-0"),
				ms:     30000,
				want:   fallback,
			},
			"success: not a number gives the fallback": {
				lookup: timerTLookup(variable, "soon"),
				ms:     30000,
				want:   fallback,
			},
			"success: a timeout below minus one second is kept and sends no request": {
				lookup: timerTLookup(variable, "-1001"),
				ms:     -1001,
				want:   timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "-1"},
			},
			"success: a negative fraction is kept and bounds the request at one millisecond": {
				lookup: timerTLookup(variable, "-999.5"),
				ms:     -999.5,
				want:   timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: time.Millisecond, Sent: true}, Seconds: "0"},
			},
			"success: infinity is kept and sends no request": {
				lookup: timerTLookup(variable, "Infinity"),
				ms:     math.Inf(1),
				want:   timerTDrain{Timer: time.Millisecond, Post: timerTPost{}, Seconds: "Infinity"},
			},
			"success: a large exponent spelling is kept": {
				lookup: timerTLookup(variable, "1e25"),
				ms:     1e25,
				want:   timerTDrain{Timer: time.Millisecond, Post: timerTPost{Budget: timerTLongest, Sent: true}, Seconds: "1e+22"},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ms := config.DrainTimeout.Get(tt.lookup)
				if diff := gocmp.Diff(tt.ms, ms); diff != "" {
					t.Fatalf("config.DrainTimeout mismatch (-want +got):\n%s", diff)
				}
				timerTCheckDrain(t, ms, tt.want)
			})
		}
	})

	t.Run("proxy recovery interval", func(t *testing.T) {
		t.Parallel()

		tests := map[string]struct {
			ms   float64
			want timerTRecovery
		}{
			"success: the configuration's minimum": {
				ms:   1000,
				want: timerTRecovery{Interval: time.Second},
			},
			"success: a fraction above the minimum is truncated, never rounded": {
				ms:   1000.9,
				want: timerTRecovery{Interval: time.Second},
			},
			"success: the default": {
				ms:   5000,
				want: timerTRecovery{Interval: 5 * time.Second},
			},
			"success: a fraction above the default is truncated": {
				ms:   5000.9,
				want: timerTRecovery{Interval: 5 * time.Second},
			},
			"success: a fraction just below the longest timer is truncated": {
				ms:   2147483646.9,
				want: timerTRecovery{Interval: 2147483646 * time.Millisecond},
			},
			"success: the longest timer is kept": {
				ms:   2147483647,
				want: timerTRecovery{Interval: timerTLongest},
			},
			"success: half a millisecond above the longest timer is replaced by the default": {
				ms:   2147483647.5,
				want: timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: one millisecond above the longest timer is replaced by the default": {
				ms:   2147483648,
				want: timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: a large value is replaced by the default": {
				ms:   1e300,
				want: timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: the largest finite value is replaced by the default": {
				ms:   math.MaxFloat64,
				want: timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: positive infinity is replaced by the default": {
				ms:   math.Inf(1),
				want: timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				timerTCheckRecovery(t, tt.ms, tt.want)
			})
		}
	})

	t.Run("proxy recovery interval from the environment", func(t *testing.T) {
		t.Parallel()

		const variable = "CODEX_GATEWAY_PROXY_RECOVERY_INTERVAL_MS"
		tests := map[string]struct {
			lookup config.LookupEnv
			ms     float64
			want   timerTRecovery
		}{
			"success: unset gives the default": {
				lookup: nil,
				ms:     5000,
				want:   timerTRecovery{Interval: 5 * time.Second},
			},
			"success: negative zero gives the default": {
				lookup: timerTLookup(variable, "-0"),
				ms:     5000,
				want:   timerTRecovery{Interval: 5 * time.Second},
			},
			"success: a value below one second is raised to one second": {
				lookup: timerTLookup(variable, "0.5"),
				ms:     1000,
				want:   timerTRecovery{Interval: time.Second},
			},
			"success: negative infinity is raised to one second and not replaced": {
				lookup: timerTLookup(variable, "-Infinity"),
				ms:     1000,
				want:   timerTRecovery{Interval: time.Second},
			},
			"success: a configured fraction is truncated": {
				lookup: timerTLookup(variable, "5000.9"),
				ms:     5000.9,
				want:   timerTRecovery{Interval: 5 * time.Second},
			},
			"success: the longest timer is kept": {
				lookup: timerTLookup(variable, "2147483647"),
				ms:     2147483647,
				want:   timerTRecovery{Interval: timerTLongest},
			},
			"success: one millisecond above the longest timer is replaced by the default": {
				lookup: timerTLookup(variable, "2147483648"),
				ms:     2147483648,
				want:   timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: infinity is replaced by the default": {
				lookup: timerTLookup(variable, "Infinity"),
				ms:     math.Inf(1),
				want:   timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
			"success: an exponent beyond the float range is replaced by the default": {
				lookup: timerTLookup(variable, "1e999"),
				ms:     math.Inf(1),
				want:   timerTRecovery{Interval: 5 * time.Second, Replaced: true},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ms := config.ProxyRecovery.Get(tt.lookup)
				if diff := gocmp.Diff(tt.ms, ms); diff != "" {
					t.Fatalf("config.ProxyRecovery mismatch (-want +got):\n%s", diff)
				}
				timerTCheckRecovery(t, ms, tt.want)
			})
		}
	})
}
