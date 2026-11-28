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

// Package workerproc holds the supervisor's decisions about its worker process over evidence the caller supplies.
package workerproc

import "time"

// CrashSchedule counts one supervisor's consecutive quick worker crashes and gives the pause before each replacement.
//
// The zero value has counted no quick crash. A CrashSchedule is not safe for concurrent use.
type CrashSchedule struct {
	// quick is the count of consecutive quick crashes. It stops growing once the pause for the next quick crash is
	// the limit, because every later pause is the limit too, so it never overflows.
	quick uint64
}

// Exit records one observed worker exit and returns the pause before that worker's replacement may start.
//
// uptime runs from the return of the process start to the return of the wait for its exit. intentional reports that
// the supervisor recorded, for this generation and before the exit was observed, a drain, a termination or a
// disconnect after a failed drain acknowledgment that it caused itself.
//
// An exit after an uptime of 60 s or more resets the count and returns 50 ms. An exit that is not intentional after
// an uptime below 10 s, a negative one included, is a quick crash: it grows the count, and the pause doubles from
// 50 ms with each consecutive quick crash up to 5 s. Every other exit keeps the count and returns 50 ms.
func (s *CrashSchedule) Exit(uptime time.Duration, intentional bool) time.Duration {
	const (
		base      = 50 * time.Millisecond
		limit     = 5 * time.Second
		quickExit = 10 * time.Second
		resetAt   = 60 * time.Second
	)
	switch {
	case uptime >= resetAt:
		s.quick = 0
		return base
	case intentional || uptime >= quickExit:
		return base
	}
	pause := base
	for range s.quick {
		pause = min(2*pause, limit)
	}
	if pause < limit {
		s.quick++
	}
	return pause
}
