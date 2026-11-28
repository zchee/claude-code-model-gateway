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

import (
	json "encoding/json/v2"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

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
	rec := r.Supervisor
	if !validPath(r.Root) || rec.Validate(r.Root) != nil || rec.Port == 0 {
		return false
	}
	if _, ok := parseVersion(r.Version); !ok {
		return false
	}
	if rec.Role != "supervisor" || rec.StartToken == "" {
		return false
	}
	if row, ok := r.row(rec.PID); !ok || !showsRecord(rec, row) {
		return false
	}
	return r.Fresh.PID == rec.PID && r.Fresh.Err == nil && showsRecord(rec, r.Fresh.Identity)
}

// Decide judges one birth found in r.Root's birth directory from the samples taken so far.
//
// A verdict that needs a sample not yet taken is OrphanPending; the caller then takes the next sample in field order
// and calls Decide again with every sample taken so far. A verdict other than OrphanPending never changes when later
// samples are added.
func (r OrphanRule) Decide(b Birth, s OrphanSamples) OrphanVerdict {
	if !r.Protected() || !r.validBirth(b) {
		return OrphanKeep
	}
	// A birth launched from another installation's executable is not this sweep's to judge.
	if !slices.Contains(r.Executables, b.Image.Path) {
		return OrphanKeep
	}
	// Only a birth of an older version is reaped; an invalid text on either side is never older.
	birthVersion, birthValid := parseVersion(b.Version)
	sweepVersion, sweepValid := parseVersion(r.Version)
	if !birthValid || !sweepValid || !birthVersion.olderThan(sweepVersion) {
		return OrphanKeep
	}

	if s.Worker.PID == 0 {
		return OrphanPending
	}
	if Ended(b, s.Worker) {
		return OrphanRetire
	}
	// A worker that the sample and the table do not both show, or that the table places under the protected
	// supervisor or under no proven root, is kept before its parent is sampled.
	if !r.matching(b, s.Worker) || !r.rootedOrphan(b.PID) {
		return OrphanKeep
	}

	if s.Parent.PID == 0 {
		return OrphanPending
	}
	if !lifetimeOver(b.Parent.PID, b.Parent.StartToken, s.Parent) {
		return OrphanKeep
	}

	if s.Supervisor.PID == 0 {
		return OrphanPending
	}
	if s.Supervisor.PID != r.Supervisor.PID || s.Supervisor.Err != nil || !showsRecord(r.Supervisor, s.Supervisor.Identity) {
		return OrphanKeep
	}

	if s.Recheck.PID == 0 {
		return OrphanPending
	}
	// The recheck is never proof of exit: a worker it no longer shows is kept, not retired.
	if !r.matching(b, s.Recheck) {
		return OrphanKeep
	}
	return OrphanTerminate
}

// Ended reports whether s proves that the lifetime of b's worker is over.
//
// It is true only for a sample of b's own process number, for a b with a process number in range and a start token,
// when the probe confirmed absence or the sampled process carries a start token other than b's.
func Ended(b Birth, s Sample) bool {
	return lifetimeOver(b.PID, b.StartToken, s)
}

// lifetimeOver reports whether s proves that the lifetime of process number pid with start token token is over: s
// was requested for pid, pid is in range, token is not empty, and the probe confirmed absence or sampled pid with
// another, non-empty start token.
func lifetimeOver(pid int, token procinfo.StartToken, s Sample) bool {
	if s.PID != pid || pid < 1 || pid > math.MaxInt32 || token == "" {
		return false
	}
	if errors.Is(s.Err, os.ErrNotExist) {
		return true
	}
	return s.Err == nil && s.Identity.PID == pid && s.Identity.StartToken != "" && s.Identity.StartToken != token
}

// row returns the table row of process number p, which exists only when the table holds p and the row's own process
// number is p.
func (r OrphanRule) row(p int) (procinfo.Identity, bool) {
	id, ok := r.Table[p]
	return id, ok && id.PID == p
}

// matching reports whether s is a successful sample of b's worker that shows b's lifetime and command line, and the
// table row of b's process number carries b's start token and the parent process number s shows.
func (r OrphanRule) matching(b Birth, s Sample) bool {
	if s.PID != b.PID || s.Err != nil || !liveBirth(b, s.Identity) {
		return false
	}
	row, ok := r.row(b.PID)
	return ok && row.StartToken == b.StartToken && s.Identity.ParentPID == row.ParentPID
}

