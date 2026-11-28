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
	"strconv"
	"time"
)

// timerLongestMillis is the longest delay a Node timer accepts, the largest signed 32-bit integer.
const timerLongestMillis = 2147483647

// recoveryDefault is the proxy recovery cadence used when the configured interval does not fit a timer.
const recoveryDefault = 5 * time.Second

// timerDelay converts ms as a Node timer does: a value below 1 or above the longest delay, or not a number, gives 1 ms,
// and any other value loses its fraction.
func timerDelay(ms float64) time.Duration {
	// The negated range test also catches NaN, and it runs before the conversion to an integer, which is undefined in
	// Go for NaN and for values out of range.
	if !(ms >= 1 && ms <= timerLongestMillis) {
		return time.Millisecond
	}
	return time.Duration(math.Trunc(ms)) * time.Millisecond
}

// drainTimer converts the drain timeout in milliseconds to the restart's deadline as the source's timer converts it.
func drainTimer(ms float64) time.Duration {
	return timerDelay(ms)
}

// drainPostBudget converts the drain timeout to the bound of the drain request, and reports false when no request is sent.
func drainPostBudget(ms float64) (time.Duration, bool) {
	p := ms + 1000
	switch {
	case math.IsNaN(p) || math.IsInf(p, 0) || p < 0:
		// The source's socket timeout rejects a negative or non-finite value by throwing before the request is
		// written, and the restart ignores the error, so no request is sent.
		return 0, false
	case p == 0:
		// The source treats a zero socket timeout as no timeout at all; the port keeps the request bounded.
		return time.Second, true
	case p > timerLongestMillis:
		// The source's socket timeout caps the value at the longest timer delay.
		return timerLongestMillis * time.Millisecond, true
	default:
		// Below 1 the timer itself raises the delay to 1 ms.
		return max(time.Duration(math.Trunc(p))*time.Millisecond, time.Millisecond), true
	}
}

// drainSeconds is the drain timeout in whole seconds as the source prints it in the timeout diagnostic.
func drainSeconds(ms float64) string {
	s := math.Ceil(ms / 1000)
	switch {
	case math.IsNaN(s):
		return "NaN"
	case math.IsInf(s, 1):
		return "Infinity"
	case math.IsInf(s, -1):
		return "-Infinity"
	case s == 0:
		// JavaScript prints negative zero as "0".
		return "0"
	case math.Abs(s) < 1e21:
		// JavaScript prints the shortest round-trip digits in plain decimal below 1e21.
		return strconv.FormatFloat(s, 'f', -1, 64)
	default:
		// From 1e21 JavaScript switches to exponent form; the exponent has at least two digits there, so Go's
		// padding of a one-digit exponent never shows.
		return strconv.FormatFloat(s, 'e', -1, 64)
	}
}

// recoveryInterval converts the proxy recovery interval in milliseconds, and reports whether a too-large value was replaced by the default.
func recoveryInterval(ms float64) (d time.Duration, replaced bool) {
	if ms > timerLongestMillis {
		// The source's interval timer would tick every millisecond here; the port keeps the normal cadence instead.
		return recoveryDefault, true
	}
	return timerDelay(ms), false
}
