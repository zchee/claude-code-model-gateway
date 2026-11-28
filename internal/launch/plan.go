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

package launch

import (
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/shimstate"
)

// StartRequest is what a command asks of one start.
type StartRequest struct {
	// Operation names the command that starts: start, recover, ensure or setup. It prefixes the start's lifecycle
	// events. An empty Operation writes no recovery events, names a refusal's unknown-owner event start, and names the
	// events of the stop of a found supervisor restart.
	Operation string
	// Quiet suppresses the start's report lines and selects the short readiness limit.
	Quiet bool
	// ResumeIntent lets the start proceed over the stop marker and removes the marker after a launch.
	ResumeIntent bool
	// PreserveRunning keeps a starting or current supervisor and waits for its readiness instead of replacing it.
	PreserveRunning bool
}

// Action is what a start does with the reading it settled on.
type Action int

// The actions of a start.
const (
	// Refuse ends the start with Plan.Reason; nothing is stopped or launched.
	Refuse Action = iota
	// KeepRunning keeps the running supervisor.
	KeepRunning
	// WaitForRunning waits for the starting supervisor without launching another.
	WaitForRunning
	// Replace launches a supervisor, after stopping the one found when Plan.StopFirst is set.
	Replace
)

// Plan is a start's decision for one settled reading.
type Plan struct {
	// Action is what the start does.
	Action Action
	// Reason is the refusal text of Refuse, without the "model-gateway: " prefix; it is empty for every other action.
	Reason string
	// OwnerUnknownEvent is the lifecycle event a refusal of an unestablished holder records, or empty.
	OwnerUnknownEvent string
	// ReapOrphans asks KeepRunning to sweep orphaned workers beside the kept supervisor.
	ReapOrphans bool
	// AwaitReadiness asks KeepRunning to wait for the kept supervisor's readiness before it reports.
	AwaitReadiness bool
	// StopFirst asks Replace to stop the supervisor it found before it launches.
	StopFirst bool
}

// Admit returns the refusal of a start that may not read the shim at all, or the empty string when it may.
func Admit(req StartRequest, stopped, proxyInstalled bool) string {
	panic("not implemented")
}

// Choose decides what a start does with the settled reading s of the public port, for this CLI's installed version.
func Choose(req StartRequest, s shimstate.Shim, installed string, port int) Plan {
	panic("not implemented")
}

// StartupLimit returns how long a start waits for readiness: 12 s when quiet, otherwise the larger of 12 s and the
// drain timeout in milliseconds plus 12 s, saturated at the largest duration.
func StartupLimit(quiet bool, drainTimeoutMS float64) time.Duration {
	panic("not implemented")
}

// StaleSessionNotice returns the notice for a session whose installed version is older than the serving supervisor's,
// or the empty string.
func StaleSessionNotice(installed string, h *shimstate.Health) string {
	panic("not implemented")
}

// Outcome is what one start reports to its command.
type Outcome struct {
	// OK reports that the start ended with a ready or kept supervisor.
	OK bool
	// Reason is the raw refusal or failure message when OK is false; it may already have a "model-gateway: " prefix.
	Reason string
	// Started lists what the start launched: "shim" after a launch, nothing otherwise.
	Started []string
	// StillStarting reports a wait that ended at its limit while the waited-for supervisor was alive.
	StillStarting bool
	// SupervisorPID is the waited-for supervisor of a StillStarting outcome, or zero.
	SupervisorPID int
	// WaitCutShort reports a wait that ended at its limit in a quiet start.
	WaitCutShort bool
	// RecoveryAttempted reports that the start, for a nonempty operation, began a recovery: the stop of the supervisor
	// it found or a launch. The start sets it; Finish leaves it false.
	RecoveryAttempted bool
}

// Finish maps the readiness result of the wait for the supervisor pid to the start's outcome; limit is the wait's
// limit and logs is the logs directory named by an unhealthy outcome's text.
func Finish(r ReadyResult, started []string, pid int, limit time.Duration, logs string, quiet bool) Outcome {
	panic("not implemented")
}

// RecoveryOutcome returns the outcome field of the start's recovery-finished event: ready, starting or failed.
func (o Outcome) RecoveryOutcome() string {
	panic("not implemented")
}
