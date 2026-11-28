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

// Package shimstate is the one reading of the shim supervisor that status, doctor and the lifecycle commands share,
// so that they cannot contradict each other.
package shimstate

import (
	"context"
	"io"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

// State names what one reading found on the public port.
type State string

// The four states of a reading.
const (
	// RunningOurs is a supervisor of this state root whose health document says it is ok.
	RunningOurs State = "running-ours"
	// RunningForeign is a holder this state root cannot claim: another install, this install's Node gateway, or a
	// holder that could not be established.
	RunningForeign State = "running-foreign"
	// Starting is a supervisor of this state root that does not answer as ok yet.
	Starting State = "starting"
	// Stopped means no supervisor of this state root was found.
	Stopped State = "stopped"
)

// Health is what a reading consults in one /healthz document.
type Health struct {
	// OK reports whether the document's ok member is truthy as JavaScript judges it.
	OK bool
	// HasServingVersion reports whether the member supervisorVersion or the member version is truthy as JavaScript
	// judges it, so that a selected value whose text is empty still counts as a selected version.
	HasServingVersion bool
	// ServingVersion is the text of the first truthy of the members supervisorVersion and version, as JavaScript's
	// String converts it, with every unpaired surrogate replaced by U+FFFD; it is empty when neither is truthy, and it
	// can be empty when one is.
	ServingVersion string
	// ProxyRecovery reports whether the member proxyRecovery is the JSON value true.
	ProxyRecovery bool
	// Document is the parsed document, for readers that consult other members.
	Document jsonx.WireValue
}

// Recorded is this state root's supervisor record as a reading used it.
type Recorded struct {
	// PID is the recorded supervisor's process number, or zero when there is no such record.
	PID int
	// StartedAt is that record's legacy start text.
	StartedAt string
}

// Evidence is everything one reading is decided on.
type Evidence struct {
	// Owner is the classified holder of the public port.
	Owner ownership.Owner
	// Health is the parsed health document, or nil when none was read.
	Health *Health
	// Recorded is this state root's supervisor record: for a starting holder of this state root the record of its
	// classification or of the one display read, and for an unowned port only when a sample showed its process alive in
	// the recorded lifetime or could not tell.
	Recorded Recorded
}

// Shim is one reading.
type Shim struct {
	// State is what the reading found.
	State State
	// PID is the holder's or the recorded supervisor's process number, or zero when there is none.
	PID int
	// HasVersion reports, for a RunningOurs reading, that the health document selected a serving version.
	HasVersion bool
	// Version is the serving version text of a RunningOurs reading; it may be empty also when HasVersion is true.
	Version string
	// Image is the install root a RunningForeign holder's command line names; it may be empty.
	Image string
	// Since is the start text of a Starting reading's supervisor record; it may be empty.
	Since string
	// Owner is the classified holder the reading was decided on.
	Owner ownership.Owner
	// Health is the health document the reading was decided on, or nil.
	Health *Health
}

// ParseHealth reads one /healthz body as JSON.parse reads it and reports false when the body is not one JSON document.
func ParseHealth(body []byte) (Health, bool) {
	panic("not implemented")
}

// Decide returns the reading the evidence shows.
func Decide(e Evidence) Shim {
	panic("not implemented")
}

// Describe returns the status line of a reading for the port its health was read on, without a line end.
func Describe(s Shim, port int) string {
	panic("not implemented")
}

// NeedsRestart reports whether a supervisor whose health is h must be replaced by the installed version.
func NeedsRestart(installed string, h *Health) bool {
	panic("not implemented")
}

// ServingCurrentVersion reports whether s is RunningOurs with a supervisor that does not need a restart.
func ServingCurrentVersion(s Shim, installed string) bool {
	panic("not implemented")
}

// FetchHealth reads the /healthz document of the loopback port and returns nil when no document was read.
func FetchHealth(ctx context.Context, port uint16) *Health {
	panic("not implemented")
}

// Reader takes readings of one state root.
type Reader struct {
	// Authority classifies the public port's holder against this state root's record "guardian" and holds the store.
	Authority ownership.Authority
	// Processes samples the recorded supervisor; nil means the native probe.
	Processes ownership.Processes
	// PublicPort is the supervisor's public port.
	PublicPort uint16
	// HealthPort is the port whose /healthz is read; the lifecycle commands set it to PublicPort.
	HealthPort uint16
	// Warn receives the lines that report a retired record; nil discards them.
	Warn io.Writer
}

// Read takes one reading: it classifies the public port's holder and reads the health document concurrently, then
// reads this state root's supervisor record where the reading needs it, and retires that record when its process is
// gone.
func (r Reader) Read(ctx context.Context) Shim {
	panic("not implemented")
}
