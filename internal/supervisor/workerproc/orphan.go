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

package workerproc

import "github.com/zchee/claude-code-model-gateway/internal/procinfo"

// Sample is one identity sample of one requested process number together with the error the probe returned.
type Sample struct {
	// PID is the process number the sample was requested for; zero means the sample was not taken.
	PID int
	// Identity is the sampled identity; it is meaningful only when Err is nil.
	Identity procinfo.Identity
	// Err is the probe's error: os.ErrNotExist for confirmed absence, another error for an inconclusive sample.
	Err error
}

// OrphanRule is what every orphan decision of one sweep is judged against.
type OrphanRule struct {
	// Root is the clean absolute state root whose birth directory is swept.
	Root string
	// Version is the sweeping gateway's version; only births of an older version are reaped.
	Version string
	// Executables are the resolved paths this installation launches workers from.
	Executables []string
	// Supervisor is the verified record of the running supervisor that the sweep protects.
	Supervisor procinfo.Record
	// Table is one process table sample taken at the start of the sweep.
	Table map[int]procinfo.Identity
	// Fresh is a sample of the supervisor's process number taken after Table.
	Fresh Sample
}

// OrphanSamples are the samples one birth is judged by, taken in field order, each after the one before it.
type OrphanSamples struct {
	// Worker is the first sample of the birth's worker.
	Worker Sample
	// Parent is a sample of the birth's recorded parent.
	Parent Sample
	// Supervisor is a second sample of the protected supervisor.
	Supervisor Sample
	// Recheck is the second sample of the worker, taken after the signal handle was opened.
	Recheck Sample
}

// OrphanVerdict is what an orphan decision allows for one birth.
type OrphanVerdict int

const (
	// OrphanKeep leaves the birth record in place and signals nothing.
	OrphanKeep OrphanVerdict = iota
	// OrphanRetire removes the birth record without a signal: its worker's lifetime is over.
	OrphanRetire
	// OrphanTerminate allows one SIGTERM to the process the open handle names.
	OrphanTerminate
	// OrphanPending means the next sample in field order is needed before a verdict.
	OrphanPending
)

// Protected reports whether the sweep may judge any birth: the table and a fresh sample show the recorded supervisor's lifetime.
//
// It requires a clean absolute Root, a structurally complete supervisor record of that root with a nonzero port, the
// supervisor role and a start token, a valid sweeping version, and both the table row and the fresh sample of the
// supervisor's process number showing that record's lifetime, command line and role.
func (r OrphanRule) Protected() bool {
	panic("not implemented")
}

// Decide judges one birth found in r.Root's birth directory from the samples taken so far.
//
// A verdict that needs a sample not yet taken is OrphanPending; the caller then takes the next sample in field order
// and calls Decide again with every sample taken so far. A verdict other than OrphanPending never changes when later
// samples are added.
func (r OrphanRule) Decide(b Birth, s OrphanSamples) OrphanVerdict {
	panic("not implemented")
}

// Ended reports whether s proves that the lifetime of b's worker is over.
//
// It is true only for a sample of b's own process number, for a b with a process number in range and a start token,
// when the probe confirmed absence or the sampled process carries a start token other than b's.
func Ended(b Birth, s Sample) bool {
	panic("not implemented")
}
