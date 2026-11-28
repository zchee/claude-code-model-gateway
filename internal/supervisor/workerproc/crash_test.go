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

package workerproc_test

import (
	"math"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// exit is one observed worker exit and the pause the schedule must return for it.
type exit struct {
	uptime      time.Duration
	intentional bool
	// want is the pause Exit must return for this exit.
	want time.Duration
	// times repeats the same exit, with the same expected pause, this many times in a row; zero means once.
	times int
}

// expand writes out the repetitions of exits, one entry per call to Exit.
func expand(exits []exit) []exit {
	var out []exit
	for _, e := range exits {
		for range max(e.times, 1) {
			out = append(out, exit{uptime: e.uptime, intentional: e.intentional, want: e.want})
		}
	}
	return out
}

// maxReported bounds the mismatches one sequence reports in detail; the total is always reported.
const maxReported = 5

func TestCrashBackoff(t *testing.T) {
	tests := map[string]struct {
		exits []exit
	}{
		"success: the zero value counts its first quick exit as the first quick crash": {
			exits: []exit{
				{uptime: 2 * time.Second, want: 50 * time.Millisecond},
				{uptime: 2 * time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: consecutive quick exits double the pause from 50 ms and the eighth and later return 5 s": {
			exits: []exit{
				{uptime: 0, want: 50 * time.Millisecond},
				{uptime: time.Nanosecond, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: 5 * time.Second, want: 400 * time.Millisecond},
				{uptime: 9 * time.Second, want: 800 * time.Millisecond},
				{uptime: 3 * time.Second, want: 1600 * time.Millisecond},
				{uptime: 7 * time.Second, want: 3200 * time.Millisecond},
				{uptime: 4 * time.Second, want: 5 * time.Second},
				{uptime: 1 * time.Second, want: 5 * time.Second},
				{uptime: 6 * time.Second, want: 5 * time.Second},
				{uptime: 2 * time.Second, want: 5 * time.Second, times: 5},
			},
		},
		"success: an ordinary exit at exactly 10 s returns 50 ms and keeps the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 10 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
			},
		},
		"success: an ordinary exit one nanosecond below 10 s is a quick crash": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 10*time.Second - time.Nanosecond, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
			},
		},
		"success: from the zero value an ordinary exit at exactly 10 s counts nothing": {
			exits: []exit{
				{uptime: 10 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: from the zero value an ordinary exit one nanosecond below 10 s is the first quick crash": {
			exits: []exit{
				{uptime: 10*time.Second - time.Nanosecond, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: an ordinary exit at exactly 60 s returns 50 ms and resets the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: 60 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: an ordinary exit one nanosecond below 60 s returns 50 ms and keeps the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: 60*time.Second - time.Nanosecond, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
			},
		},
		"success: an ordinary exit after the longest uptime resets the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Duration(math.MaxInt64), want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
			},
		},
		"success: from the zero value an ordinary exit at exactly 60 s returns 50 ms": {
			exits: []exit{
				{uptime: 60 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
			},
		},
		"success: an ordinary exit with a negative uptime is a quick crash": {
			exits: []exit{
				{uptime: -time.Nanosecond, want: 50 * time.Millisecond},
				{uptime: -time.Hour, want: 100 * time.Millisecond},
				{uptime: time.Duration(math.MinInt64), want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
			},
		},
		"success: an ordinary exit between 10 s and 60 s returns 50 ms between quick crashes and keeps their count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 30 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
			},
		},
		"success: repeated ordinary exits between 10 s and 60 s never grow the count": {
			exits: []exit{
				{uptime: 10 * time.Second, want: 50 * time.Millisecond},
				{uptime: 11 * time.Second, want: 50 * time.Millisecond},
				{uptime: 45 * time.Second, want: 50 * time.Millisecond},
				{uptime: 60*time.Second - time.Nanosecond, want: 50 * time.Millisecond},
				{uptime: 20 * time.Second, want: 50 * time.Millisecond, times: 10},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: an intentional quick exit returns 50 ms between quick crashes and keeps their count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: 2 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: intentional exits below 10 s never grow the count": {
			exits: []exit{
				{uptime: 0, intentional: true, want: 50 * time.Millisecond},
				{uptime: -time.Nanosecond, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Duration(math.MinInt64), intentional: true, want: 50 * time.Millisecond},
				{uptime: 10*time.Second - time.Nanosecond, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, intentional: true, want: 50 * time.Millisecond, times: 10},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: an intentional exit between 10 s and 60 s returns 50 ms and keeps the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 10 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: 30 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
			},
		},
		"success: an intentional exit one nanosecond below 60 s returns 50 ms and keeps the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 60*time.Second - time.Nanosecond, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
			},
		},
		"success: an intentional exit at exactly 60 s returns 50 ms and resets the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: 60 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: an intentional exit above 60 s returns 50 ms and resets the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: 2 * time.Hour, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
			},
		},
		"success: quick crashes after a reset at the cap double again from 50 ms": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
				{uptime: time.Second, want: 1600 * time.Millisecond},
				{uptime: time.Second, want: 3200 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second, times: 2},
				{uptime: 90 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
				{uptime: time.Second, want: 1600 * time.Millisecond},
				{uptime: time.Second, want: 3200 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second},
			},
		},
		"success: after ten thousand quick crashes the pause is still 5 s and one exit at 60 s resets the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
				{uptime: time.Second, want: 1600 * time.Millisecond},
				{uptime: time.Second, want: 3200 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second, times: 9993},
				{uptime: 60 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
			},
		},
		"success: after ten thousand quick crashes an ordinary exit between 10 s and 60 s keeps the count": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
				{uptime: time.Second, want: 1600 * time.Millisecond},
				{uptime: time.Second, want: 3200 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second, times: 9993},
				{uptime: 30 * time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second},
			},
		},
		"success: after ten thousand quick crashes an intentional quick exit keeps the count and an intentional exit at 60 s resets it": {
			exits: []exit{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
				{uptime: time.Second, want: 1600 * time.Millisecond},
				{uptime: time.Second, want: 3200 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second, times: 9993},
				{uptime: time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 5 * time.Second},
				{uptime: 60 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s workerproc.CrashSchedule
			exits := expand(tt.exits)
			mismatches := 0
			for i, e := range exits {
				got := s.Exit(e.uptime, e.intentional)
				diff := gocmp.Diff(e.want, got)
				if diff == "" {
					continue
				}
				mismatches++
				if mismatches <= maxReported {
					t.Errorf("exit %d of %d (uptime %v, intentional %t): pause mismatch (-want +got):\n%s", i+1, len(exits), e.uptime, e.intentional, diff)
				}
			}
			if mismatches > 0 {
				t.Errorf("%d of %d exits returned the wrong pause; the first %d are shown above", mismatches, len(exits), min(mismatches, maxReported))
			}
		})
	}
}