// rootedOrphan reports whether the ancestry of process number pid in the table ends at a proven root without passing
// through the protected supervisor, a process number missing from the table, or a cycle.
func (r OrphanRule) rootedOrphan(pid int) bool {
	// Every step adds a process number with a row of the table, so the walk ends after at most len(r.Table) steps.
	visited := make(map[int]struct{})
	for p := pid; p != 0; {
		row, ok := r.row(p)
		if !ok || p == r.Supervisor.PID {
			return false
		}
		if _, seen := visited[p]; seen {
			return false
		}
		visited[p] = struct{}{}
		p = row.ParentPID
	}
	return true
}

// validBirth reports whether b is a complete, consistent, storable birth record of r.Root: schema version 1; a
// worker process number in range with a start token; a command line of the image path and "serve-worker"; r.Root,
// clean and absolute, as its state root; a generation; a valid version; an absolute, clean image path without a NUL
// byte and a nonzero inode; a parent process number in range other than the worker's, with a start token and a
// command line whose first element is not empty; legacy start texts for the worker and the parent; and an
// encoding/json/v2 encoding with no options that succeeds and has at most 65,536 bytes.
func (r OrphanRule) validBirth(b Birth) bool {
	const maxEncoding = 65_536
	if b.SchemaVersion != 1 || b.PID < 1 || b.PID > math.MaxInt32 || b.StartToken == "" {
		return false
	}
	if len(b.Argv) < 2 || b.Argv[0] != b.Image.Path || b.Argv[1] != "serve-worker" {
		return false
	}
	if !validPath(r.Root) || b.StateRoot != r.Root || b.Generation == "" {
		return false
	}
	if _, ok := parseVersion(b.Version); !ok {
		return false
	}
	if !validPath(b.Image.Path) || b.Image.Inode == 0 {
		return false
	}
	parent := b.Parent
	if parent.PID < 1 || parent.PID > math.MaxInt32 || parent.PID == b.PID || parent.StartToken == "" || len(parent.Argv) < 1 || parent.Argv[0] == "" {
		return false
	}
	if !isLegacyStartText(b.StartedAt) || !isLegacyStartText(parent.StartedAt) {
		return false
	}
	encoded, err := json.Marshal(b)
	return err == nil && len(encoded) <= maxEncoding
}

// isLegacyStartText reports whether text parses in the form procinfo.LegacyStart writes and formats back to itself.
func isLegacyStartText(text string) bool {
	t, err := time.Parse(time.ANSIC, text)
	return err == nil && procinfo.LegacyStart(t) == text
}

// showsRecord reports whether id shows the lifetime, command line and supervisor role of rec: the same process
// number, rec's non-empty start token, a nonzero birth whose legacy start text is rec's, a command line that joins to
// rec's command with "serve-shim" as its role argument, and no direct image of another device or inode.
func showsRecord(rec procinfo.Record, id procinfo.Identity) bool {
	return id.PID == rec.PID &&
		rec.StartToken != "" && id.StartToken == rec.StartToken &&
		!id.Birth.IsZero() && procinfo.LegacyStart(id.Birth) == rec.StartedAt &&
		strings.Join(id.Argv, " ") == rec.Command && len(id.Argv) > 1 && id.Argv[1] == "serve-shim" &&
		!contradictsImage(id.Image, rec.Image)
}

// liveBirth reports whether id shows the lifetime and command line of b's worker: the same process number, b's
// non-empty start token, a nonzero birth whose legacy start text is b's, the same command line element by element,
// and no direct image of another device or inode.
func liveBirth(b Birth, id procinfo.Identity) bool {
	return id.PID == b.PID &&
		id.StartToken != "" && id.StartToken == b.StartToken &&
		!id.Birth.IsZero() && procinfo.LegacyStart(id.Birth) == b.StartedAt &&
		slices.Equal(id.Argv, b.Argv) &&
		!contradictsImage(id.Image, b.Image)
}

// contradictsImage reports whether e was read directly from the executable and names another device or inode than
// image. Correlated evidence neither proves nor contradicts sameness, so it never contradicts.
func contradictsImage(e procinfo.ImageEvidence, image procinfo.Image) bool {
	return e.DirectFromExecutable && (e.Device != image.Device || e.Inode != image.Inode)
}
