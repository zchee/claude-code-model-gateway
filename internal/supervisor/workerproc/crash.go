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
	// quick is the count of consecutive quick crashes.
	quick uint64 //nolint:unused // Unused only until Exit is written: Exit reads and updates it.
}

// Exit records one observed worker exit and returns the pause before that worker's replacement may start.
//
// uptime runs from the return of the process start to the return of the wait for its exit. intentional reports that
// the supervisor recorded, for this generation and before the exit, a drain or a termination it caused itself.
func (s *CrashSchedule) Exit(uptime time.Duration, intentional bool) time.Duration {
	panic("not implemented")
}