func TestCrashSchedulesKeepSeparateCounts(t *testing.T) {
	// step is one exit observed by one of two schedules.
	type step struct {
		second      bool
		uptime      time.Duration
		intentional bool
		want        time.Duration
	}
	tests := map[string]struct {
		steps []step
	}{
		"success: a second schedule starts from no quick crash while the first has counted three": {
			steps: []step{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 200 * time.Millisecond},
				{second: true, uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 400 * time.Millisecond},
				{second: true, uptime: time.Second, want: 100 * time.Millisecond},
				{uptime: time.Second, want: 800 * time.Millisecond},
			},
		},
		"success: a reset of one schedule leaves the other's count": {
			steps: []step{
				{uptime: time.Second, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{second: true, uptime: time.Second, want: 50 * time.Millisecond},
				{second: true, uptime: time.Second, want: 100 * time.Millisecond},
				{second: true, uptime: time.Second, want: 200 * time.Millisecond},
				{uptime: 60 * time.Second, want: 50 * time.Millisecond},
				{second: true, uptime: time.Second, want: 400 * time.Millisecond},
				{uptime: time.Second, want: 50 * time.Millisecond},
				{second: true, uptime: 60 * time.Second, intentional: true, want: 50 * time.Millisecond},
				{uptime: time.Second, want: 100 * time.Millisecond},
				{second: true, uptime: time.Second, want: 50 * time.Millisecond},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var first workerproc.CrashSchedule
			second := new(workerproc.CrashSchedule)
			for i, st := range tt.steps {
				s, which := &first, "first"
				if st.second {
					s, which = second, "second"
				}
				got := s.Exit(st.uptime, st.intentional)
				if diff := gocmp.Diff(st.want, got); diff != "" {
					t.Errorf("step %d, %s schedule (uptime %v, intentional %t): pause mismatch (-want +got):\n%s", i+1, which, st.uptime, st.intentional, diff)
				}
			}
		})
	}
}
